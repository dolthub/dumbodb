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

import (
	"context"
	"fmt"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dongo/internal/backends"
)

// mergeInProgress holds the state of a merge that could not be automatically
// completed due to document-level conflicts. It persists in dbState until the
// user resolves all conflicts and calls DongoCommit.
type mergeInProgress struct {
	fromBranch string
	intoBranch string
	premergeAM prolly.AddressMap // ours branch AM before the merge started (used to abort)
	fromHash   hash.Hash         // fromBranch HEAD hash at merge time (merge commit parent)
	intoHash   hash.Hash         // intoBranch HEAD hash at merge time (merge commit parent)
	// conflicts: collection name → ordered list of conflict entries.
	// Entries are never removed; resolved ones have resolved==true.
	conflicts map[string][]*conflictEntry
	// resolvedAM is the working AddressMap being built as conflicts are resolved.
	// It starts as the partial merged AM (keeping "ours" for conflicting docs) and
	// is updated as each conflict is resolved.
	resolvedAM prolly.AddressMap
}

// hasUnresolvedConflicts reports whether any conflict entry in the merge state is unresolved.
func (m *mergeInProgress) hasUnresolvedConflicts() bool {
	for _, entries := range m.conflicts {
		for _, e := range entries {
			if !e.resolved {
				return true
			}
		}
	}
	return false
}

// summaries builds a per-collection list of unresolved conflict counts.
func (m *mergeInProgress) summaries() []backends.ConflictSummary {
	var out []backends.ConflictSummary
	for name, entries := range m.conflicts {
		count := 0
		for _, e := range entries {
			if !e.resolved {
				count++
			}
		}
		if count > 0 {
			out = append(out, backends.ConflictSummary{Collection: name, Count: count})
		}
	}
	return out
}

// conflictEntry represents a single document-level conflict captured during a merge.
// The raw key and value tuples are stored as byte slices so they can be decoded lazily on demand.
type conflictEntry struct {
	id            string
	rawKey        val.Tuple // encoded _id key bytes
	baseRawVal    val.Tuple // base document value tuple (nil if document was absent in ancestor)
	oursRawVal    val.Tuple // ours document value tuple (nil if our branch deleted the document)
	theirsRawVal  val.Tuple // theirs document value tuple (nil if their branch deleted the document)
	ourDiffType   string    // "added", "modified", or "deleted"
	theirDiffType string    // "added", "modified", or "deleted"
	resolved      bool
}

// diffTypeString converts a tree.DiffType to the canonical string used in the wire protocol.
func diffTypeString(dt tree.DiffType) string {
	switch dt {
	case tree.AddedDiff:
		return "added"
	case tree.ModifiedDiff:
		return "modified"
	case tree.RemovedDiff:
		return "deleted"
	default:
		return "none"
	}
}

