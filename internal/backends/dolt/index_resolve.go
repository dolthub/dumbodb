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
// All functions in this file are pure with respect to dbState: they read
// from the chunk store, NodeStore, and ValueStore handed to them, but
// touch no in-memory caches keyed by collection name. The process-wide
// entry memos here are content-addressed, so they are safe to share across
// branches and databases.

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/FerretDB/wire/wirebson"
	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/bson"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
)

type resolvedIndexEntry struct {
	info    backends.IndexInfo
	mapRoot hash.Hash
	// defHash identifies the definition; with mapRoot, the index state.
	defHash hash.Hash
}

// indexDefinitionMemo caches decoded definitions by their encoding, so a
// definition decodes once from any branch or database. It is keyed by
// definition alone because the map root changes on every indexed write, and
// by the bytes rather than their hash because every query resolves its
// indexes. No eviction; see design 6.1.
var indexDefinitionMemo sync.Map // string(definition) -> memoizedDefinition

type memoizedDefinition struct {
	info backends.IndexInfo
	hash hash.Hash
}

// resolveIndexEntry decodes the definition def of the index whose map is at
// root. The returned entry's info is shared and must not be mutated.
func resolveIndexEntry(def []byte, root hash.Hash) (*resolvedIndexEntry, error) {
	if v, ok := indexDefinitionMemo.Load(string(def)); ok {
		m := v.(memoizedDefinition)
		return &resolvedIndexEntry{info: m.info, mapRoot: root, defHash: m.hash}, nil
	}
	doc, err := bson.ToDocument(wirebson.RawDocument(def))
	if err != nil {
		return nil, fmt.Errorf("resolveIndexEntry: %w", err)
	}
	info, err := decodeIndexDefinition(doc)
	if err != nil {
		return nil, fmt.Errorf("resolveIndexEntry: %w", err)
	}
	actual, _ := indexDefinitionMemo.LoadOrStore(string(def), memoizedDefinition{info: info, hash: hash.Of(def)})
	m := actual.(memoizedDefinition)
	return &resolvedIndexEntry{info: m.info, mapRoot: root, defHash: m.hash}, nil
}

// emptySecondaryIndexesCache memoizes the per-NodeStore empty secondary
// indexes so writes to collections with none do not rebuild them. See
// design 6.4.
var emptySecondaryIndexesCache sync.Map // tree.NodeStore -> secondaryIndexes

func emptySecondaryIndexes(ctx context.Context, ns tree.NodeStore) (secondaryIndexes, error) {
	if v, ok := emptySecondaryIndexesCache.Load(ns); ok {
		return v.(secondaryIndexes), nil
	}
	s, err := newSecondaryIndexes(ctx, ns)
	if err != nil {
		return secondaryIndexes{}, fmt.Errorf("emptySecondaryIndexes: %w", err)
	}
	actual, _ := emptySecondaryIndexesCache.LoadOrStore(ns, s)
	return actual.(secondaryIndexes), nil
}

