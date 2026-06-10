// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dolt

// Secondary-index maintenance for the 3-way collection merge (Phase 3,
// workspace-nth, behaviors B2-B6 of
// docs/design/secondary-index-structural-sharing.md).
//
// The primary-map merge walks a tree.ThreeWayDiffer and resolves
// documents (including field-level merging that can produce documents
// existing on neither parent). Index maintenance rides that same diff
// stream: each surviving index is seeded from one parent's persisted
// map -- wholesale chunk reuse with that parent -- and the other
// side's per-document edits (plus all resolutions) are applied through
// indexEntriesForDoc. Merging the index maps directly with a 3-way
// map merge would be wrong: resolved documents' entries exist in
// neither parent's index.

import (
	"context"
	"fmt"
	"sort"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
)

// indexSetFromAM resolves every entry of a secondary-index AddressMap
// into name -> resolvedIndexEntry.
func indexSetFromAM(ctx context.Context, state *dbState, am prolly.AddressMap) (map[string]*resolvedIndexEntry, error) {
	out := make(map[string]*resolvedIndexEntry)
	if err := am.IterAll(ctx, func(name string, entryHash hash.Hash) error {
		if entryHash.IsEmpty() {
			return nil
		}
		resolved, err := resolveIndexEntry(ctx, state.ns, entryHash)
		if err != nil {
			return err
		}
		out[name] = resolved
		return nil
	}); err != nil {
		return nil, fmt.Errorf("indexSetFromAM: %w", err)
	}
	return out, nil
}

// indexSpecEqual compares index DEFINITIONS: name, key fields, unique,
// sparse, and the partial filter expression. Content (map root) and
// the sticky Lossy/Multikey flags never count as definition changes.
func indexSpecEqual(a, b backends.IndexInfo) bool {
	if a.Name != b.Name || a.Unique != b.Unique || a.Sparse != b.Sparse {
		return false
	}
	if len(a.Key) != len(b.Key) {
		return false
	}
	for i := range a.Key {
		if a.Key[i] != b.Key[i] {
			return false
		}
	}
	aEntry, aErr := indexInfoToEntry(a, hash.Hash{})
	bEntry, bErr := indexInfoToEntry(b, hash.Hash{})
	if aErr != nil || bErr != nil {
		return false
	}
	return aEntry.PartialBSONHex == bEntry.PartialBSONHex
}

// pendingOwner tracks an entry inserted during the merge, for unique
// bookkeeping: which primary ID claimed the value, and whether the
// claiming document is an "ours" (into-side or resolved) document.
type pendingOwner struct {
	id   []byte
	ours bool
}

// indexMergeSurvivor is one index that survives the name-level
// reconciliation, with its seeded mutable map and unique bookkeeping.
type indexMergeSurvivor struct {
	info     backends.IndexInfo
	seedMap  prolly.Map
	mut      *prolly.MutableMap
	seedLeft bool // true: seeded from INTO's map; false: from FROM's

	// Unique bookkeeping (nil maps when !info.Unique).
	pending map[string]pendingOwner // value prefix -> claiming owner (this merge)
	removed map[string]struct{}     // full entry key removed this merge
}

