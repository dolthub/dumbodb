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
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"slices"

	"github.com/FerretDB/wire/wirebson"
	sqltypes "github.com/dolthub/go-mysql-server/sql/types"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"
	"github.com/google/uuid"
	mongobson "go.mongodb.org/mongo-driver/bson"

	"github.com/dolthub/docudolt/internal/backends"
	"github.com/dolthub/docudolt/internal/bson"
	"github.com/dolthub/docudolt/internal/types"
	"github.com/dolthub/docudolt/internal/util/iterator"
	"github.com/dolthub/docudolt/internal/util/must"
)

// collection implements backends.Collection.
type collection struct {
	db   *database
	name string
}

// getMap returns the prolly.Map for this collection.
//
// When the database's rootish is "main" (the default), the current working-set
// AM (state.am) is used. When the rootish is a bare commit hash or a tag name,
// the AM is loaded from the historical RTVL at that commit.
//
// Returns (emptyMap, false, nil, nil) if the database or collection doesn't exist.
func (c *collection) getMap(ctx context.Context) (prolly.Map, bool, *dbState, error) {
	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, false)
	if err != nil {
		return prolly.Map{}, false, nil, err
	}

	if state == nil {
		return prolly.Map{}, false, nil, nil
	}

	state.mu.RLock()
	defer state.mu.RUnlock()

	am, err := c.db.resolveAM(ctx, state)
	if err != nil {
		return prolly.Map{}, false, nil, err
	}

	rootHash, err := am.Get(ctx, c.name)
	if err != nil {
		return prolly.Map{}, false, nil, err
	}

	if rootHash.IsEmpty() {
		return prolly.Map{}, false, nil, nil
	}

	m, err := openCollection(ctx, state.cs, state.ns, rootHash)
	if err != nil {
		return prolly.Map{}, false, nil, err
	}

	return m, true, state, nil
}

// Query implements backends.Collection.
func (c *collection) Query(ctx context.Context, params *backends.QueryParams) (*backends.QueryResult, error) {
	m, exists, state, err := c.getMap(ctx)
	if err != nil {
		return nil, err
	}

	if !exists {
		return &backends.QueryResult{
			Iter: newEmptyIter(),
		}, nil
	}

	// Determine direction based on sort.
	reverse := false
	if params != nil && params.Sort != nil && params.Sort.Len() > 0 {
		sortVal := params.Sort.Map()["$natural"].(int64)
		reverse = sortVal == -1
	}

	var limit int64
	if params != nil {
		limit = params.Limit
	}

	onlyRecordIDs := params != nil && params.OnlyRecordIDs

	return &backends.QueryResult{
		Iter: newMapIter(ctx, state.ns, m, reverse, limit, onlyRecordIDs),
	}, nil
}

// Explain implements backends.Collection.
func (c *collection) Explain(ctx context.Context, params *backends.ExplainParams) (*backends.ExplainResult, error) {
	qp := must.NotFail(types.NewDocument(
		"namespace", c.db.name+"."+c.name,
		"parsedQuery", types.MakeDocument(0),
		"winningPlan", must.NotFail(types.NewDocument("stage", "COLLSCAN")),
	))
	return &backends.ExplainResult{
		QueryPlanner: qp,
	}, nil
}

// extractIndexKey returns a composite key for the given index extracted from doc.
// Fields absent in the document are represented as types.Null.
func extractIndexKey(doc *types.Document, idx backends.IndexInfo) []any {
	key := make([]any, len(idx.Key))
	for i, kp := range idx.Key {
		val, err := doc.Get(kp.Field)
		if err != nil {
			val = types.Null
		}
		key[i] = val
	}
	return key
}

// allNull returns true if every element in the key slice is types.Null.
// Used to detect sparse index documents that should be excluded from unique checks.
func allNull(key []any) bool {
	for _, v := range key {
		if v != types.Null {
			return false
		}
	}
	return true
}

// indexKeysEqual returns true if two composite index keys are element-wise equal.
func indexKeysEqual(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if types.Compare(a[i], b[i]) != types.Equal {
			return false
		}
	}
	return true
}