// forgetEmptySecondaryIndexes evicts ns from emptySecondaryIndexesCache. The
// cached map references ns, so without eviction a closed database's chunk
// store, journal buffers included, stays reachable for the life of the
// process.
func forgetEmptySecondaryIndexes(ns tree.NodeStore) {
	emptySecondaryIndexesCache.Delete(ns)
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

// secondaryIndexesForDTBL returns the secondary indexes inlined in the DTBL
// chunk at dtblHash, the shared empty ones when dtblHash is the zero hash,
// the chunk is empty, or the DTBL inlines no secondary_indexes field.
func secondaryIndexesForDTBL(ctx context.Context, cs *nbs.GenerationalNBS, ns tree.NodeStore, dtblHash hash.Hash) (secondaryIndexes, error) {
	if dtblHash.IsEmpty() {
		return emptySecondaryIndexes(ctx, ns)
	}
	data, err := readTableChunk(ctx, cs, dtblHash)
	if err != nil {
		return secondaryIndexes{}, fmt.Errorf("secondaryIndexesForDTBL: reading DTBL chunk: %w", err)
	}
	if len(data) == 0 {
		return emptySecondaryIndexes(ctx, ns)
	}
	if serial.GetFileID(data) != serial.TableFileID {
		return secondaryIndexes{}, fmt.Errorf("secondaryIndexesForDTBL: unexpected file ID %q (want DTBL)", serial.GetFileID(data))
	}
	tbl, err := serial.TryGetRootAsTable(data, serial.MessagePrefixSz)
	if err != nil {
		return secondaryIndexes{}, fmt.Errorf("secondaryIndexesForDTBL: parsing DTBL: %w", err)
	}
	if err := checkTableLayout(tbl); err != nil {
		return secondaryIndexes{}, fmt.Errorf("secondaryIndexesForDTBL: %w", err)
	}
	idxBytes := tbl.SecondaryIndexesBytes()
	if len(idxBytes) == 0 {
		return emptySecondaryIndexes(ctx, ns)
	}
	s, err := secondaryIndexesFromBytes(ns, idxBytes)
	if err != nil {
		return secondaryIndexes{}, fmt.Errorf("secondaryIndexesForDTBL: %w", err)
	}
	return s, nil
}

func secondaryIndexesFromAM(ctx context.Context, cs *nbs.GenerationalNBS, ns tree.NodeStore, collAM prolly.AddressMap, collName string) (secondaryIndexes, error) {
	dtblHash, err := collAM.Get(ctx, collName)
	if err != nil {
		return secondaryIndexes{}, err
	}
	return secondaryIndexesForDTBL(ctx, cs, ns, dtblHash)
}

// resolveCollSecondaryIndexes resolves the (session, branch)-routed
// AddressMap, looks up collName's DTBL, and returns its secondary indexes:
// the cached empty ones when the collection does not exist or has none;
// callers that need to distinguish those cases can branch on Count() == 0.
//
// The caller must hold at least state.mu.RLock(). Read-path call sites
// (ListIndexes, tryIndexLookup, tryIndexedCount, DistinctScan, Explain)
// take RLock around the call; write-path call sites (InsertAll,
// UpdateAll, DeleteAll, CreateIndexes, DropIndexes) already hold the
// write lock.
func resolveCollSecondaryIndexes(ctx context.Context, c *collection, state *dbState) (secondaryIndexes, error) {
	collAM, err := c.db.resolveAM(ctx, state)
	if err != nil {
		return secondaryIndexes{}, err
	}
	return secondaryIndexesFromAM(ctx, state.cs, state.ns, collAM, c.name)
}

// resolveIndexes returns parallel slices, sorted by index name: the
// IndexInfo of each secondary index and its prolly.Map.
func resolveIndexes(ctx context.Context, c *collection, state *dbState) ([]backends.IndexInfo, []prolly.Map, error) {
	indexes, err := resolveCollSecondaryIndexes(ctx, c, state)
	if err != nil {
		return nil, nil, err
	}
	cnt, err := indexes.Count()
	if err != nil {
		return nil, nil, err
	}
	if cnt == 0 {
		return nil, nil, nil
	}
	infos := make([]backends.IndexInfo, 0, cnt)
	maps := make([]prolly.Map, 0, cnt)
	if err := indexes.IterAll(ctx, func(_ string, entry *resolvedIndexEntry) error {
		m, err := openIndexMap(ctx, state.ns, entry.mapRoot)
		if err != nil {
			return err
		}
		infos = append(infos, entry.info)
		maps = append(maps, m)
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return infos, maps, nil
}

// resolveBranchIndexState is the write-path counterpart to resolveIndexes:
// returns the per-branch index state as (infos in name order, name
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

// indexEntriesForDoc is the single source of truth for which entries a
// document contributes: partial/sparse membership (a filter-evaluation
// error counts as non-membership), multikey expansion, and the
// Lossy/Multikey flag detection. Every build, insert, update, delete,
// and merge path routes through it.
func indexEntriesForDoc(ctx context.Context, doc *types.Document, idx backends.IndexInfo) (rows [][]any, multikey, lossy bool, err error) {
	if idx.MatchesPartialFilter != nil {
		match, err := idx.MatchesPartialFilter(doc)
		if err != nil || !match {
			return nil, false, false, nil
		}
	}
	if sparseExcludes(doc, idx) {
		return nil, false, false, nil
	}
	fieldVals := extractIndexFieldValues(doc, idx)
	for _, v := range fieldVals {
		if _, isArr := v.(*types.Array); isArr {
			multikey = true
		}
	}
	rows = expandMultiKeyValues(fieldVals)
	if cmp := indexComparator(idx); cmp != nil {
		for i := range rows {
			rows[i] = collateRowValues(rows[i], cmp)
		}
	}
	if multikey {
		if rows, err = firstOfEqualRows(ctx, rows); err != nil {
			return nil, false, false, err
		}
	}
	for _, row := range rows {
		for _, v := range row {
			if idxpkg.ValueLossy(v) {
				lossy = true
			}
		}
	}
	return rows, multikey, lossy, nil
}

// firstOfEqualRows drops each row whose key equals an earlier row's. Equal
// keys share one index entry, and mongod keeps the first element's value:
// {a: [2, 2.0]} indexes 2, and {a: [2.0, 2]} indexes 2.0.
func firstOfEqualRows(ctx context.Context, rows [][]any) ([][]any, error) {
	if len(rows) < 2 {
		return rows, nil
	}
	seen := make(map[string]struct{}, len(rows))
	out := rows[:0]
	for j, row := range rows {
		// A key can cost microseconds to encode, and an array holds one
		// per element.
		if j%256 == 255 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		key := string(idxpkg.BuildSecondaryKey(row, nil))
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, row)
	}
	return out, nil
}

// applyInsertsToIndexes is pure: inputs are not mutated; the returned infos
// carry updated Lossy/Multikey flags.
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
			h, err := primaryKey(docID)
			if err != nil {
				return nil, nil, fmt.Errorf("applyInsertsToIndexes: hashing _id for index %q: %w", info.Name, err)
			}
			idBytes := h
			if err := checkGeoKeys(doc, info); err != nil {
				return nil, nil, err
			}
			rows, multikey, lossy, err := indexEntriesForDoc(ctx, doc, info)
			if err != nil {
				return nil, nil, err
			}
			outInfos[i].Multikey = outInfos[i].Multikey || multikey
			outInfos[i].Lossy = outInfos[i].Lossy || lossy
			for j, fv := range rows {
				// As in firstOfEqualRows.
				if j%256 == 255 && ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
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

func entryKeysForRows(ctx context.Context, rows [][]any, idBytes []byte) (map[string][]any, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	out := make(map[string][]any, len(rows))
	for j, row := range rows {
		if j%256 == 255 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		out[string(idxpkg.BuildSecondaryKey(row, idBytes))] = row
	}
	return out, nil
}

// applyUpdatesToIndexes deletes/inserts only the entry-set difference
// per (oldDoc, newDoc) pair, so an update that does not change an
// indexed entry leaves that index's root hash untouched. Pure;
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
			h, err := primaryKey(docID)
			if err != nil {
				return nil, nil, fmt.Errorf("applyUpdatesToIndexes: hashing _id for index %q: %w", info.Name, err)
			}
			idBytes := h

			oldRows, _, _, err := indexEntriesForDoc(ctx, oldDocs[d], info)
			if err != nil {
				return nil, nil, err
			}
			if err := checkGeoKeys(newDocs[d], info); err != nil {
				return nil, nil, err
			}
			newRows, multikey, lossy, err := indexEntriesForDoc(ctx, newDocs[d], info)
			if err != nil {
				return nil, nil, err
			}
			outInfos[i].Multikey = outInfos[i].Multikey || multikey
			outInfos[i].Lossy = outInfos[i].Lossy || lossy

			oldKeys, err := entryKeysForRows(ctx, oldRows, idBytes)
			if err != nil {
				return nil, nil, err
			}
			newKeys, err := entryKeysForRows(ctx, newRows, idBytes)
			if err != nil {
				return nil, nil, err
			}

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

// applyDeletesToIndexes is pure.
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
			h, err := primaryKey(docID)
			if err != nil {
				return nil, fmt.Errorf("applyDeletesToIndexes: hashing _id for index %q: %w", info.Name, err)
			}
			idBytes := h
			rows, _, _, err := indexEntriesForDoc(ctx, doc, info)
			if err != nil {
				return nil, err
			}
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

// buildSecondaryIndexes writes each index's map root to the value store and
// returns the secondary indexes naming them. No dbState fields are touched.
// Callers pass the result to dtblHashForCollection.
func buildSecondaryIndexes(ctx context.Context, state *dbState, infos []backends.IndexInfo, maps map[string]prolly.Map) (secondaryIndexes, error) {
	if len(infos) == 0 {
		return emptySecondaryIndexes(ctx, state.ns)
	}
	roots := make([]hash.Hash, len(infos))
	for i, idx := range infos {
		m, ok := maps[idx.Name]
		if !ok {
			return secondaryIndexes{}, fmt.Errorf("buildSecondaryIndexes: index %q has no map", idx.Name)
		}
		ref, err := state.vs.WriteValue(ctx, tree.ValueFromNode(m.Node()))
		if err != nil {
			return secondaryIndexes{}, fmt.Errorf("buildSecondaryIndexes: writing index map root for %q: %w", idx.Name, err)
		}
		roots[i] = ref.TargetHash()
	}
	s, err := secondaryIndexesFromRoots(ctx, state.ns, infos, roots)
	if err != nil {
		return secondaryIndexes{}, fmt.Errorf("buildSecondaryIndexes: %w", err)
	}
	return s, nil
}

// computeIndexChanges compares the secondary indexes of two DTBL hashes and
// returns the per-kind index lifecycle: added, modified, and removed. An
// index is:
//   - added    when only the b side has it,
//   - removed  when only the a side has it,
//   - modified when both sides carry the same name but different entries
//     (drop+recreate with a different spec).
//
// Either hash may be the zero hash (collection absent on that side); in
// that case the corresponding side's index list is empty.
//
// All three returned slices are sorted by index name. Each IndexChange in
// modified carries both the pre-state (From) and post-state (To)
// definitions; the index name is From.Name == To.Name.
//
// This walks both sides in full rather than diffing them: the cost is
// O(number of indexes on the collection), not O(number of documents). Only
// the document-level diffs (forEachCollectionChange in collection_diff.go)
// scale with collection size and use prolly.DiffMaps.
func computeIndexChanges(ctx context.Context, state *dbState, aDTBL, bDTBL hash.Hash) (added []backends.IndexInfo, modified []backends.IndexChange, removed []backends.IndexInfo, err error) {
	entriesOf := func(dtbl hash.Hash) (map[string]*resolvedIndexEntry, error) {
		indexes, err := secondaryIndexesForDTBL(ctx, state.cs, state.ns, dtbl)
		if err != nil {
			return nil, err
		}
		entries := make(map[string]*resolvedIndexEntry)
		err = indexes.IterAll(ctx, func(name string, entry *resolvedIndexEntry) error {
			entries[name] = entry
			return nil
		})
		return entries, err
	}
	aEntries, err := entriesOf(aDTBL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("computeIndexChanges: a side: %w", err)
	}
	bEntries, err := entriesOf(bDTBL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("computeIndexChanges: b side: %w", err)
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
		a, inA := aEntries[name]
		b, inB := bEntries[name]
		switch {
		case !inA && inB:
			added = append(added, b.info)
		case inA && !inB:
			removed = append(removed, a.info)
		case a.defHash != b.defHash || a.mapRoot != b.mapRoot:
			modified = append(modified, backends.IndexChange{From: a.info, To: b.info})
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

func openIndexMap(ctx context.Context, ns tree.NodeStore, mapRoot hash.Hash) (prolly.Map, error) {
	// The node store's cache shares the root node across queries, and with it
	// the node's memoized hash, which pinning the map for GC reads.
	node, err := ns.Read(ctx, mapRoot)
	if err != nil {
		return prolly.Map{}, fmt.Errorf("openIndexMap: %w", err)
	}
	return prolly.NewMap(node, ns, idxpkg.KeyDescriptor(), idxpkg.ValDescriptor()), nil
}
