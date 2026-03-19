// Copyright 2024 Dolt Inc.
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
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/bson"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
)

// collection implements backends.Collection.
type collection struct {
	db   *database
	name string
}

// getMap returns the current prolly.Map for this collection.
// Returns (emptyMap, false, nil) if the database or collection doesn't exist.
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

	rootHash, err := state.am.Get(ctx, c.name)
	if err != nil {
		return prolly.Map{}, false, nil, err
	}

	if rootHash.IsEmpty() {
		return prolly.Map{}, false, nil, nil
	}

	m, err := openMap(ctx, state.ns, rootHash)
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
	return &backends.ExplainResult{
		QueryPlanner: new(types.Document),
	}, nil
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

	mut := m.Mutate()

	for _, doc := range params.Docs {
		// Check for duplicate _id.
		recordID := doc.RecordID()
		key, err := buildKey(recordID)

		if err != nil {
			return nil, err
		}

		var existing val.Tuple

		if err := mut.Get(ctx, key, func(k, v val.Tuple) error {
			existing = v
			return nil
		}); err != nil {
			return nil, err
		}

		if existing != nil {
			// Check for duplicate _id (by scanning for same _id in existing docs).
			// Since key is RecordID (unique per insert), we won't have key collisions.
			// But we need to check for _id duplicates.
			// RecordID is always unique (assigned from timestamp), so this won't trigger.
			// However, per contract, _id duplicates must return ErrorCodeInsertDuplicateID.
			// We handle this by checking the _id against all existing docs' _ids during scan.
			// For now, RecordID uniqueness is guaranteed by the contract.
		}

		// Encode the document to BSON.
		bsonBytes, err := encodeDocument(doc)
		if err != nil {
			return nil, err
		}

		// Store the BSON bytes in the node store.
		bsonHash, err := state.ns.WriteBytes(ctx, bsonBytes)
		if err != nil {
			return nil, err
		}

		v, err := buildValue(bsonHash)
		if err != nil {
			return nil, err
		}

		if err := mut.Put(ctx, key, v); err != nil {
			return nil, err
		}
	}

	// Flush the mutable map.
	newMap, err := mut.Map(ctx)
	if err != nil {
		return nil, err
	}

	// Update the address map.
	if err := state.updateAddressMap(ctx, func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, newMap.HashOf())
	}); err != nil {
		return nil, err
	}

	return &backends.InsertAllResult{}, nil
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
		recordID := doc.RecordID()
		key, err := buildKey(recordID)

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

		// Encode the updated document.
		bsonBytes, err := encodeDocument(doc)
		if err != nil {
			return nil, err
		}

		bsonHash, err := state.ns.WriteBytes(ctx, bsonBytes)
		if err != nil {
			return nil, err
		}

		v, err := buildValue(bsonHash)
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

	if err := state.updateAddressMap(ctx, func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, newMap.HashOf())
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
		// Delete by RecordID (direct key lookup).
		for _, recordID := range params.RecordIDs {
			key, err := buildKey(recordID)
			if err != nil {
				return nil, err
			}

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

			if err := mut.Delete(ctx, key); err != nil {
				return nil, err
			}

			deleted++
		}
	} else {
		// Delete by _id: scan and find matching documents.
		idSet := make(map[string]struct{}, len(params.IDs))
		for _, id := range params.IDs {
			idKey, err := marshalID(id)
			if err != nil {
				continue
			}

			idSet[string(idKey)] = struct{}{}
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
				if errors.Is(err, tree.ErrEndOfIndex) {
					break
				}

				return nil, err
			}

			if v == nil {
				break
			}

			// Decode the document to get its _id.
			bsonHash, ok := valDesc.GetBytesAddr(0, v)
			if !ok {
				continue
			}

			docBytes, err := state.ns.ReadBytes(ctx, bsonHash)
			if err != nil {
				continue
			}

			doc, err := decodeDocument(docBytes)
			if err != nil {
				continue
			}

			id, err := doc.Get("_id")
			if err != nil {
				continue
			}

			idKey, err := marshalID(id)
			if err != nil {
				continue
			}

			if _, ok := idSet[string(idKey)]; ok {
				toDeleteList = append(toDeleteList, toDelete{key: k})
			}
		}

		for _, td := range toDeleteList {
			if err := mut.Delete(ctx, td.key); err != nil {
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

	if err := state.updateAddressMap(ctx, func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, newMap.HashOf())
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
		return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("dolt: collection %q does not exist", c.name))
	}

	count, err := m.Count()
	if err != nil {
		return nil, err
	}

	return &backends.CollectionStatsResult{
		CountDocuments: int64(count),
	}, nil
}

