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

// Secondary-index resolution from the session's working root.
//
// See docs/design/branch-scoped-index-metadata.md sections 3.3 and 6.4.
//
// All functions in this file are pure with respect to dbState: they read
// from the chunk store, NodeStore, and ValueStore handed to them, but
// touch no in-memory caches keyed by collection name. The single
// process-wide memo here (indexEntryMemo) is content-addressed on the
// IndexEntry chunk hash, so it is safe to share across branches and
// databases.

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
)

// resolvedIndexEntry is the decoded form of an IndexEntry chunk: the
// metadata plus the hash of the index's prolly.Map root.
type resolvedIndexEntry struct {
	info    backends.IndexInfo
	mapRoot hash.Hash
}

// indexEntryMemo caches resolveIndexEntry results. Hashes are immutable
// and content-addressed; the same hash from any branch or database
// resolves to the same metadata. No eviction; see design 6.1.
var indexEntryMemo sync.Map // hash.Hash -> *resolvedIndexEntry

// emptyIndexAMCache memoizes the per-NodeStore empty AddressMap so writes
// that have no secondary indexes do not re-allocate a fresh empty AM.
// Replaces the per-dbState state.emptyIndexAM field; see design 6.4.
var emptyIndexAMCache sync.Map // tree.NodeStore -> prolly.AddressMap

// emptyIndexAM returns the cached empty AddressMap for ns, building it on
// first call.
func emptyIndexAM(ns tree.NodeStore) (prolly.AddressMap, error) {
	if v, ok := emptyIndexAMCache.Load(ns); ok {
		return v.(prolly.AddressMap), nil
	}
	am, err := prolly.NewEmptyAddressMap(ns)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("emptyIndexAM: %w", err)
	}
	actual, _ := emptyIndexAMCache.LoadOrStore(ns, am)
	return actual.(prolly.AddressMap), nil
}

// dtblHashForColl returns the DTBL hash for collName under rv. Returns
// the zero hash if the collection is not in the root's ADRM.
func dtblHashForColl(ctx context.Context, ns tree.NodeStore, rv doltdb.RootValue, collName string) (hash.Hash, error) {
	if rv == nil {
		return hash.Hash{}, nil
	}
	am, err := amFromWorkingRoot(ctx, rv, ns)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("dtblHashForColl: %w", err)
	}
	h, err := am.Get(ctx, collName)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("dtblHashForColl: %s: %w", collName, err)
	}
	return h, nil
}

// indexAMForDTBL returns the secondary_indexes AddressMap inlined in the
// DTBL chunk at dtblHash. Returns the shared empty AM if dtblHash is the
// zero hash, the chunk is empty, the chunk is a legacy TUPM (no secondary
// indexes encoded), or the DTBL inlines an empty secondary_indexes field.
func indexAMForDTBL(ctx context.Context, cs *nbs.GenerationalNBS, ns tree.NodeStore, dtblHash hash.Hash) (prolly.AddressMap, error) {
	if dtblHash.IsEmpty() {
		return emptyIndexAM(ns)
	}
	chunk, err := cs.Get(ctx, dtblHash)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("indexAMForDTBL: reading DTBL chunk: %w", err)
	}
	data := chunk.Data()
	if len(data) == 0 {
		return emptyIndexAM(ns)
	}
	if serial.GetFileID(data) != serial.TableFileID {
		// Legacy TUPM has no secondary indexes encoded.
		return emptyIndexAM(ns)
	}
	tbl, err := serial.TryGetRootAsTable(data, serial.MessagePrefixSz)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("indexAMForDTBL: parsing DTBL: %w", err)
	}
	idxBytes := tbl.SecondaryIndexesBytes()
	if len(idxBytes) == 0 {
		return emptyIndexAM(ns)
	}
	amNode, _, err := tree.NodeFromBytes(idxBytes)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("indexAMForDTBL: parsing AM node: %w", err)
	}
	am, err := prolly.NewAddressMap(amNode, ns)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("indexAMForDTBL: building AM: %w", err)
	}
	return am, nil
}