// DongoConflicts implements backends.VersioningBackend.
//
// When ConflictsParams.Collection is empty it returns per-collection conflict counts.
// When ConflictsParams.Collection is set it returns per-conflict details for that collection.
func (b *Backend) DongoConflicts(ctx context.Context, params *backends.ConflictsParams) (*backends.ConflictsResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoConflicts: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: DongoConflicts: database %q does not exist", params.DBName))
	}

	db.mu.RLock()
	ms := db.mergeState
	db.mu.RUnlock()

	if ms == nil {
		return nil, fmt.Errorf("dolt: DongoConflicts: no merge in progress on branch %q", params.Branch)
	}

	if params.Collection == "" {
		summaries := ms.summaries()
		if summaries == nil {
			summaries = []backends.ConflictSummary{}
		}
		return &backends.ConflictsResult{Collections: summaries}, nil
	}

	entries, ok := ms.conflicts[params.Collection]
	if !ok {
		return &backends.ConflictsResult{Conflicts: []backends.ConflictInfo{}}, nil
	}

	var infos []backends.ConflictInfo
	for _, e := range entries {
		if e.resolved {
			continue
		}

		info := backends.ConflictInfo{
			ConflictID:    e.id,
			OurDiffType:   e.ourDiffType,
			TheirDiffType: e.theirDiffType,
		}

		if e.baseRawVal != nil {
			doc, docErr := readDocFromEntry(ctx, db.ns, e.rawKey, e.baseRawVal)
			if docErr != nil {
				return nil, fmt.Errorf("dolt: DongoConflicts: reading base doc for conflict %q: %w", e.id, docErr)
			}
			info.Base = doc
		}

		if e.oursRawVal != nil {
			doc, docErr := readDocFromEntry(ctx, db.ns, e.rawKey, e.oursRawVal)
			if docErr != nil {
				return nil, fmt.Errorf("dolt: DongoConflicts: reading ours doc for conflict %q: %w", e.id, docErr)
			}
			info.Ours = doc
		}

		if e.theirsRawVal != nil {
			doc, docErr := readDocFromEntry(ctx, db.ns, e.rawKey, e.theirsRawVal)
			if docErr != nil {
				return nil, fmt.Errorf("dolt: DongoConflicts: reading theirs doc for conflict %q: %w", e.id, docErr)
			}
			info.Theirs = doc
		}

		infos = append(infos, info)
	}

	if infos == nil {
		infos = []backends.ConflictInfo{}
	}

	return &backends.ConflictsResult{Conflicts: infos}, nil
}

// DongoResolveConflict implements backends.VersioningBackend.
//
// Resolves a single document conflict in the current in-progress merge.
// After resolution the conflict entry is marked resolved and the resolvedAM is
// updated to reflect the chosen document state.
func (b *Backend) DongoResolveConflict(ctx context.Context, params *backends.ResolveConflictParams) (*backends.ResolveConflictResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoResolveConflict: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: DongoResolveConflict: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	ms := db.mergeState
	if ms == nil {
		return nil, fmt.Errorf("dolt: DongoResolveConflict: no merge in progress on branch %q", params.Branch)
	}

	entries, ok := ms.conflicts[params.Collection]
	if !ok {
		return nil, fmt.Errorf("dolt: DongoResolveConflict: collection %q has no conflicts", params.Collection)
	}

	var target *conflictEntry
	for _, e := range entries {
		if e.id == params.ConflictID {
			target = e
			break
		}
	}

	if target == nil {
		return nil, fmt.Errorf("dolt: DongoResolveConflict: conflict %q not found in collection %q", params.ConflictID, params.Collection)
	}
	if target.resolved {
		return nil, fmt.Errorf("dolt: DongoResolveConflict: conflict %q is already resolved", params.ConflictID)
	}

	// For "ours", the resolvedAM already has our value — just mark resolved, no AM update needed.
	if params.Resolution == "ours" {
		target.resolved = true
		return &backends.ResolveConflictResult{}, nil
	}

	// Determine the chosen value for "theirs" or "custom".
	var chosenVal val.Tuple
	var deleteDoc bool

	switch params.Resolution {
	case "theirs":
		chosenVal = target.theirsRawVal
		deleteDoc = target.theirsRawVal == nil

	case "custom":
		if params.Value == nil {
			return nil, fmt.Errorf("dolt: DongoResolveConflict: resolution %q requires a value document", params.Resolution)
		}
		jsonHash, writeErr := writeDocJSON(ctx, db.ns, params.Value)
		if writeErr != nil {
			return nil, fmt.Errorf("dolt: DongoResolveConflict: writing custom document: %w", writeErr)
		}
		v, buildErr := buildValue(jsonHash)
		if buildErr != nil {
			return nil, fmt.Errorf("dolt: DongoResolveConflict: building value tuple: %w", buildErr)
		}
		chosenVal = v

	default:
		return nil, fmt.Errorf("dolt: DongoResolveConflict: unknown resolution %q (must be 'ours', 'theirs', or 'custom')", params.Resolution)
	}

	// Update the collection map in resolvedAM to reflect the chosen resolution.
	collMap, err := collectionMapFromAM(ctx, db, ms.resolvedAM, params.Collection)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoResolveConflict: opening collection %q from resolvedAM: %w", params.Collection, err)
	}

	mut := collMap.Mutate()

	if deleteDoc {
		if err := mut.Delete(ctx, target.rawKey); err != nil {
			return nil, fmt.Errorf("dolt: DongoResolveConflict: deleting document from collection %q: %w", params.Collection, err)
		}
	} else {
		if err := mut.Put(ctx, target.rawKey, chosenVal); err != nil {
			return nil, fmt.Errorf("dolt: DongoResolveConflict: updating document in collection %q: %w", params.Collection, err)
		}
	}

	newCollMap, err := mut.Map(ctx)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoResolveConflict: flushing collection map for %q: %w", params.Collection, err)
	}

	// Update the resolvedAM to point to the new collection map.
	newCollHash, err := db.dtblHashForMap(ctx, newCollMap)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoResolveConflict: getting DTBL hash for %q: %w", params.Collection, err)
	}

	amEditor := ms.resolvedAM.Editor()

	newCollCount, countErr := newCollMap.Count()
	if countErr != nil {
		return nil, fmt.Errorf("dolt: DongoResolveConflict: counting new collection map for %q: %w", params.Collection, countErr)
	}

	if newCollCount == 0 {
		if err := amEditor.Delete(ctx, params.Collection); err != nil {
			return nil, fmt.Errorf("dolt: DongoResolveConflict: deleting collection %q from AM: %w", params.Collection, err)
		}
	} else {
		if err := amEditor.Update(ctx, params.Collection, newCollHash); err != nil {
			return nil, fmt.Errorf("dolt: DongoResolveConflict: updating AM for collection %q: %w", params.Collection, err)
		}
	}

	newAM, err := amEditor.Flush(ctx)
	if err != nil {
		return nil, fmt.Errorf("dolt: DongoResolveConflict: flushing AM editor: %w", err)
	}

	ms.resolvedAM = newAM
	target.resolved = true

	return &backends.ResolveConflictResult{}, nil
}