// reconcileIndexSets implements the design doc's B5 case table over
// the three sides' index sets. Returns the surviving indexes with
// their seed side chosen, or an error for competing redefinitions.
//
// Survival rules (spec compared against base):
//   - present on both sides, specs equal: survives (content merge).
//   - present on both, specs differ: the side whose spec still equals
//     base's is unaltered and loses to the other side's redefinition;
//     if neither matches base, error.
//   - present on one side only: survives if new since base (creation);
//     dropped silently if base had it and that side's spec is
//     unaltered relative to base (drop wins); error if the surviving
//     side also redefined it (competing definition changes).
func reconcileIndexSets(intoSet, fromSet, baseSet map[string]*resolvedIndexEntry) (survivors []*indexMergeSurvivor, seeds map[string]struct {
	entry    *resolvedIndexEntry
	seedLeft bool
}, err error) {
	names := make(map[string]struct{})
	for n := range intoSet {
		names[n] = struct{}{}
	}
	for n := range fromSet {
		names[n] = struct{}{}
	}
	for n := range baseSet {
		names[n] = struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for n := range names {
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)

	seeds = make(map[string]struct {
		entry    *resolvedIndexEntry
		seedLeft bool
	})

	for _, name := range ordered {
		into, hasInto := intoSet[name]
		from, hasFrom := fromSet[name]
		base, hasBase := baseSet[name]

		switch {
		case hasInto && hasFrom:
			if indexSpecEqual(into.info, from.info) {
				// Ordinary content merge. Seed from FROM, mirroring the
				// primary merge's seed side.
				seeds[name] = struct {
					entry    *resolvedIndexEntry
					seedLeft bool
				}{from, false}
				continue
			}
			intoAltered := !hasBase || !indexSpecEqual(into.info, base.info)
			fromAltered := !hasBase || !indexSpecEqual(from.info, base.info)
			switch {
			case intoAltered && !fromAltered:
				// Into redefined; from left it untouched. Into's wins;
				// it must cover from's docs (seeded from INTO, right
				// edits applied).
				seeds[name] = struct {
					entry    *resolvedIndexEntry
					seedLeft bool
				}{into, true}
			case fromAltered && !intoAltered:
				seeds[name] = struct {
					entry    *resolvedIndexEntry
					seedLeft bool
				}{from, false}
			default:
				return nil, nil, fmt.Errorf("index %q was redefined with different specs on both branches; drop or align one side before merging", name)
			}

		case hasInto:
			if !hasBase {
				// Created on into since base; covers into's docs, needs
				// from's edits.
				seeds[name] = struct {
					entry    *resolvedIndexEntry
					seedLeft bool
				}{into, true}
				continue
			}
			// From dropped it. Drop wins over an unaltered keeper;
			// competing definition changes error.
			if indexSpecEqual(into.info, base.info) {
				continue // dropped
			}
			return nil, nil, fmt.Errorf("index %q was dropped on one branch and redefined on the other; resolve the definitions before merging", name)

		case hasFrom:
			if !hasBase {
				seeds[name] = struct {
					entry    *resolvedIndexEntry
					seedLeft bool
				}{from, false}
				continue
			}
			if indexSpecEqual(from.info, base.info) {
				continue // dropped by into
			}
			return nil, nil, fmt.Errorf("index %q was dropped on one branch and redefined on the other; resolve the definitions before merging", name)

		default:
			// Base only: dropped on both sides.
		}
	}
	return nil, seeds, nil
}

// openSurvivors opens each seed's prolly.Map and wraps it in a
// mutable survivor.
func openSurvivors(ctx context.Context, state *dbState, seeds map[string]struct {
	entry    *resolvedIndexEntry
	seedLeft bool
}) ([]*indexMergeSurvivor, error) {
	names := make([]string, 0, len(seeds))
	for n := range seeds {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]*indexMergeSurvivor, 0, len(seeds))
	for _, name := range names {
		seed := seeds[name]
		m, err := openIndexMap(ctx, state.vs, state.ns, seed.entry.mapRoot)
		if err != nil {
			return nil, fmt.Errorf("openSurvivors: %q: %w", name, err)
		}
		s := &indexMergeSurvivor{
			info:     seed.entry.info,
			seedMap:  m,
			mut:      m.Mutate(),
			seedLeft: seed.seedLeft,
		}
		if s.info.Unique {
			s.pending = make(map[string]pendingOwner)
			s.removed = make(map[string]struct{})
		}
		out = append(out, s)
	}
	return out, nil
}