// resolveIndexEntry decodes the IndexEntry JSON chunk at entryHash via
// the process-wide memo. The returned pointer must not be mutated by the
// caller. See design 6.5 for why memoizing is safe (chunk bytes are
// immutable; the captured MatchesPartialFilter closure is pure).
func resolveIndexEntry(ctx context.Context, ns tree.NodeStore, entryHash hash.Hash) (*resolvedIndexEntry, error) {
	if entryHash.IsEmpty() {
		return nil, fmt.Errorf("resolveIndexEntry: empty hash")
	}
	if v, ok := indexEntryMemo.Load(entryHash); ok {
		return v.(*resolvedIndexEntry), nil
	}
	entry, err := readIndexEntryChunk(ctx, ns, entryHash)
	if err != nil {
		return nil, fmt.Errorf("resolveIndexEntry: %w", err)
	}
	info, mapRoot, err := entryToIndexInfo(entry)
	if err != nil {
		return nil, fmt.Errorf("resolveIndexEntry: decode: %w", err)
	}
	resolved := &resolvedIndexEntry{info: info, mapRoot: mapRoot}
	actual, _ := indexEntryMemo.LoadOrStore(entryHash, resolved)
	return actual.(*resolvedIndexEntry), nil
}

// indexAMFromAM returns the per-collection secondary_indexes AddressMap
// stored under collName in collAM. Convenience for callers that already
// hold an ADRM (e.g. a merge driver iterating two branches' ADRMs).
func indexAMFromAM(ctx context.Context, cs *nbs.GenerationalNBS, ns tree.NodeStore, collAM prolly.AddressMap, collName string) (prolly.AddressMap, error) {
	dtblHash, err := collAM.Get(ctx, collName)
	if err != nil {
		return prolly.AddressMap{}, err
	}
	return indexAMForDTBL(ctx, cs, ns, dtblHash)
}

// resolveCollIndexAM resolves the (session, branch)-routed AddressMap,
// looks up collName's DTBL, and returns the secondary_indexes
// AddressMap. Returns the cached empty AM when the collection does not
// exist or has no secondary indexes; callers that need to distinguish
// those cases can branch on (am.Count() == 0).
//
// The caller must hold at least state.mu.RLock(). Read-path call sites
// (ListIndexes, tryIndexLookup, tryIndexedCount, DistinctScan, Explain)
// take RLock around the call; write-path call sites (InsertAll,
// UpdateAll, DeleteAll, CreateIndexes, DropIndexes) already hold the
// write lock.
func resolveCollIndexAM(ctx context.Context, c *collection, state *dbState) (prolly.AddressMap, error) {
	collAM, err := c.db.resolveAM(ctx, state)
	if err != nil {
		return prolly.AddressMap{}, err
	}
	dtblHash, err := collAM.Get(ctx, c.name)
	if err != nil {
		return prolly.AddressMap{}, err
	}
	return indexAMForDTBL(ctx, state.cs, state.ns, dtblHash)
}