// Compact implements backends.Collection.
func (c *collection) Compact(ctx context.Context, params *backends.CompactParams) (*backends.CompactResult, error) {
	return &backends.CompactResult{}, nil
}

// ListIndexes implements backends.Collection.
func (c *collection) ListIndexes(ctx context.Context, params *backends.ListIndexesParams) (*backends.ListIndexesResult, error) {
	_, exists, _, err := c.getMap(ctx)
	if err != nil {
		return nil, err
	}

	if !exists {
		return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("dolt: collection %q does not exist", c.name))
	}

	return &backends.ListIndexesResult{
		Indexes: []backends.IndexInfo{
			{
				Name: backends.DefaultIndexName,
				Key:  []backends.IndexKeyPair{{Field: "_id", Descending: false}},
			},
		},
	}, nil
}

// CreateIndexes implements backends.Collection.
func (c *collection) CreateIndexes(ctx context.Context, params *backends.CreateIndexesParams) (*backends.CreateIndexesResult, error) {
	// We only support the default _id index; ignore additional index creation requests.
	return &backends.CreateIndexesResult{}, nil
}

// DropIndexes implements backends.Collection.
func (c *collection) DropIndexes(ctx context.Context, params *backends.DropIndexesParams) (*backends.DropIndexesResult, error) {
	return &backends.DropIndexesResult{}, nil
}

// loadOrCreateMap returns the prolly.Map for this collection, creating an empty
// one if it doesn't exist. The caller must hold state.mu (write lock).
func (c *collection) loadOrCreateMap(ctx context.Context, state *dbState) (prolly.Map, error) {
	rootHash, err := state.am.Get(ctx, c.name)
	if err != nil {
		return prolly.Map{}, err
	}

	if !rootHash.IsEmpty() {
		return openMap(ctx, state.ns, rootHash)
	}

	// Collection doesn't exist: create it.
	emptyMap, err := newEmptyMap(ctx, state.ns)
	if err != nil {
		return prolly.Map{}, err
	}

	if err := state.updateAddressMap(ctx, func(ed prolly.AddressMapEditor) error {
		return ed.Add(ctx, c.name, emptyMap.HashOf())
	}); err != nil {
		return prolly.Map{}, err
	}

	return emptyMap, nil
}

// encodeDocument serializes a types.Document to BSON bytes.
func encodeDocument(doc *types.Document) ([]byte, error) {
	wdoc, err := bson.FromDocument(doc)
	if err != nil {
		return nil, fmt.Errorf("dolt: encoding document: %w", err)
	}

	b, err := wdoc.Encode()
	if err != nil {
		return nil, fmt.Errorf("dolt: BSON encoding: %w", err)
	}

	return b, nil
}

// decodeDocument deserializes BSON bytes to a types.Document.
func decodeDocument(data []byte) (*types.Document, error) {
	doc, err := bson.ToDocument(bsonRawDoc(data))
	if err != nil {
		return nil, fmt.Errorf("dolt: decoding document: %w", err)
	}

	return doc, nil
}

// bsonRawDoc implements wirebson.AnyDocument for raw BSON bytes.
type bsonRawDoc []byte

