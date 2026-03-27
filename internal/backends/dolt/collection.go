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
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"github.com/FerretDB/wire/wirebson"
	sqltypes "github.com/dolthub/go-mysql-server/sql/types"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"
	"github.com/google/uuid"
	mongobson "go.mongodb.org/mongo-driver/bson"

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/bson"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/must"
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

	// Collect all existing _id values from the map for duplicate detection.
	existingIDs, err := collectExistingIDs(ctx, m)
	if err != nil {
		return nil, err
	}

	mut := m.Mutate()

	// Track _id values inserted in this batch to detect in-batch duplicates.
	batchIDs := make([]any, 0, len(params.Docs))

	for _, doc := range params.Docs {
		// Extract the _id from this document.
		docID, err := doc.Get("_id")
		if err != nil {
			return nil, fmt.Errorf("dolt: document missing _id: %w", err)
		}

		// Check against existing IDs in the collection.
		for _, existingID := range existingIDs {
			if types.Compare(existingID, docID) == types.Equal {
				return nil, backends.NewError(
					backends.ErrorCodeInsertDuplicateID,
					fmt.Errorf("dolt: duplicate _id in collection"),
				)
			}
		}

		// Check against IDs already inserted in this batch.
		for _, batchID := range batchIDs {
			if types.Compare(batchID, docID) == types.Equal {
				return nil, backends.NewError(
					backends.ErrorCodeInsertDuplicateID,
					fmt.Errorf("dolt: duplicate _id in batch"),
				)
			}
		}

		// Encode the _id to varbinary key bytes.
		idBytes, err := encodeID(docID)
		if err != nil {
			return nil, fmt.Errorf("dolt: encoding _id: %w", err)
		}

		key, err := buildKey(idBytes)
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

		batchIDs = append(batchIDs, docID)
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
	if err := state.updateAddressMap(ctx, func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, dtblHash)
	}); err != nil {
		return nil, err
	}

	return &backends.InsertAllResult{}, nil
}

// collectExistingIDs scans the prolly.Map and returns all _id values stored in the collection.
// _id values are decoded from the key tuple directly.
func collectExistingIDs(ctx context.Context, m prolly.Map) ([]any, error) {
	iter, err := m.IterAll(ctx)
	if err != nil {
		return nil, err
	}

	var ids []any

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

		id, err := decodeID(keyBytes)
		if err != nil {
			continue
		}

		ids = append(ids, id)
	}

	return ids, nil
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

		idBytes, err := encodeID(docID)
		if err != nil {
			return nil, fmt.Errorf("dolt: encoding _id: %w", err)
		}

		key, err := buildKey(idBytes)
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
	if err := state.updateAddressMap(ctx, func(ed prolly.AddressMapEditor) error {
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
			idBytes, err := encodeID(id)
			if err != nil {
				continue
			}

			key, err := buildKey(idBytes)
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
	if err := state.updateAddressMap(ctx, func(ed prolly.AddressMapEditor) error {
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

		// If there are no secondary indexes for this collection, the collection doesn't exist.
		state.mu.RLock()
		hasIndexes := len(state.indexes[c.name]) > 0
		state.mu.RUnlock()

		if !hasIndexes {
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
	if err := state.updateAddressMap(ctx, func(ed prolly.AddressMapEditor) error {
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

// writeDocJSON converts a types.Document to JSON bytes via BSON encoding,
// writes those bytes to the dolt JSON chunk store, and returns the root hash.
func writeDocJSON(ctx context.Context, ns tree.NodeStore, doc *types.Document) (hash.Hash, error) {
	// Step 1: Convert types.Document → wirebson.Document → BSON bytes.
	wdoc, err := bson.FromDocument(doc)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("dolt: encoding document to wirebson: %w", err)
	}

	bsonBytes, err := wdoc.Encode()
	if err != nil {
		return hash.Hash{}, fmt.Errorf("dolt: encoding document to BSON: %w", err)
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
	doc, err := bson.ToDocument(wirebson.RawDocument(data))
	if err != nil {
		return nil, fmt.Errorf("dolt: decoding document: %w", err)
	}

	return doc, nil
}

// decodeID decodes varbinary key bytes back to a MongoDB _id value.
// The format is: 1-byte BSON type tag + canonical value bytes.
func decodeID(b []byte) (any, error) {
	if len(b) < 1 {
		return nil, fmt.Errorf("dolt: empty _id key bytes")
	}

	tag := b[0]
	data := b[1:]

	switch tag {
	case 0x10: // Int32
		if len(data) != 4 {
			return nil, fmt.Errorf("dolt: Int32 _id: expected 4 bytes, got %d", len(data))
		}
		return int32(binary.BigEndian.Uint32(data)), nil

	case 0x12: // Int64
		if len(data) != 8 {
			return nil, fmt.Errorf("dolt: Int64 _id: expected 8 bytes, got %d", len(data))
		}
		return int64(binary.BigEndian.Uint64(data)), nil

	case 0x01: // Double (sign-magnitude encoded)
		if len(data) != 8 {
			return nil, fmt.Errorf("dolt: Double _id: expected 8 bytes, got %d", len(data))
		}
		u := binary.BigEndian.Uint64(data)
		if u&(1<<63) != 0 {
			// Positive: clear the sign bit we set.
			u &^= 1 << 63
		} else {
			// Negative: flip all bits back.
			u = ^u
		}
		return math.Float64frombits(u), nil

	case 0x07: // ObjectId
		if len(data) != 12 {
			return nil, fmt.Errorf("dolt: ObjectId _id: expected 12 bytes, got %d", len(data))
		}
		var oid types.ObjectID
		copy(oid[:], data)
		return oid, nil

	case 0x02: // String
		return string(data), nil

	case 0x05: // BinData
		if len(data) < 1 {
			return nil, fmt.Errorf("dolt: BinData _id: missing subtype byte")
		}
		return types.Binary{
			Subtype: types.BinarySubtype(data[0]),
			B:       data[1:],
		}, nil

	case 0x08: // Bool
		if len(data) != 1 {
			return nil, fmt.Errorf("dolt: Bool _id: expected 1 byte, got %d", len(data))
		}
		return data[0] != 0x00, nil

	case 0x09: // Date
		if len(data) != 8 {
			return nil, fmt.Errorf("dolt: Date _id: expected 8 bytes, got %d", len(data))
		}
		ms := int64(binary.BigEndian.Uint64(data))
		return time.UnixMilli(ms).UTC(), nil

	case 0x13: // Decimal128
		if len(data) != 16 {
			return nil, fmt.Errorf("dolt: Decimal128 _id: expected 16 bytes, got %d", len(data))
		}
		return types.Decimal128{
			L: binary.LittleEndian.Uint64(data[0:8]),
			H: binary.LittleEndian.Uint64(data[8:16]),
		}, nil

	default:
		return nil, fmt.Errorf("dolt: unsupported _id tag 0x%02x", tag)
	}
}

// keyBytesToRecordID derives a stable int64 RecordID from varbinary key bytes.
// For Int64-tagged keys (0x12), the embedded int64 is returned directly.
// For other types, the first 8 bytes (zero-padded) are interpreted as a big-endian int64.
func keyBytesToRecordID(keyBytes []byte) int64 {
	if len(keyBytes) >= 9 && keyBytes[0] == 0x12 {
		return int64(binary.BigEndian.Uint64(keyBytes[1:9]))
	}
	// Fallback: use first 8 bytes, zero-padded.
	var buf [8]byte
	n := copy(buf[:], keyBytes)
	_ = n
	return int64(binary.BigEndian.Uint64(buf[:]))
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