// deleteDoc removes every entry doc contributed to the survivor.
func (s *indexMergeSurvivor) deleteDoc(ctx context.Context, doc *types.Document, idBytes []byte) error {
	if doc == nil {
		return nil
	}
	rows, _, _ := indexEntriesForDoc(doc, s.info)
	for _, row := range rows {
		if err := idxpkg.DeleteEntry(ctx, s.mut, row, idBytes); err != nil {
			return err
		}
		if s.info.Unique {
			full := string(idxpkg.BuildSecondaryKey(row, idBytes))
			s.removed[full] = struct{}{}
			prefix, _ := idxpkg.EqualityProbeBounds(row)
			if owner, ok := s.pending[string(prefix)]; ok && string(owner.id) == string(idBytes) {
				delete(s.pending, string(prefix))
			}
		}
	}
	return nil
}

// insertDoc adds every entry doc contributes. The caller has already
// run uniqueCollision when it matters.
func (s *indexMergeSurvivor) insertDoc(ctx context.Context, doc *types.Document, idBytes []byte, ours bool) error {
	if doc == nil {
		return nil
	}
	rows, multikey, lossy := indexEntriesForDoc(doc, s.info)
	s.info.Multikey = s.info.Multikey || multikey
	s.info.Lossy = s.info.Lossy || lossy
	for _, row := range rows {
		if err := idxpkg.InsertEntry(ctx, s.mut, row, idBytes); err != nil {
			return err
		}
		if s.info.Unique {
			prefix, _ := idxpkg.EqualityProbeBounds(row)
			s.pending[string(prefix)] = pendingOwner{id: idBytes, ours: ours}
		}
	}
	return nil
}

// uniqueCollision reports whether inserting doc would collide with a
// DIFFERENT document's live entry: an entry pending from this merge,
// or a seed entry that has not been removed during this merge. Returns
// the owning primary ID and whether that owner is an "ours" document.
func (s *indexMergeSurvivor) uniqueCollision(ctx context.Context, doc *types.Document, idBytes []byte) (ownerID []byte, ownerOurs, collision bool, err error) {
	if !s.info.Unique || doc == nil {
		return nil, false, false, nil
	}
	rows, _, lossy := indexEntriesForDoc(doc, s.info)
	if lossy || s.info.Lossy {
		// No faithful encoding: probes would collide on collapsed
		// bytes. Skip merge-time enforcement for this index; write-path
		// validation still applies after the merge.
		return nil, false, false, nil
	}
	for _, row := range rows {
		prefix, _ := idxpkg.EqualityProbeBounds(row)
		if owner, ok := s.pending[string(prefix)]; ok {
			if string(owner.id) != string(idBytes) {
				return owner.id, owner.ours, true, nil
			}
			continue
		}
		start, stop := idxpkg.EqualityProbeBounds(row)
		ids, _, lerr := idxpkg.RangeLookupCapped(ctx, s.seedMap, start, stop, 4)
		if lerr != nil {
			return nil, false, false, lerr
		}
		for _, id := range ids {
			if string(id) == string(idBytes) {
				continue
			}
			full := string(idxpkg.BuildSecondaryKey(row, id))
			if _, gone := s.removed[full]; gone {
				continue
			}
			// Seed owners are from-side docs for FROM-seeded indexes
			// and into-side docs for INTO-seeded ones.
			return id, s.seedLeft, true, nil
		}
	}
	return nil, false, false, nil
}

// indexMergeApplier coordinates per-document edits across every
// survivor as the primary differ emits ops.
type indexMergeApplier struct {
	state     *dbState
	survivors []*indexMergeSurvivor
}

// mergeEditKind classifies the primary differ ops that change the
// merged map. Convergent ops never reach the applier: both sides made
// the same change, so every seed already reflects it.
type mergeEditKind int

const (
	editLeftChange  mergeEditKind = iota // into-side add/modify/delete
	editRightChange                      // from-side add/modify/delete
	editResolved                         // field-level merged document
	editKeepOurs                         // divergent conflict, ours kept
)