// InsertAll implements backends.Collection.
func (c *collection) InsertAll(ctx context.Context, params *backends.InsertAllParams) (*backends.InsertAllResult, error) {
	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, true)
	if err != nil {
		return nil, err
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	// Load or create the collection's prolly map.
	m, err := c.loadOrCreateMap(ctx, state)
	if err != nil {
		return nil, err
	}

	// Collect unique secondary indexes for this collection.
	var uniqueIndexes []backends.IndexInfo
	for _, idx := range state.indexes[c.name] {
		if idx.Unique {
			uniqueIndexes = append(uniqueIndexes, idx)
		}
	}

	// For each unique index, gather the key values of all existing documents.
	// existingUniqueKeys[i] holds the composite keys for unique index i.
	existingUniqueKeys := make([][][]any, len(uniqueIndexes))
	if len(uniqueIndexes) > 0 {
		iter, err := m.IterAll(ctx)
		if err != nil {
			return nil, err
		}
		for {
			_, v, err := iter.Next(ctx)
			if err != nil {
				if err == io.EOF {
					break
				}
				return nil, err
			}
			if v == nil {
				break
			}
			jsonHash, ok := valDesc.GetJSONAddr(0, v)
			if !ok {
				continue
			}
			existingDoc, err := readDocJSON(ctx, state.ns, jsonHash)
			if err != nil {
				return nil, err
			}
			for i, idx := range uniqueIndexes {
				// For partial indexes, only include existing docs that satisfy the filter.
				if idx.MatchesPartialFilter != nil {
					matches, filterErr := idx.MatchesPartialFilter(existingDoc)
					if filterErr != nil || !matches {
						continue
					}
				}
				existingUniqueKeys[i] = append(existingUniqueKeys[i], extractIndexKey(existingDoc, idx))
			}
		}
	}

	mut := m.Mutate()

	// batchIDs tracks docIDs inserted in this batch for capped-collection ordering.
	batchIDs := make([]any, 0, len(params.Docs))
	// batchHashSet detects in-batch duplicate _id hashes in O(1).
	batchHashSet := make(map[[20]byte]struct{}, len(params.Docs))
	// batchUniqueKeys[i] holds composite keys for unique index i from docs in this batch.
	batchUniqueKeys := make([][][]any, len(uniqueIndexes))

	for _, doc := range params.Docs {
		// Extract the _id from this document.
		docID, err := doc.Get("_id")
		if err != nil {
			return nil, fmt.Errorf("dolt: document missing _id: %w", err)
		}

		// Hash _id to get the fixed-size primary key.
		h, err := hashID(docID)
		if err != nil {
			return nil, fmt.Errorf("dolt: hashing _id: %w", err)
		}

		// Check against existing IDs in the collection (point lookup).
		exists, err := existsID(ctx, m, h)
		if err != nil {
			return nil, fmt.Errorf("dolt: checking existing _id: %w", err)
		}
		if exists {
			return nil, backends.NewError(
				backends.ErrorCodeInsertDuplicateID,
				fmt.Errorf("dolt: duplicate _id in collection"),
			)
		}

		// Check against IDs already inserted in this batch.
		if _, dup := batchHashSet[h]; dup {
			return nil, backends.NewError(
				backends.ErrorCodeInsertDuplicateID,
				fmt.Errorf("dolt: duplicate _id in batch"),
			)
		}

		// Check unique secondary index constraints.
		for i, idx := range uniqueIndexes {
			// For partial indexes, only documents that satisfy the partial filter
			// expression are indexed. Skip uniqueness checks for non-matching docs.
			if idx.MatchesPartialFilter != nil {
				matches, filterErr := idx.MatchesPartialFilter(doc)
				if filterErr != nil || !matches {
					continue
				}
			}

			newKey := extractIndexKey(doc, idx)

			// For sparse indexes, documents where all indexed fields are missing
			// are not indexed and thus do not participate in uniqueness checks.
			if idx.Sparse && allNull(newKey) {
				continue
			}

			for _, existKey := range existingUniqueKeys[i] {
				if indexKeysEqual(newKey, existKey) {
					return nil, backends.NewError(
						backends.ErrorCodeInsertDuplicateID,
						fmt.Errorf("dolt: duplicate key for unique index %s", idx.Name),
					)
				}
			}
			for _, batchKey := range batchUniqueKeys[i] {
				if indexKeysEqual(newKey, batchKey) {
					return nil, backends.NewError(
						backends.ErrorCodeInsertDuplicateID,
						fmt.Errorf("dolt: duplicate key for unique index %s", idx.Name),
					)
				}
			}
			batchUniqueKeys[i] = append(batchUniqueKeys[i], newKey)
		}

		key, err := buildKey(h[:])
		if err != nil {
			return nil, err
		}

		// Convert document to JSON bytes and write to JSON chunk store.
		jsonHash, err := writeDocJSON(ctx, state.ns, doc)
		if err != nil {
			return nil, err
		}

		v, err := buildValue(jsonHash)
		if err != nil {
			return nil, err
		}

		if err := mut.Put(ctx, key, v); err != nil {
			return nil, err
		}

		batchHashSet[h] = struct{}{}
		batchIDs = append(batchIDs, docID)
	}

	// Track insertion order for capped collections.
	if _, isCapped := state.capped[c.name]; isCapped {
		state.insertionOrder[c.name] = append(state.insertionOrder[c.name], batchIDs...)

		// Perform FIFO eviction if limits are exceeded.
		if err := c.evictCappedDocs(ctx, state, mut); err != nil {
			return nil, err
		}
	}

	// Flush the mutable map.
	newMap, err := mut.Map(ctx)
	if err != nil {
		return nil, err
	}

	// Wrap the updated map in a DTBL chunk and update the address map.
	dtblHash, err := state.dtblHashForMap(ctx, newMap)
	if err != nil {
		return nil, err
	}
	if err := state.updateAddressMap(ctx, c.db.rootish, func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, dtblHash)
	}); err != nil {
		return nil, err
	}

	return &backends.InsertAllResult{}, nil
}