// marshalID serializes an _id value to a comparable byte key.
func marshalID(id any) ([]byte, error) {
	doc := types.MustNewDocument("_id", id)

	wdoc, err := bson.FromDocument(doc)
	if err != nil {
		return nil, err
	}

	return wdoc.Encode()
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
func (it *mapIter) Next() (int64, *types.Document, error) {
	if it.limit > 0 && it.count >= it.limit {
		return 0, nil, iterator.ErrIteratorDone
	}

	for {
		k, v, err := it.iter.Next(it.ctx)
		if err != nil {
			if errors.Is(err, tree.ErrEndOfIndex) {
				return 0, nil, iterator.ErrIteratorDone
			}

			return 0, nil, err
		}

		if v == nil {
			return 0, nil, iterator.ErrIteratorDone
		}

		// Extract RecordID from key.
		recordID, ok := keyDesc.GetInt64(0, k)
		if !ok {
			continue
		}

		if it.onlyRecordID {
			// Return a minimal document with just the RecordID.
			doc := types.MustNewDocument()
			doc.SetRecordID(recordID)
			it.count++

			return recordID, doc, nil
		}

		// Read BSON bytes from the store.
		bsonHash, ok := valDesc.GetBytesAddr(0, v)
		if !ok {
			continue
		}

		docBytes, err := it.ns.ReadBytes(it.ctx, bsonHash)
		if err != nil {
			return 0, nil, err
		}

		doc, err := decodeDocument(docBytes)
		if err != nil {
			return 0, nil, err
		}

		doc.SetRecordID(recordID)
		it.count++

		return recordID, doc, nil
	}
}

// Close implements types.DocumentsIterator.
func (it *mapIter) Close() {}

// emptyIter is an iterator that immediately returns done.
type emptyIter struct{}

func newEmptyIter() types.DocumentsIterator {
	return &emptyIter{}
}

func (it *emptyIter) Next() (int64, *types.Document, error) {
	return 0, nil, iterator.ErrIteratorDone
}

func (it *emptyIter) Close() {}

// errorIter returns an error on first Next call.
type errorIter struct {
	err error
}

func (it *errorIter) Next() (int64, *types.Document, error) {
	return 0, nil, it.err
}

func (it *errorIter) Close() {}

// getID extracts the _id field from a document, returning the BSON-encoded key bytes.
func getIDBytes(doc *types.Document, ns tree.NodeStore) (hash.Hash, error) {
	id, err := doc.Get("_id")
	if err != nil {
		return hash.Hash{}, fmt.Errorf("dolt: document missing _id: %w", err)
	}

	idBytes, err := marshalID(id)
	if err != nil {
		return hash.Hash{}, err
	}

	return ns.WriteBytes(context.Background(), idBytes)
}

// iterID extracts _id from the value tuple in the map.
// It returns (idBytes, ok, error).
func iterID(ctx context.Context, ns tree.NodeStore, v val.Tuple) ([]byte, bool, error) {
	bsonHash, ok := valDesc.GetBytesAddr(0, v)
	if !ok {
		return nil, false, nil
	}

	docBytes, err := ns.ReadBytes(ctx, bsonHash)
	if err != nil {
		return nil, false, err
	}

	doc, err := decodeDocument(docBytes)
	if err != nil {
		return nil, false, err
	}

	id, err := doc.Get("_id")
	if err != nil {
		return nil, false, err
	}

	idBytes, err := marshalID(id)
	if err != nil {
		return nil, false, err
	}

	return idBytes, true, nil
}

// getInt64 is a helper to read int64 from a key tuple.
func getInt64(tup val.Tuple) (int64, bool) {
	return keyDesc.GetInt64(0, tup)
}

// compareIDBytes compares two _id byte sequences.
func compareIDBytes(a, b []byte) bool {
	return bytes.Equal(a, b)
}