// resolveIndexes walks the index AM and resolves every entry through
// the memoized JSON-decode path. Returns parallel slices: the IndexInfo
// for each index, and the prolly.Map handle. Both slices are in AM-walk
// order (sorted by index name).
func resolveIndexes(ctx context.Context, c *collection, state *dbState) ([]backends.IndexInfo, []prolly.Map, error) {
	idxAM, err := resolveCollIndexAM(ctx, c, state)
	if err != nil {
		return nil, nil, err
	}
	cnt, err := idxAM.Count()
	if err != nil {
		return nil, nil, err
	}
	if cnt == 0 {
		return nil, nil, nil
	}
	infos := make([]backends.IndexInfo, 0, cnt)
	maps := make([]prolly.Map, 0, cnt)
	if err := idxAM.IterAll(ctx, func(_ string, entryHash hash.Hash) error {
		if entryHash.IsEmpty() {
			return nil
		}
		resolved, rerr := resolveIndexEntry(ctx, state.ns, entryHash)
		if rerr != nil {
			return rerr
		}
		m, merr := openIndexMap(ctx, state.vs, state.ns, resolved.mapRoot)
		if merr != nil {
			return merr
		}
		infos = append(infos, resolved.info)
		maps = append(maps, m)
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return infos, maps, nil
}

// resolveBranchIndexState is the write-path counterpart to resolveIndexes:
// returns the per-branch index state as (infos in name-sorted order, name
// -> map). It reads from the session's working root via the resolver
// chain. Mutating these maps is safe: they are immutable prolly.Map
// values, so .Mutate() / .Map(ctx) round-trips do not touch any shared
// state.
func resolveBranchIndexState(ctx context.Context, c *collection, state *dbState) ([]backends.IndexInfo, map[string]prolly.Map, error) {
	infos, maps, err := resolveIndexes(ctx, c, state)
	if err != nil {
		return nil, nil, err
	}
	byName := make(map[string]prolly.Map, len(infos))
	for i, info := range infos {
		byName[info.Name] = maps[i]
	}
	return infos, byName, nil
}

// indexEntriesForDoc is the single source of truth for which index
// entries a document contributes: it applies the index's membership
// rules, expands multikey arrays, and reports whether the doc makes
// the index multikey (an array value expanded per element) or lossy
// (a value the KeyString encoding cannot represent faithfully).
//
// Membership: a doc outside a partial index's filter expression
// contributes no entries (a filter-evaluation error counts as
// non-membership, mirroring the unique-validation path), and a doc
// whose indexed fields are all missing contributes nothing to a
// sparse index. Index content is authoritative for membership --
// every build, insert, update, delete, and merge path routes through
// this function.
func indexEntriesForDoc(doc *types.Document, idx backends.IndexInfo) (rows [][]any, multikey, lossy bool) {
	if idx.MatchesPartialFilter != nil {
		match, err := idx.MatchesPartialFilter(doc)
		if err != nil || !match {
			return nil, false, false
		}
	}
	fieldVals := extractIndexFieldValues(doc, idx)
	if idx.Sparse && allNull(fieldVals) {
		return nil, false, false
	}
	for _, v := range fieldVals {
		if _, isArr := v.(*types.Array); isArr {
			multikey = true
		}
	}
	rows = expandMultiKeyValues(fieldVals)
	for _, row := range rows {
		for _, v := range row {
			if idxpkg.EncodeValueLossy(v) {
				lossy = true
			}
		}
	}
	return rows, multikey, lossy
}

// applyInsertsToIndexes runs each inserted document through every
// indexed field and adds the corresponding entries to a fresh copy of
// the input maps. Pure: neither input slice/map is mutated; the
// returned infos carry updated Lossy/Multikey flags and the returned
// map contains new prolly.Map values for indexes that had any new
// entries.
//
// Used by write paths that previously called updateSecondaryIndexesOnInsert
// against state.secIndexMaps. The new shape decouples the in-memory
// mutation from any dbState field so per-branch writes do not collide.
func applyInsertsToIndexes(ctx context.Context, infos []backends.IndexInfo, maps map[string]prolly.Map, docs []*types.Document) ([]backends.IndexInfo, map[string]prolly.Map, error) {
	if len(infos) == 0 {
		return infos, maps, nil
	}
	out := make(map[string]prolly.Map, len(maps))
	for k, v := range maps {
		out[k] = v
	}
	outInfos := append([]backends.IndexInfo(nil), infos...)
	for i, info := range outInfos {
		idxMap, ok := out[info.Name]
		if !ok {
			return nil, nil, fmt.Errorf("applyInsertsToIndexes: missing map for %q", info.Name)
		}
		mut := idxMap.Mutate()
		for _, doc := range docs {
			docID, err := doc.Get("_id")
			if err != nil {
				return nil, nil, fmt.Errorf("applyInsertsToIndexes: doc missing _id for index %q: %w", info.Name, err)
			}
			h, err := hashID(docID)
			if err != nil {
				return nil, nil, fmt.Errorf("applyInsertsToIndexes: hashing _id for index %q: %w", info.Name, err)
			}
			idBytes := h[:]
			rows, multikey, lossy := indexEntriesForDoc(doc, info)
			outInfos[i].Multikey = outInfos[i].Multikey || multikey
			outInfos[i].Lossy = outInfos[i].Lossy || lossy
			for _, fv := range rows {
				if err := idxpkg.InsertEntry(ctx, mut, fv, idBytes); err != nil {
					return nil, nil, fmt.Errorf("applyInsertsToIndexes: inserting entry into %q: %w", info.Name, err)
				}
			}
		}
		updated, err := mut.Map(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("applyInsertsToIndexes: flushing map for %q: %w", info.Name, err)
		}
		out[info.Name] = updated
	}
	return outInfos, out, nil
}

// entryKeysForRows maps each entry row to its full composite index key
// bytes (as a map key) for set-difference comparison.
func entryKeysForRows(rows [][]any, idBytes []byte) map[string][]any {
	if len(rows) == 0 {
		return nil
	}
	out := make(map[string][]any, len(rows))
	for _, row := range rows {
		out[string(idxpkg.BuildSecondaryKey(row, idBytes))] = row
	}
	return out
}

// applyUpdatesToIndexes applies per-document index maintenance for
// updated documents: for each (oldDoc, newDoc) pair it computes the
// entry-set difference and deletes/inserts only the changed entries.
// An update that does not change any indexed entry performs zero edits
// on that index (the noop short-circuit falls out of the empty
// difference), so untouched indexes keep their root hash bit for bit.
//
// Pure in the same sense as applyInsertsToIndexes; the returned infos
// carry updated Lossy/Multikey flags from the new document versions.
// oldDocs and newDocs are parallel slices.
func applyUpdatesToIndexes(ctx context.Context, infos []backends.IndexInfo, maps map[string]prolly.Map, oldDocs, newDocs []*types.Document) ([]backends.IndexInfo, map[string]prolly.Map, error) {
	if len(infos) == 0 || len(oldDocs) == 0 {
		return infos, maps, nil
	}
	if len(oldDocs) != len(newDocs) {
		return nil, nil, fmt.Errorf("applyUpdatesToIndexes: %d old docs vs %d new docs", len(oldDocs), len(newDocs))
	}
	out := make(map[string]prolly.Map, len(maps))
	for k, v := range maps {
		out[k] = v
	}
	outInfos := append([]backends.IndexInfo(nil), infos...)
	for i, info := range outInfos {
		idxMap, ok := out[info.Name]
		if !ok {
			return nil, nil, fmt.Errorf("applyUpdatesToIndexes: missing map for %q", info.Name)
		}
		mut := idxMap.Mutate()
		edited := false
		for d := range oldDocs {
			docID, err := newDocs[d].Get("_id")
			if err != nil {
				return nil, nil, fmt.Errorf("applyUpdatesToIndexes: doc missing _id for index %q: %w", info.Name, err)
			}
			h, err := hashID(docID)
			if err != nil {
				return nil, nil, fmt.Errorf("applyUpdatesToIndexes: hashing _id for index %q: %w", info.Name, err)
			}
			idBytes := h[:]

			oldRows, _, _ := indexEntriesForDoc(oldDocs[d], info)
			newRows, multikey, lossy := indexEntriesForDoc(newDocs[d], info)
			outInfos[i].Multikey = outInfos[i].Multikey || multikey
			outInfos[i].Lossy = outInfos[i].Lossy || lossy

			oldKeys := entryKeysForRows(oldRows, idBytes)
			newKeys := entryKeysForRows(newRows, idBytes)

			for k, row := range oldKeys {
				if _, keep := newKeys[k]; keep {
					continue
				}
				if err := idxpkg.DeleteEntry(ctx, mut, row, idBytes); err != nil {
					return nil, nil, fmt.Errorf("applyUpdatesToIndexes: deleting entry from %q: %w", info.Name, err)
				}
				edited = true
			}
			for k, row := range newKeys {
				if _, had := oldKeys[k]; had {
					continue
				}
				if err := idxpkg.InsertEntry(ctx, mut, row, idBytes); err != nil {
					return nil, nil, fmt.Errorf("applyUpdatesToIndexes: inserting entry into %q: %w", info.Name, err)
				}
				edited = true
			}
		}
		if !edited {
			continue
		}
		updated, err := mut.Map(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("applyUpdatesToIndexes: flushing map for %q: %w", info.Name, err)
		}
		out[info.Name] = updated
	}
	return outInfos, out, nil
}

// applyDeletesToIndexes removes every entry the deleted documents
// contributed. Pure in the same sense as applyInsertsToIndexes.
func applyDeletesToIndexes(ctx context.Context, infos []backends.IndexInfo, maps map[string]prolly.Map, oldDocs []*types.Document) (map[string]prolly.Map, error) {
	if len(infos) == 0 || len(oldDocs) == 0 {
		return maps, nil
	}
	out := make(map[string]prolly.Map, len(maps))
	for k, v := range maps {
		out[k] = v
	}
	for _, info := range infos {
		idxMap, ok := out[info.Name]
		if !ok {
			return nil, fmt.Errorf("applyDeletesToIndexes: missing map for %q", info.Name)
		}
		mut := idxMap.Mutate()
		edited := false
		for _, doc := range oldDocs {
			docID, err := doc.Get("_id")
			if err != nil {
				return nil, fmt.Errorf("applyDeletesToIndexes: doc missing _id for index %q: %w", info.Name, err)
			}
			h, err := hashID(docID)
			if err != nil {
				return nil, fmt.Errorf("applyDeletesToIndexes: hashing _id for index %q: %w", info.Name, err)
			}
			idBytes := h[:]
			rows, _, _ := indexEntriesForDoc(doc, info)
			for _, row := range rows {
				if err := idxpkg.DeleteEntry(ctx, mut, row, idBytes); err != nil {
					return nil, fmt.Errorf("applyDeletesToIndexes: deleting entry from %q: %w", info.Name, err)
				}
				edited = true
			}
		}
		if !edited {
			continue
		}
		updated, err := mut.Map(ctx)
		if err != nil {
			return nil, fmt.Errorf("applyDeletesToIndexes: flushing map for %q: %w", info.Name, err)
		}
		out[info.Name] = updated
	}
	return out, nil
}

// buildIndexAM is the pure replacement for persistIndexes: given a set of
// (IndexInfo, prolly.Map) pairs it writes each map's root to the value
// store and a per-index JSON IndexEntry chunk, then returns a new
// AddressMap from index name to IndexEntry hash. No dbState fields are
// touched. Callers pass the result to dtblHashForCollection.
func buildIndexAM(ctx context.Context, state *dbState, infos []backends.IndexInfo, maps map[string]prolly.Map) (prolly.AddressMap, error) {
	if len(infos) == 0 {
		return emptyIndexAM(state.ns)
	}
	am, err := prolly.NewEmptyAddressMap(state.ns)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("buildIndexAM: %w", err)
	}
	edt := am.Editor()
	for _, idx := range infos {
		m, ok := maps[idx.Name]
		if !ok {
			return prolly.AddressMap{}, fmt.Errorf("buildIndexAM: index %q has no map", idx.Name)
		}
		ref, err := state.vs.WriteValue(ctx, tree.ValueFromNode(m.Node()))
		if err != nil {
			return prolly.AddressMap{}, fmt.Errorf("buildIndexAM: writing index map root for %q: %w", idx.Name, err)
		}
		mapRoot := ref.TargetHash()
		entry, err := indexInfoToEntry(idx, mapRoot)
		if err != nil {
			return prolly.AddressMap{}, fmt.Errorf("buildIndexAM: encoding entry for %q: %w", idx.Name, err)
		}
		entryHash, err := writeIndexEntryChunk(ctx, state.ns, entry)
		if err != nil {
			return prolly.AddressMap{}, fmt.Errorf("buildIndexAM: writing entry chunk for %q: %w", idx.Name, err)
		}
		if err := edt.Add(ctx, idx.Name, entryHash); err != nil {
			return prolly.AddressMap{}, fmt.Errorf("buildIndexAM: adding %q to AM: %w", idx.Name, err)
		}
	}
	newAM, err := edt.Flush(ctx)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("buildIndexAM: flush: %w", err)
	}
	return newAM, nil
}