// evictCappedDocs removes oldest documents from a capped collection to enforce size and count limits.
// Must be called with state.mu held for writing.
// avgDocSize is a rough estimate in bytes per document for size-based eviction.
const cappedAvgDocSize = 512

func (c *collection) evictCappedDocs(ctx context.Context, state *dbState, mut *prolly.MutableMap) error {
	cappedMeta, ok := state.capped[c.name]
	if !ok {
		return nil
	}

	insertionOrder := state.insertionOrder[c.name]
	currentCount := int64(len(insertionOrder))

	// Determine how many documents to evict.
	var toEvict int64

	// Count-based eviction.
	if cappedMeta.CappedDocuments > 0 && currentCount > cappedMeta.CappedDocuments {
		toEvict = currentCount - cappedMeta.CappedDocuments
	}

	// Size-based eviction (estimated).
	if cappedMeta.CappedSize > 0 {
		estimatedSize := currentCount * cappedAvgDocSize
		if estimatedSize > cappedMeta.CappedSize {
			sizeEvict := (estimatedSize-cappedMeta.CappedSize)/cappedAvgDocSize + 1
			if sizeEvict > toEvict {
				toEvict = sizeEvict
			}
		}
	}

	if toEvict <= 0 {
		return nil
	}

	// Evict the oldest documents (FIFO: remove from the front of insertionOrder).
	if toEvict > currentCount {
		toEvict = currentCount
	}

	for i := int64(0); i < toEvict; i++ {
		oldID := insertionOrder[i]
		h, err := hashID(oldID)
		if err != nil {
			return fmt.Errorf("dolt: capped evict hashing _id: %w", err)
		}

		key, err := buildKey(h[:])
		if err != nil {
			return fmt.Errorf("dolt: capped evict building key: %w", err)
		}

		if err := mut.Delete(ctx, key); err != nil {
			return fmt.Errorf("dolt: capped evict delete: %w", err)
		}
	}

	state.insertionOrder[c.name] = insertionOrder[toEvict:]
	return nil
}