// captureConflictsForCollection merges two collection maps at document level,
// capturing conflicts instead of erroring. Returns the partial merged map (keeping
// "ours" for conflicting documents) and the list of captured conflict entries.
func captureConflictsForCollection(
	ctx context.Context,
	intoMap, fromMap, baseMap prolly.Map,
	baseConflictIdx int,
) (mergedMap prolly.Map, entries []*conflictEntry, err error) {
	idx := baseConflictIdx

	collisionFn := func(left, right tree.Diff) (tree.Diff, bool) {
		entry := &conflictEntry{
			id:            fmt.Sprintf("c%d", idx),
			rawKey:        val.Tuple(left.Key),
			ourDiffType:   diffTypeString(left.Type),
			theirDiffType: diffTypeString(right.Type),
		}
		idx++

		// base value: From is the same for both left and right (common ancestor).
		if left.From != nil {
			entry.baseRawVal = val.Tuple(left.From)
		}
		// ours value: left.To (nil for RemovedDiff — our branch deleted the document).
		if left.To != nil {
			entry.oursRawVal = val.Tuple(left.To)
		}
		// theirs value: right.To (nil for RemovedDiff — their branch deleted the document).
		if right.To != nil {
			entry.theirsRawVal = val.Tuple(right.To)
		}

		entries = append(entries, entry)

		// Keep "ours" (left) value in the merged result.
		// For RemovedDiff (our branch deleted the doc), exclude the key from the merged map.
		if left.Type == tree.RemovedDiff {
			return tree.Diff{}, false
		}
		return left, true
	}

	mergedMap, _, err = prolly.MergeMaps(ctx, intoMap, fromMap, baseMap, collisionFn)
	if err != nil {
		return prolly.Map{}, nil, fmt.Errorf("merging collection documents: %w", err)
	}

	return mergedMap, entries, nil
}