// mergeDocEdit carries every decoded version of the document so each
// survivor can compute its own before-image (the seed side's version).
type mergeDocEdit struct {
	kind   mergeEditKind
	base   *types.Document // nil when absent in the ancestor
	left   *types.Document // nil when into deleted / never had it
	right  *types.Document // nil when from deleted / never had it
	merged *types.Document // editResolved only
}

// perSurvivor returns the (old, new) pair this survivor must apply,
// and whether the edit touches it at all.
func (e mergeDocEdit) perSurvivor(s *indexMergeSurvivor) (old, new *types.Document, touches bool) {
	switch e.kind {
	case editLeftChange:
		if s.seedLeft {
			return nil, nil, false // already in INTO's seed
		}
		return e.base, e.left, true
	case editRightChange:
		if !s.seedLeft {
			return nil, nil, false // already in FROM's seed
		}
		return e.base, e.right, true
	case editResolved:
		if s.seedLeft {
			return e.left, e.merged, true
		}
		return e.right, e.merged, true
	case editKeepOurs:
		if s.seedLeft {
			return nil, nil, false // ours is what the seed holds
		}
		return e.right, e.left, true
	}
	return nil, nil, false
}

// incomingOurs reports whether the version being indexed belongs to
// "ours" for collision-preference purposes.
func (e mergeDocEdit) incomingOurs() bool {
	return e.kind != editRightChange
}

// mergeLoser names a document evicted by a unique-key collision. The
// caller must remove it from the merged primary map and record the
// conflict; the applier has already kept its index entries consistent.
type mergeLoser struct {
	id       []byte // primary id of the losing doc
	incoming bool   // true when the loser is this edit's own new doc
}

// apply routes one document edit to every survivor. For unique
// survivors the incoming version is collision-checked first; "ours
// wins" (into-side and resolved documents beat from-side ones; between
// same-side claims the earlier one keeps the key). A non-nil loser
// tells the caller who to evict; when loser.incoming the edit was NOT
// applied to any index.
func (a *indexMergeApplier) apply(ctx context.Context, edit mergeDocEdit, idBytes []byte) (*mergeLoser, error) {
	for _, s := range a.survivors {
		_, newDoc, touches := edit.perSurvivor(s)
		if !touches || newDoc == nil {
			continue
		}
		ownerID, ownerOurs, collision, err := s.uniqueCollision(ctx, newDoc, idBytes)
		if err != nil {
			return nil, err
		}
		if !collision {
			continue
		}
		if edit.incomingOurs() && !ownerOurs {
			return &mergeLoser{id: ownerID, incoming: false}, nil
		}
		return &mergeLoser{id: idBytes, incoming: true}, nil
	}

	for _, s := range a.survivors {
		oldDoc, newDoc, touches := edit.perSurvivor(s)
		if !touches {
			continue
		}
		if err := s.deleteDoc(ctx, oldDoc, idBytes); err != nil {
			return nil, err
		}
		if err := s.insertDoc(ctx, newDoc, idBytes, edit.incomingOurs()); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// removeDocEverywhere deletes a losing document's entries from every
// survivor (used when a unique conflict evicts a doc the seed or an
// earlier edit had indexed).
func (a *indexMergeApplier) removeDocEverywhere(ctx context.Context, doc *types.Document, idBytes []byte) error {
	for _, s := range a.survivors {
		if err := s.deleteDoc(ctx, doc, idBytes); err != nil {
			return err
		}
	}
	return nil
}

// finalize flushes every survivor and builds the merged index AM.
func (a *indexMergeApplier) finalize(ctx context.Context) (prolly.AddressMap, error) {
	infos := make([]backends.IndexInfo, 0, len(a.survivors))
	maps := make(map[string]prolly.Map, len(a.survivors))
	for _, s := range a.survivors {
		m, err := s.mut.Map(ctx)
		if err != nil {
			return prolly.AddressMap{}, fmt.Errorf("flushing merged index %q: %w", s.info.Name, err)
		}
		infos = append(infos, s.info)
		maps[s.info.Name] = m
	}
	return buildIndexAM(ctx, a.state, infos, maps)
}