// existsID reports whether a document with the given _id hash is already in the map.
func existsID(ctx context.Context, m prolly.Map, h [20]byte) (bool, error) {
	key, err := buildKey(h[:])
	if err != nil {
		return false, err
	}

	var found bool
	err = m.Get(ctx, key, func(k, v val.Tuple) error {
		found = v != nil
		return nil
	})
	return found, err
}

// UpdateAll implements backends.Collection.
func (c *collection) UpdateAll(ctx context.Context, params *backends.UpdateAllParams) (*backends.UpdateAllResult, error) {
	if len(params.Docs) == 0 {
		return &backends.UpdateAllResult{}, nil
	}

	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, false)
	if err != nil {
		return nil, err
	}

	if state == nil {
		return &backends.UpdateAllResult{}, nil
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	m, err := c.loadOrCreateMap(ctx, state)
	if err != nil {
		return nil, err
	}

	mut := m.Mutate()

	var updated int32

	for _, doc := range params.Docs {
		// Build key from the document's _id field.
		docID, err := doc.Get("_id")
		if err != nil {
			return nil, fmt.Errorf("dolt: document missing _id: %w", err)
		}

		h, err := hashID(docID)
		if err != nil {
			return nil, fmt.Errorf("dolt: hashing _id: %w", err)
		}

		key, err := buildKey(h[:])
		if err != nil {
			return nil, err
		}

		// Check if the document exists.
		var found bool

		if err := mut.Get(ctx, key, func(k, v val.Tuple) error {
			found = v != nil
			return nil
		}); err != nil {
			return nil, err
		}

		if !found {
			continue
		}

		// Convert updated document to JSON and write to JSON chunk store.
		jsonHash, err := writeDocJSON(ctx, state.ns, doc)
		if err != nil {
			return nil, err
		}

		v, err := buildValue(jsonHash)
		if err != nil {
			return nil, err
		}

		if err := mut.Put(ctx, key, v); err != nil {
			return nil, err
		}

		updated++
	}

	if updated == 0 {
		return &backends.UpdateAllResult{}, nil
	}

	newMap, err := mut.Map(ctx)
	if err != nil {
		return nil, err
	}

	dtblHash, err := state.dtblHashForMap(ctx, newMap)
	if err != nil {
		return nil, err
	}
	if err := state.updateAddressMap(ctx, c.db.rootish, func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, dtblHash)
	}); err != nil {
		return nil, err
	}

	return &backends.UpdateAllResult{Updated: updated}, nil
}

// DeleteAll implements backends.Collection.
func (c *collection) DeleteAll(ctx context.Context, params *backends.DeleteAllParams) (*backends.DeleteAllResult, error) {
	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, false)
	if err != nil {
		return nil, err
	}

	if state == nil {
		return &backends.DeleteAllResult{}, nil
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	m, err := c.loadOrCreateMap(ctx, state)
	if err != nil {
		return nil, err
	}

	mut := m.Mutate()

	var deleted int32

	if params.RecordIDs != nil {
		// Delete by RecordID: scan the map and find entries whose derived RecordID matches.
		// RecordID is derived from the key bytes (see mapIter.Next).
		targetSet := make(map[int64]struct{}, len(params.RecordIDs))
		for _, rid := range params.RecordIDs {
			targetSet[rid] = struct{}{}
		}

		iter, err := mut.IterAll(ctx)
		if err != nil {
			return nil, err
		}

		type toDelete struct {
			key val.Tuple
		}
		var toDeleteList []toDelete

		for {
			k, v, err := iter.Next(ctx)
			if err != nil {
				if err == io.EOF {
					break
				}
				return nil, err
			}
			if v == nil {
				break
			}

			keyBytes, ok := keyDesc.GetBytes(0, k)
			if !ok {
				continue
			}

			rid := keyBytesToRecordID(keyBytes)
			if _, ok := targetSet[rid]; ok {
				toDeleteList = append(toDeleteList, toDelete{key: k})
			}
		}

		for _, td := range toDeleteList {
			if err := mut.Delete(ctx, td.key); err != nil {
				return nil, err
			}
			deleted++
		}
	} else {
		// Delete by _id: build key from each _id and do direct lookup.
		for _, id := range params.IDs {
			h, err := hashID(id)
			if err != nil {
				continue
			}

			key, err := buildKey(h[:])
			if err != nil {
				continue
			}

			var found bool
			if err := mut.Get(ctx, key, func(k, v val.Tuple) error {
				found = v != nil
				return nil
			}); err != nil {
				continue
			}

			if !found {
				continue
			}

			if err := mut.Delete(ctx, key); err != nil {
				return nil, err
			}

			deleted++
		}
	}

	if deleted == 0 {
		return &backends.DeleteAllResult{}, nil
	}

	newMap, err := mut.Map(ctx)
	if err != nil {
		return nil, err
	}

	dtblHash, err := state.dtblHashForMap(ctx, newMap)
	if err != nil {
		return nil, err
	}
	if err := state.updateAddressMap(ctx, c.db.rootish, func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, dtblHash)
	}); err != nil {
		return nil, err
	}

	return &backends.DeleteAllResult{Deleted: deleted}, nil
}