// mergeAddressMapsWithConflicts performs a 3-way merge of collections AddressMaps,
// capturing document-level conflicts rather than erroring on them.
//
// Collection-level conflicts (entire collection deleted on one branch while modified on the other)
// are still returned as hard errors.
//
// Returns the partial merged AM (with "ours" values for conflicting documents) and a
// per-collection map of captured conflict entries. The conflicts map is non-nil but may be empty.
func mergeAddressMapsWithConflicts(ctx context.Context, state *dbState, intoAM, fromAM, baseAM prolly.AddressMap) (prolly.AddressMap, map[string][]*conflictEntry, error) {
	allNames := make(map[string]struct{})
	for _, am := range []prolly.AddressMap{intoAM, fromAM, baseAM} {
		if err := am.IterAll(ctx, func(name string, _ hash.Hash) error {
			allNames[name] = struct{}{}
			return nil
		}); err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("iterating collections AM: %w", err)
		}
	}

	editor := intoAM.Editor()
	allConflicts := make(map[string][]*conflictEntry)
	globalIdx := 0

	for name := range allNames {
		intoH, err := intoAM.Get(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("reading into AM for %q: %w", name, err)
		}
		fromH, err := fromAM.Get(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("reading from AM for %q: %w", name, err)
		}
		baseH, err := baseAM.Get(ctx, name)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("reading base AM for %q: %w", name, err)
		}

		intoChanged := intoH != baseH
		fromChanged := fromH != baseH

		if !fromChanged {
			// From did not touch this collection; keep Into's version.
			continue
		}

		if !intoChanged {
			// Only From changed this collection; apply From's change.
			switch {
			case fromH.IsEmpty():
				if err := editor.Delete(ctx, name); err != nil {
					return prolly.AddressMap{}, nil, fmt.Errorf("deleting collection %q: %w", name, err)
				}
			case intoH.IsEmpty():
				if err := editor.Add(ctx, name, fromH); err != nil {
					return prolly.AddressMap{}, nil, fmt.Errorf("adding collection %q: %w", name, err)
				}
			default:
				if err := editor.Update(ctx, name, fromH); err != nil {
					return prolly.AddressMap{}, nil, fmt.Errorf("updating collection %q: %w", name, err)
				}
			}
			continue
		}

		// Both sides changed this collection.
		if fromH.IsEmpty() && intoH.IsEmpty() {
			// Both independently deleted the collection; result is deletion (already absent).
			continue
		}
		if fromH.IsEmpty() || intoH.IsEmpty() {
			// Collection-level conflict: one side deleted, the other modified — unresolvable here.
			return prolly.AddressMap{}, nil, fmt.Errorf("conflict in collection %q: deleted on one branch and modified on the other", name)
		}

		// Both sides modified the collection — merge at document level, capturing conflicts.
		intoMap, err := openCollection(ctx, state.cs, state.ns, intoH)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("opening into collection %q: %w", name, err)
		}
		fromMap, err := openCollection(ctx, state.cs, state.ns, fromH)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("opening from collection %q: %w", name, err)
		}
		baseMap, err := openCollection(ctx, state.cs, state.ns, baseH)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("opening base collection %q: %w", name, err)
		}

		mergedMap, collConflicts, err := captureConflictsForCollection(ctx, intoMap, fromMap, baseMap, globalIdx)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("merging collection %q: %w", name, err)
		}
		globalIdx += len(collConflicts)

		if len(collConflicts) > 0 {
			allConflicts[name] = collConflicts
		}

		mergedH, err := state.dtblHashForMap(ctx, mergedMap)
		if err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("writing merged collection %q: %w", name, err)
		}
		if err := editor.Update(ctx, name, mergedH); err != nil {
			return prolly.AddressMap{}, nil, fmt.Errorf("updating merged collection %q in AM: %w", name, err)
		}
	}

	am, err := editor.Flush(ctx)
	if err != nil {
		return prolly.AddressMap{}, nil, err
	}

	return am, allConflicts, nil
}