// computeIndexChanges compares the secondary-index AMs of two DTBL
// hashes and returns the per-kind index lifecycle: added, modified, and
// removed. An index is:
//   - added    when only the b side has it,
//   - removed  when only the a side has it,
//   - modified when both sides carry the same name but different
//              IndexEntry chunk hashes (drop+recreate with a different
//              spec).
//
// Either hash may be the zero hash (collection absent on that side); in
// that case the corresponding side's index list is empty.
//
// All three returned slices are sorted by index name. The IndexInfo
// values are decoded via resolveIndexEntry (memoized). Each IndexChange
// in modified carries both the pre-state (From) and post-state (To)
// definitions; the index name is From.Name == To.Name.
//
// This walks both index address maps in full rather than using a structural-
// sharing diff. That is intentional: the cost is O(number of indexes on the
// collection), not O(number of documents), and prolly.AddressMap exposes no
// public diff primitive. Only the document-level diffs (forEachCollectionChange
// in collection_diff.go) scale with collection size and use prolly.DiffMaps.
func computeIndexChanges(ctx context.Context, state *dbState, aDTBL, bDTBL hash.Hash) (added []backends.IndexInfo, modified []backends.IndexChange, removed []backends.IndexInfo, err error) {
	aAM, err := indexAMForDTBL(ctx, state.cs, state.ns, aDTBL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("computeIndexChanges: a side: %w", err)
	}
	bAM, err := indexAMForDTBL(ctx, state.cs, state.ns, bDTBL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("computeIndexChanges: b side: %w", err)
	}

	aEntries := make(map[string]hash.Hash)
	if err = aAM.IterAll(ctx, func(name string, h hash.Hash) error {
		aEntries[name] = h
		return nil
	}); err != nil {
		return nil, nil, nil, fmt.Errorf("computeIndexChanges: walking a side: %w", err)
	}
	bEntries := make(map[string]hash.Hash)
	if err = bAM.IterAll(ctx, func(name string, h hash.Hash) error {
		bEntries[name] = h
		return nil
	}); err != nil {
		return nil, nil, nil, fmt.Errorf("computeIndexChanges: walking b side: %w", err)
	}

	allNames := make(map[string]struct{}, len(aEntries)+len(bEntries))
	for n := range aEntries {
		allNames[n] = struct{}{}
	}
	for n := range bEntries {
		allNames[n] = struct{}{}
	}
	sorted := make([]string, 0, len(allNames))
	for n := range allNames {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	for _, name := range sorted {
		aH, inA := aEntries[name]
		bH, inB := bEntries[name]
		switch {
		case !inA && inB:
			info, rerr := resolveIndexEntry(ctx, state.ns, bH)
			if rerr != nil {
				return nil, nil, nil, fmt.Errorf("computeIndexChanges: resolving b entry for %q: %w", name, rerr)
			}
			added = append(added, info.info)
		case inA && !inB:
			info, rerr := resolveIndexEntry(ctx, state.ns, aH)
			if rerr != nil {
				return nil, nil, nil, fmt.Errorf("computeIndexChanges: resolving a entry for %q: %w", name, rerr)
			}
			removed = append(removed, info.info)
		case aH != bH:
			aInfo, rerr := resolveIndexEntry(ctx, state.ns, aH)
			if rerr != nil {
				return nil, nil, nil, fmt.Errorf("computeIndexChanges: resolving a entry for %q: %w", name, rerr)
			}
			bInfo, rerr := resolveIndexEntry(ctx, state.ns, bH)
			if rerr != nil {
				return nil, nil, nil, fmt.Errorf("computeIndexChanges: resolving b entry for %q: %w", name, rerr)
			}
			modified = append(modified, backends.IndexChange{From: aInfo.info, To: bInfo.info})
		}
	}
	return added, modified, removed, nil
}

// indexNamesOf extracts the Name field from each IndexInfo, in input
// order. Returns a nil slice for nil input so callers passing a status
// row through json.Marshal-style code can rely on "always emit []"
// downstream by len-checking, not nil-checking.
func indexNamesOf(infos []backends.IndexInfo) []string {
	if len(infos) == 0 {
		return nil
	}
	out := make([]string, len(infos))
	for i, info := range infos {
		out[i] = info.Name
	}
	return out
}

// indexChangeNamesOf extracts the index name from each IndexChange.
// From.Name == To.Name by construction (same-name drop+recreate).
func indexChangeNamesOf(changes []backends.IndexChange) []string {
	if len(changes) == 0 {
		return nil
	}
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = c.From.Name
	}
	return out
}

// openIndexMap returns a prolly.Map handle for the secondary-index data
// stored at mapRoot.
func openIndexMap(ctx context.Context, vs *dolttypes.ValueStore, ns tree.NodeStore, mapRoot hash.Hash) (prolly.Map, error) {
	v, err := vs.ReadValue(ctx, mapRoot)
	if err != nil {
		return prolly.Map{}, fmt.Errorf("openIndexMap: ReadValue: %w", err)
	}
	msg, ok := v.(dolttypes.SerialMessage)
	if !ok {
		return prolly.Map{}, fmt.Errorf("openIndexMap: unexpected value type %T", v)
	}
	node, _, err := tree.NodeFromBytes([]byte(msg))
	if err != nil {
		return prolly.Map{}, fmt.Errorf("openIndexMap: NodeFromBytes: %w", err)
	}
	return prolly.NewMap(node, ns, idxpkg.KeyDescriptor(), idxpkg.ValDescriptor()), nil
}