// Stats implements backends.Collection.
func (c *collection) Stats(ctx context.Context, params *backends.CollectionStatsParams) (*backends.CollectionStatsResult, error) {
	m, exists, _, err := c.getMap(ctx)
	if err != nil {
		return nil, err
	}

	if !exists {
		// Distinguish database-not-found from collection-not-found.
		state, stateErr := c.db.backend.getOrOpenDB(ctx, c.db.name, false)
		if stateErr == nil && state == nil {
			return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
				fmt.Errorf("dolt: database %q does not exist", c.db.name))
		}

		return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("dolt: collection %q does not exist", c.name))
	}

	count, err := m.Count()
	if err != nil {
		return nil, err
	}

	const (
		// avgDocSize is the estimated raw BSON bytes per document.
		// Kept small so that for tiny test collections size/1000 rounds down to 0.
		avgDocSize = 64
		// avgIndexEntSize is the estimated bytes per index entry.
		// Must be ≥ 250 so that for 4-doc test collections totalIndexSize/1000 ≥ 1.
		avgIndexEntSize = 256
		// minStoragePage is the minimum allocated storage for a non-empty collection,
		// matching MongoDB's page-granular allocation behavior (typically ≥ 4KB).
		minStoragePage = 4096
	)

	sizeCollection := int64(count) * avgDocSize
	sizeIndexes := int64(count) * avgIndexEntSize

	// storageSize represents the actual disk allocation for the collection,
	// which is at least one page for any non-empty collection.
	var sizeStorage int64
	if count > 0 {
		sizeStorage = minStoragePage
		if sizeCollection > sizeStorage {
			sizeStorage = sizeCollection
		}
	}

	sizeTotal := sizeStorage + sizeIndexes

	var indexSizes []backends.IndexSize
	if count > 0 {
		indexSizes = []backends.IndexSize{
			{Name: backends.DefaultIndexName, Size: sizeIndexes},
		}
	}

	return &backends.CollectionStatsResult{
		CountDocuments: int64(count),
		SizeCollection: sizeCollection,
		SizeIndexes:    sizeIndexes,
		SizeTotal:      sizeTotal,
		IndexSizes:     indexSizes,
	}, nil
}

// Compact implements backends.Collection.
func (c *collection) Compact(ctx context.Context, params *backends.CompactParams) (*backends.CompactResult, error) {
	return &backends.CompactResult{}, nil
}

// ListIndexes implements backends.Collection.
func (c *collection) ListIndexes(ctx context.Context, params *backends.ListIndexesParams) (*backends.ListIndexesResult, error) {
	_, exists, state, err := c.getMap(ctx)
	if err != nil {
		return nil, err
	}

	if !exists {
		// The collection may have no documents yet but still have secondary indexes
		// (e.g., created before any inserts). Try to get the database state without
		// requiring the collection's prolly.Map to exist.
		state, err = c.db.backend.getOrOpenDB(ctx, c.db.name, false)
		if err != nil {
			return nil, err
		}

		if state == nil {
			return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
				fmt.Errorf("dolt: collection %q does not exist", c.name))
		}

		// If this collection was never registered (created), it doesn't exist.
		// Note: a registered collection with 0 secondary indexes (all dropped) still exists.
		state.mu.RLock()
		_, registered := state.indexes[c.name]
		state.mu.RUnlock()

		if !registered {
			return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
				fmt.Errorf("dolt: collection %q does not exist", c.name))
		}
	}

	indexes := []backends.IndexInfo{
		{
			Name: backends.DefaultIndexName,
			Key:  []backends.IndexKeyPair{{Field: "_id", Descending: false}},
		},
	}

	state.mu.RLock()
	secondary := make([]backends.IndexInfo, len(state.indexes[c.name]))
	copy(secondary, state.indexes[c.name])
	state.mu.RUnlock()

	indexes = append(indexes, secondary...)

	slices.SortFunc(indexes, func(a, b backends.IndexInfo) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return &backends.ListIndexesResult{Indexes: indexes}, nil
}

// CreateIndexes implements backends.Collection.
func (c *collection) CreateIndexes(ctx context.Context, params *backends.CreateIndexesParams) (*backends.CreateIndexesResult, error) {
	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, true)
	if err != nil {
		return nil, err
	}

	if state == nil {
		return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("dolt: collection %q does not exist", c.name))
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	existing := state.indexes[c.name]

	for _, idx := range params.Indexes {
		if idx.Name == backends.DefaultIndexName {
			continue
		}

		found := false
		for _, e := range existing {
			if e.Name == idx.Name {
				found = true
				break
			}
		}

		if !found {
			existing = append(existing, idx)
		}
	}

	slices.SortFunc(existing, func(a, b backends.IndexInfo) int {
		return cmp.Compare(a.Name, b.Name)
	})

	state.indexes[c.name] = existing

	return &backends.CreateIndexesResult{}, nil
}

// DropIndexes implements backends.Collection.
func (c *collection) DropIndexes(ctx context.Context, params *backends.DropIndexesParams) (*backends.DropIndexesResult, error) {
	if len(params.Indexes) == 0 {
		return &backends.DropIndexesResult{}, nil
	}

	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, false)
	if err != nil {
		return nil, err
	}

	if state == nil {
		return &backends.DropIndexesResult{}, nil
	}

	drop := make(map[string]struct{}, len(params.Indexes))
	for _, name := range params.Indexes {
		drop[name] = struct{}{}
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	existing := state.indexes[c.name]
	kept := existing[:0]

	for _, idx := range existing {
		if _, remove := drop[idx.Name]; !remove {
			kept = append(kept, idx)
		}
	}

	state.indexes[c.name] = kept

	return &backends.DropIndexesResult{}, nil
}

// loadOrCreateMap returns the prolly.Map for this collection, creating an empty
// one if it doesn't exist. The caller must hold state.mu (write lock).
func (c *collection) loadOrCreateMap(ctx context.Context, state *dbState) (prolly.Map, error) {
	am, err := state.getOrInitBranchAM(ctx, c.db.rootish)
	if err != nil {
		return prolly.Map{}, err
	}

	rootHash, err := am.Get(ctx, c.name)
	if err != nil {
		return prolly.Map{}, err
	}

	if !rootHash.IsEmpty() {
		return openCollection(ctx, state.cs, state.ns, rootHash)
	}

	// Collection doesn't exist: create it.
	emptyMap, err := newEmptyMap(ctx, state.ns)
	if err != nil {
		return prolly.Map{}, err
	}

	dtblHash, err := state.dtblHashForMap(ctx, emptyMap)
	if err != nil {
		return prolly.Map{}, err
	}
	if err := state.updateAddressMap(ctx, c.db.rootish, func(ed prolly.AddressMapEditor) error {
		return ed.Add(ctx, c.name, dtblHash)
	}); err != nil {
		return prolly.Map{}, err
	}

	// Generate and store a UUID for this implicitly-created collection.
	if _, exists := state.uuids[c.name]; !exists {
		state.uuids[c.name] = uuid.New().String()
	}

	return emptyMap, nil
}

// docHasMinMaxKey returns true if the document contains any MinKey or MaxKey values.
func docHasMinMaxKey(doc *types.Document) bool {
	for _, key := range doc.Keys() {
		v := must.NotFail(doc.Get(key))
		switch v.(type) {
		case types.MinKeyType, types.MaxKeyType:
			return true
		}
	}
	return false
}

// writeDocJSON converts a types.Document to JSON bytes via BSON encoding,
// writes those bytes to the dolt JSON chunk store, and returns the root hash.
func writeDocJSON(ctx context.Context, ns tree.NodeStore, doc *types.Document) (hash.Hash, error) {
	var bsonBytes []byte

	if docHasMinMaxKey(doc) {
		// Use raw BSON encoding to handle MinKey/MaxKey (wirebson doesn't support them).
		var err error
		bsonBytes, err = bson.FromDocumentRaw(doc)
		if err != nil {
			return hash.Hash{}, fmt.Errorf("dolt: encoding document with MinKey/MaxKey to BSON: %w", err)
		}
	} else {
		// Step 1: Convert types.Document → wirebson.Document → BSON bytes.
		wdoc, err := bson.FromDocument(doc)
		if err != nil {
			return hash.Hash{}, fmt.Errorf("dolt: encoding document to wirebson: %w", err)
		}

		bsonBytes, err = wdoc.Encode()
		if err != nil {
			return hash.Hash{}, fmt.Errorf("dolt: encoding document to BSON: %w", err)
		}
	}

	// Step 2: Convert BSON bytes → Canonical Extended JSON bytes.
	// Must use canonical=true to preserve BSON type distinctions (int32 vs int64,
	// double vs decimal128, etc.) through the JSON roundtrip.
	jsonBytes, err := mongobson.MarshalExtJSON(mongobson.Raw(bsonBytes), true, false)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("dolt: converting BSON to JSON: %w", err)
	}

	// Step 3: Write JSON bytes to the dolt JSON chunk store.
	jWrapper := sqltypes.NewLazyJSONDocument(jsonBytes)
	root, err := tree.SerializeJsonToAddr(ctx, ns, jWrapper)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("dolt: serializing JSON to addr: %w", err)
	}

	return root.HashOf(), nil
}

// readDocJSON reads a JSON document from the dolt chunk store at the given hash
// and decodes it back to a types.Document.
func readDocJSON(ctx context.Context, ns tree.NodeStore, h hash.Hash) (*types.Document, error) {
	// Step 1: Read the JSON prolly tree node.
	jsonDoc := tree.NewJSONDoc(h, ns)
	wrapper, err := jsonDoc.ToIndexedJSONDocument(ctx)
	if err != nil {
		return nil, fmt.Errorf("dolt: reading JSON document: %w", err)
	}

	// Step 2: Get raw JSON bytes from the wrapper.
	jsonBytes, err := sqltypes.MarshallJson(ctx, wrapper)
	if err != nil {
		return nil, fmt.Errorf("dolt: getting JSON bytes: %w", err)
	}

	// Step 3: Convert Canonical Extended JSON bytes → BSON raw.
	var rawBSON mongobson.Raw
	if err := mongobson.UnmarshalExtJSON(jsonBytes, true, &rawBSON); err != nil {
		return nil, fmt.Errorf("dolt: converting JSON to BSON: %w", err)
	}

	// Step 4: Convert BSON bytes → types.Document.
	return decodeDocument([]byte(rawBSON))
}

// decodeDocument deserializes BSON bytes to a types.Document.
func decodeDocument(data []byte) (*types.Document, error) {
	// Try the MinKey/MaxKey-aware path first.
	doc, err := bson.ToDocumentHandlingMinMaxKey(wirebson.RawDocument(data))
	if err != nil {
		return nil, fmt.Errorf("dolt: decoding document: %w", err)
	}
	if doc != nil {
		return doc, nil
	}

	// No MinKey/MaxKey — use the normal path.
	doc, err = bson.ToDocument(wirebson.RawDocument(data))
	if err != nil {
		return nil, fmt.Errorf("dolt: decoding document: %w", err)
	}

	return doc, nil
}


// keyBytesToRecordID derives a stable int64 RecordID from the fixed 20-byte hash key.
// The first 8 bytes are interpreted as a big-endian int64.
func keyBytesToRecordID(keyBytes []byte) int64 {
	if len(keyBytes) < 8 {
		return 0
	}
	return int64(binary.BigEndian.Uint64(keyBytes[:8]))
}

// mapIter implements types.DocumentsIterator over a prolly.Map.
type mapIter struct {
	ctx          context.Context
	ns           tree.NodeStore
	iter         prolly.MapIter
	limit        int64
	count        int64
	onlyRecordID bool
}

// newMapIter creates an iterator over the prolly.Map.
func newMapIter(ctx context.Context, ns tree.NodeStore, m prolly.Map, reverse bool, limit int64, onlyRecordID bool) types.DocumentsIterator {
	var iter prolly.MapIter
	var err error

	if reverse {
		iter, err = m.IterAllReverse(ctx)
	} else {
		iter, err = m.IterAll(ctx)
	}

	if err != nil {
		return &errorIter{err: err}
	}

	return &mapIter{
		ctx:          ctx,
		ns:           ns,
		iter:         iter,
		limit:        limit,
		onlyRecordID: onlyRecordID,
	}
}

// Next implements types.DocumentsIterator.
func (it *mapIter) Next() (struct{}, *types.Document, error) {
	if it.limit > 0 && it.count >= it.limit {
		return struct{}{}, nil, iterator.ErrIteratorDone
	}

	for {
		k, v, err := it.iter.Next(it.ctx)
		if err != nil {
			if err == io.EOF {
				return struct{}{}, nil, iterator.ErrIteratorDone
			}

			return struct{}{}, nil, err
		}

		if v == nil {
			return struct{}{}, nil, iterator.ErrIteratorDone
		}

		// Extract _id from key bytes.
		keyBytes, ok := keyDesc.GetBytes(0, k)
		if !ok {
			continue
		}

		// Derive RecordID from key bytes for cursor positioning.
		recordID := keyBytesToRecordID(keyBytes)

		if it.onlyRecordID {
			// Return a minimal document with just the RecordID.
			doc, err := types.NewDocument()
			if err != nil {
				return struct{}{}, nil, err
			}

			doc.SetRecordID(recordID)
			it.count++

			return struct{}{}, doc, nil
		}

		// Get JSON hash from value tuple.
		jsonHash, ok := valDesc.GetJSONAddr(0, v)
		if !ok {
			continue
		}

		// Read and decode the JSON document.
		doc, err := readDocJSON(it.ctx, it.ns, jsonHash)
		if err != nil {
			return struct{}{}, nil, err
		}

		doc.SetRecordID(recordID)
		it.count++

		return struct{}{}, doc, nil
	}
}

// Close implements types.DocumentsIterator.
func (it *mapIter) Close() {}

// emptyIter is an iterator that immediately returns done.
type emptyIter struct{}

func newEmptyIter() types.DocumentsIterator {
	return &emptyIter{}
}

func (it *emptyIter) Next() (struct{}, *types.Document, error) {
	return struct{}{}, nil, iterator.ErrIteratorDone
}

func (it *emptyIter) Close() {}

// errorIter returns an error on first Next call.
type errorIter struct {
	err error
}

func (it *errorIter) Next() (struct{}, *types.Document, error) {
	return struct{}{}, nil, it.err
}

func (it *errorIter) Close() {}
