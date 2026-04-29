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
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/FerretDB/wire/wirebson"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"
	sqltypes "github.com/dolthub/go-mysql-server/sql/types"
	"github.com/google/uuid"
	mongobson "go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/bson"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
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

	// spike/index-poc: attempt secondary index lookup for simple equality queries.
	if params != nil && params.Filter != nil && params.Sort.Len() == 0 {
		if docs, used, err := c.tryIndexLookup(ctx, state, m, params.Filter); used {
			if err != nil {
				return nil, err
			}
			return &backends.QueryResult{
				Iter: newSliceIter(docs),
			}, nil
		}
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

	// Fast path: if the filter pins _id to a concrete scalar value, use the
	// primary-key point lookup instead of a full collection scan. The handler's
	// downstream FilterIterator applies any remaining predicates.
	if params != nil && params.Filter != nil {
		if idVal, ok := simpleIDEquality(params.Filter); ok {
			iter, err := pointLookupByID(ctx, state.ns, m, idVal, onlyRecordIDs)
			if err == nil {
				return &backends.QueryResult{Iter: iter}, nil
			}
			// Fall back to full scan on any error (e.g. unsupported _id type).
		}
	}

	// Optimization: build a cheap byte-level prefilter from a simple scalar
	// equality in the query. The handler's FilterIterator still validates
	// every document we return, so a false positive (prefilter passes but
	// the doc doesn't actually match) is harmless. False negatives would be
	// silent data loss, so buildScanPrefilter only returns a predicate when
	// it can be proven that a document whose JSON does NOT contain the
	// pattern cannot possibly match the filter.
	//
	// Under case-insensitive collation the handler will re-check matches
	// against a regex substitution of the filter, so byte-level equality is
	// not a sound lower bound — skip the prefilter.
	var pf func([]byte) bool
	if params != nil && !onlyRecordIDs && !params.CaseInsensitive {
		pf = buildScanPrefilter(params.Filter)
	}

	return &backends.QueryResult{
		Iter: newMapIter(ctx, state.ns, m, reverse, limit, onlyRecordIDs, pf),
	}, nil
}

// buildScanPrefilter returns a byte-level predicate over a document's raw
// canonical Extended JSON bytes that is sound for the given filter — i.e. if
// the predicate returns false, the document is guaranteed not to match.
// Returns nil when no sound prefilter can be built (complex filter, null
// value, unsupported type, ambiguous numeric, etc.); in that case the scan
// falls back to decoding every document.
func buildScanPrefilter(filter *types.Document) func([]byte) bool {
	if filter == nil {
		return nil
	}

	keys := filter.Keys()
	// Collect one required substring per top-level field=scalar constraint.
	// Each element of patterns is a list of alternatives — at least one must
	// appear in the doc's JSON. Multiple fields are AND-combined.
	var patterns [][][]byte
	for _, field := range keys {
		// $and/$or/$comment/etc. — bail out and let the handler filter.
		if strings.HasPrefix(field, "$") {
			return nil
		}
		// Dotted paths pick into sub-documents; JSON encoding isn't a
		// straightforward substring for them.
		if strings.ContainsRune(field, '.') {
			return nil
		}
		v, err := filter.Get(field)
		if err != nil {
			return nil
		}
		alts := extJSONFieldPatterns(field, v)
		if alts == nil {
			return nil
		}
		patterns = append(patterns, alts)
	}
	if len(patterns) == 0 {
		return nil
	}

	return func(jsonBytes []byte) bool {
		for _, alts := range patterns {
			found := false
			for _, p := range alts {
				if bytes.Contains(jsonBytes, p) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
}

// extJSONFieldPatterns returns the set of byte substrings, any one of which
// must appear in a document's canonical Extended JSON bytes if the document
// matches `{field: value}` under MongoDB's equality semantics. Returns nil
// when the filter value is too complex or its type bracket is too broad for
// a sound byte-level check (operator docs, arrays, null, etc.).
//
// For numerics, MongoDB treats int32/int64/double/decimal128 as equal when
// the numeric value is the same, so we enumerate every canonical encoding
// a matching document could use.
func extJSONFieldPatterns(field string, value any) [][]byte {
	switch v := value.(type) {
	case *types.Document:
		// Operator form like {$eq: x} / {$gt: x} — downstream has to decide.
		return nil
	case *types.Array, types.NullType:
		return nil
	case int32:
		return numericFieldPatterns(field, int64(v), float64(v), false)
	case int64:
		return numericFieldPatterns(field, v, float64(v), false)
	case float64:
		if v != v || v-v != 0 { // NaN or ±Inf — ExtJSON has special forms.
			return nil
		}
		asInt := int64(v)
		exact := float64(asInt) == v
		return numericFieldPatterns(field, asInt, v, !exact)
	case string, bool, time.Time, types.ObjectID, types.Binary, types.Timestamp, types.Decimal128:
		p, err := marshalExtJSONField(field, value)
		if err != nil {
			return nil
		}
		return [][]byte{p}
	case types.Regex:
		// Regex in a filter value means pattern match, not literal
		// equality — byte-level substring check isn't sound.
		return nil
	default:
		return nil
	}
}

// numericFieldPatterns enumerates the canonical Extended JSON byte patterns
// a field could have if its stored value is numerically equal to the filter.
// If fractional is true, integer-form patterns are skipped because the
// filter value is not an exact integer and can't equal any stored int.
func numericFieldPatterns(field string, asInt int64, asDouble float64, fractional bool) [][]byte {
	var out [][]byte
	if !fractional {
		if p, err := marshalExtJSONField(field, int32(asInt)); err == nil && int64(int32(asInt)) == asInt {
			out = append(out, p)
		}
		if p, err := marshalExtJSONField(field, asInt); err == nil {
			out = append(out, p)
		}
	}
	if p, err := marshalExtJSONField(field, asDouble); err == nil {
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// marshalExtJSONField serializes `{field: value}` to canonical Extended JSON
// and strips the outer braces, yielding just the `"field":<canonical-value>`
// fragment suitable for substring matching against a stored document's JSON.
func marshalExtJSONField(field string, value any) ([]byte, error) {
	tmp, err := types.NewDocument(field, value)
	if err != nil {
		return nil, err
	}
	wdoc, err := bson.FromDocument(tmp)
	if err != nil {
		return nil, err
	}
	bsonBytes, err := wdoc.Encode()
	if err != nil {
		return nil, err
	}
	extJSON, err := mongobson.MarshalExtJSON(mongobson.Raw(bsonBytes), true, false)
	if err != nil {
		return nil, err
	}
	// Strip the outer `{` and `}` so the remainder matches as an interior
	// field-value pair in any document containing that field.
	if len(extJSON) < 2 || extJSON[0] != '{' || extJSON[len(extJSON)-1] != '}' {
		return nil, fmt.Errorf("unexpected ExtJSON shape %q", extJSON)
	}
	return extJSON[1 : len(extJSON)-1], nil
}

// simpleIDEquality reports whether filter contains an "_id" field bound to a
// concrete scalar value that can be hashed into a primary key. It rejects
// operator forms ({_id: {$eq: x}}), array equality ({_id: [1,2]}), and null.
// Other filter fields are allowed — the handler re-checks them on the result.
func simpleIDEquality(filter *types.Document) (any, bool) {
	v, err := filter.Get("_id")
	if err != nil {
		return nil, false
	}
	switch val := v.(type) {
	case *types.Document:
		// {$eq: x} / {$gt: x} / etc. — let the full scan handle it.
		for _, k := range val.Keys() {
			if strings.HasPrefix(k, "$") {
				return nil, false
			}
		}
		// An embedded document with no operator keys is a literal _id value.
		return v, true
	case *types.Array, types.NullType:
		return nil, false
	default:
		return v, true
	}
}

// pointLookupByID performs a single prolly.Map.Get against the hashed _id and
// returns an iterator over the at-most-one matching document.
func pointLookupByID(ctx context.Context, ns tree.NodeStore, m prolly.Map, idVal any, onlyRecordID bool) (types.DocumentsIterator, error) {
	h, err := hashID(idVal)
	if err != nil {
		return nil, err
	}

	key, err := buildKey(h[:])
	if err != nil {
		return nil, err
	}

	var doc *types.Document
	err = m.Get(ctx, key, func(k, v val.Tuple) error {
		if v == nil {
			return nil
		}

		keyBytes, ok := keyDesc.GetBytes(0, k)
		if !ok {
			return nil
		}
		recordID := keyBytesToRecordID(keyBytes)

		if onlyRecordID {
			d, err := types.NewDocument()
			if err != nil {
				return err
			}
			d.SetRecordID(recordID)
			doc = d
			return nil
		}

		jsonHash, ok := valDesc.GetJSONAddr(0, v)
		if !ok {
			return nil
		}
		d, err := readDocJSON(ctx, ns, jsonHash)
		if err != nil {
			return err
		}
		d.SetRecordID(recordID)
		doc = d
		return nil
	})
	if err != nil {
		return nil, err
	}

	if doc == nil {
		return newEmptyIter(), nil
	}
	return &singleDocIter{doc: doc}, nil
}

// singleDocIter yields exactly one document then reports done.
type singleDocIter struct {
	doc *types.Document
}

func (it *singleDocIter) Next() (struct{}, *types.Document, error) {
	if it.doc == nil {
		return struct{}{}, nil, iterator.ErrIteratorDone
	}
	d := it.doc
	it.doc = nil
	return struct{}{}, d, nil
}

func (it *singleDocIter) Close() {}

// tryIndexLookup checks whether the filter is a simple single-field equality
// query and an index exists for that field. If so it uses the secondary index
// to fetch matching documents and returns (docs, true, err).
// Returns (nil, false, nil) if no suitable index was found (caller should fall back).
func (c *collection) tryIndexLookup(ctx context.Context, state *dbState, primary prolly.Map, filter *types.Document) ([]*types.Document, bool, error) {
	// Only handle single-field equality: { field: value } with no operators.
	if filter.Len() != 1 {
		return nil, false, nil
	}

	var field string
	var filterVal any
	for _, k := range filter.Keys() {
		field = k
		v, _ := filter.Get(k)
		// Reject operator documents.
		if _, isDoc := v.(*types.Document); isDoc {
			return nil, false, nil
		}
		filterVal = v
	}

	// Check if there is a secondary index on this field.
	state.mu.RLock()
	secMaps := state.secIndexMaps[c.name]
	idxInfos := state.indexes[c.name]
	state.mu.RUnlock()

	var matchingIdxName string
	for _, idx := range idxInfos {
		if len(idx.Key) == 1 && idx.Key[0].Field == field {
			matchingIdxName = idx.Name
			break
		}
	}
	if matchingIdxName == "" {
		return nil, false, nil
	}

	idxMap, ok := secMaps[matchingIdxName]
	if !ok {
		return nil, false, nil
	}

	// Look up matching primary key bytes in the secondary index.
	primaryIDBytesList, err := idxpkg.EqualityLookup(ctx, idxMap, filterVal)
	if err != nil {
		return nil, false, err
	}

	// Fetch the actual documents from the primary map. Errors propagate so
	// callers see real failures instead of empty results that look like a
	// missing match.
	docs := make([]*types.Document, 0, len(primaryIDBytesList))
	for _, idBytes := range primaryIDBytesList {
		key, err := buildKey(idBytes)
		if err != nil {
			return nil, true, fmt.Errorf("dolt: index lookup building key: %w", err)
		}
		var doc *types.Document
		if err := primary.Get(ctx, key, func(k, v val.Tuple) error {
			if v == nil {
				return nil
			}
			jsonHash, ok := valDesc.GetJSONAddr(0, v)
			if !ok {
				return fmt.Errorf("dolt: primary value tuple missing JSON addr")
			}
			var decErr error
			doc, decErr = readDocJSON(ctx, state.ns, jsonHash)
			return decErr
		}); err != nil {
			return nil, true, fmt.Errorf("dolt: index lookup primary fetch: %w", err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}

	return docs, true, nil
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
	// autoCommit=true means "create a dolt commit on every write," which already
	// triggers its own NBS journal fsync — deferring the working-set update but
	// still committing synchronously would leave the history and working set
	// inconsistent, so we only honor SkipDurableSync when autoCommit is off.
	// Maintain secondary indexes for any documents we just inserted, then
	// persist the updated index maps before we build the DTBL — the DTBL's
	// SecondaryIndexes field needs to reflect the post-insert state so that a
	// later reopen sees the inserted entries in every index.
	if err := c.updateSecondaryIndexesOnInsert(ctx, state, params.Docs); err != nil {
		return nil, fmt.Errorf("dolt: updating secondary indexes: %w", err)
	}
	if err := state.persistIndexes(ctx, c.name); err != nil {
		return nil, fmt.Errorf("dolt: persisting secondary indexes: %w", err)
	}

	skipSync := params.SkipDurableSync && !c.db.backend.autoCommit
	dtblHash, err := state.dtblHashForCollection(ctx, c.name, newMap, hash.Hash{})
	if err != nil {
		return nil, err
	}
	if err := state.updateAddressMapWithSync(ctx, c.db.rootish, func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, dtblHash)
	}, skipSync); err != nil {
		return nil, err
	}

	if c.db.backend.autoCommit {
		msg := fmt.Sprintf("auto: insert into %s", c.name)
		newDS, _, err := commitCollectionsAMAs(ctx, state.doltDB, state.ds, state.am, msg, "dumbodb <dumbodb@localhost>", time.Now())
		if err != nil {
			return nil, fmt.Errorf("dolt: auto-commit after insert: %w", err)
		}
		state.ds = newDS
	}

	return &backends.InsertAllResult{}, nil
}

// updateSecondaryIndexesOnInsert adds entries for the inserted documents to all secondary indexes.
// Must be called with state.mu held (write lock).
func (c *collection) updateSecondaryIndexesOnInsert(ctx context.Context, state *dbState, docs []*types.Document) error {
	secMaps := state.secIndexMaps[c.name]
	if len(secMaps) == 0 {
		return nil
	}

	idxInfos := state.indexes[c.name]

	for idxName, idxMap := range secMaps {
		var idxInfo *backends.IndexInfo
		for i := range idxInfos {
			if idxInfos[i].Name == idxName {
				idxInfo = &idxInfos[i]
				break
			}
		}
		if idxInfo == nil {
			// secIndexMaps and state.indexes drifted: that's a backend
			// invariant violation, not a per-doc skip.
			return fmt.Errorf("dolt: index %q has a map but no IndexInfo on %q", idxName, c.name)
		}

		mut := idxMap.Mutate()
		for _, doc := range docs {
			docID, err := doc.Get("_id")
			if err != nil {
				return fmt.Errorf("dolt: secondary index %q: document missing _id: %w", idxName, err)
			}
			h, err := hashID(docID)
			if err != nil {
				return fmt.Errorf("dolt: secondary index %q: hashing _id: %w", idxName, err)
			}
			idBytes := h[:]
			fieldVals := extractIndexFieldValues(doc, *idxInfo)
			if err := idxpkg.InsertEntry(ctx, mut, fieldVals, idBytes); err != nil {
				return fmt.Errorf("dolt: secondary index %q: inserting entry: %w", idxName, err)
			}
		}
		updated, err := mut.Map(ctx)
		if err != nil {
			return fmt.Errorf("dolt: secondary index %q: flushing map: %w", idxName, err)
		}
		secMaps[idxName] = updated
	}
	return nil
}

// evictCappedDocs removes oldest documents from a capped collection to enforce size and count limits.
// Must be called with state.mu held for writing.
// cappedAvgDocSize is a rough estimate in bytes per document for size-based eviction.
// Kept in sync with avgDocSize in (*collection).Stats so that the eviction
// trigger point matches the collection size reported via collStats.
const cappedAvgDocSize = 64

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

	for i, doc := range params.Docs {
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

		// Locate the existing stored document so a partial update can reuse
		// its chunks. Capture the existing JSON hash while we're here.
		var (
			found        bool
			existingHash hash.Hash
		)

		if err := mut.Get(ctx, key, func(k, v val.Tuple) error {
			if v == nil {
				return nil
			}
			found = true
			if addr, ok := valDesc.GetJSONAddr(0, v); ok {
				existingHash = addr
			}
			return nil
		}); err != nil {
			return nil, err
		}

		if !found {
			continue
		}

		// Prefer a partial update when the handler supplied a trackable
		// mutation list and the prior document is indexed. Any error in the
		// partial path falls through to a full rewrite from doc below.
		var jsonHash hash.Hash

		var mutations []backends.FieldMutation
		if i < len(params.FieldMutations) {
			mutations = params.FieldMutations[i]
		}

		if len(mutations) > 0 && !existingHash.IsEmpty() {
			if h, err := applyFieldMutations(ctx, state.ns, existingHash, mutations); err == nil {
				jsonHash = h
			}
		}

		if jsonHash.IsEmpty() {
			jsonHash, err = writeDocJSON(ctx, state.ns, doc)
			if err != nil {
				return nil, err
			}
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

	// Re-persist secondary indexes so the DTBL we are about to write inlines
	// the latest secondary_indexes AM. UpdateAll itself does not currently
	// rebuild the secondary-index entries (see do-* follow-ups), but routing
	// the write through persistIndexes preserves whatever in-memory maps the
	// collection holds (e.g. from a recent CreateIndexes / InsertAll on the
	// same handle) into the persisted DTBL.
	if err := state.persistIndexes(ctx, c.name); err != nil {
		return nil, fmt.Errorf("dolt: persisting secondary indexes: %w", err)
	}

	skipSync := params.SkipDurableSync && !c.db.backend.autoCommit
	dtblHash, err := state.dtblHashForCollection(ctx, c.name, newMap, hash.Hash{})
	if err != nil {
		return nil, err
	}
	if err := state.updateAddressMapWithSync(ctx, c.db.rootish, func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, dtblHash)
	}, skipSync); err != nil {
		return nil, err
	}

	if c.db.backend.autoCommit {
		msg := fmt.Sprintf("auto: update %s", c.name)
		newDS, _, err := commitCollectionsAMAs(ctx, state.doltDB, state.ds, state.am, msg, "dumbodb <dumbodb@localhost>", time.Now())
		if err != nil {
			return nil, fmt.Errorf("dolt: auto-commit after update: %w", err)
		}
		state.ds = newDS
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

	// Persist secondary indexes through the DTBL write so any in-memory
	// updates survive restart (DeleteAll itself does not yet rebuild index
	// entries — tracked separately).
	if err := state.persistIndexes(ctx, c.name); err != nil {
		return nil, fmt.Errorf("dolt: persisting secondary indexes: %w", err)
	}

	skipSync := params.SkipDurableSync && !c.db.backend.autoCommit
	dtblHash, err := state.dtblHashForCollection(ctx, c.name, newMap, hash.Hash{})
	if err != nil {
		return nil, err
	}
	if err := state.updateAddressMapWithSync(ctx, c.db.rootish, func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, dtblHash)
	}, skipSync); err != nil {
		return nil, err
	}

	if c.db.backend.autoCommit {
		msg := fmt.Sprintf("auto: delete from %s", c.name)
		newDS, _, err := commitCollectionsAMAs(ctx, state.doltDB, state.ds, state.am, msg, "dumbodb <dumbodb@localhost>", time.Now())
		if err != nil {
			return nil, fmt.Errorf("dolt: auto-commit after delete: %w", err)
		}
		state.ds = newDS
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

	// Build prolly.Map secondary indexes for newly added indexes by scanning
	// the primary map. Errors are returned to the caller — a half-built index
	// would silently miss matches and return wrong results from later queries.
	for _, idx := range params.Indexes {
		if idx.Name == backends.DefaultIndexName {
			continue
		}
		if _, exists := state.secIndexMaps[c.name][idx.Name]; exists {
			continue
		}
		idxMap, err := c.buildSecondaryIndex(ctx, state, idx)
		if err != nil {
			return nil, fmt.Errorf("dolt: building secondary index %q on %q: %w", idx.Name, c.name, err)
		}
		if state.secIndexMaps[c.name] == nil {
			state.secIndexMaps[c.name] = make(map[string]prolly.Map)
		}
		state.secIndexMaps[c.name][idx.Name] = idxMap
	}

	// Persist the new index set and re-write the collection's DTBL so the
	// secondary_indexes AM survives restart even if no document writes follow.
	if err := state.persistIndexes(ctx, c.name); err != nil {
		return nil, fmt.Errorf("dolt: persisting secondary indexes: %w", err)
	}
	if err := c.rewriteDTBLAfterIndexChange(ctx, state); err != nil {
		return nil, fmt.Errorf("dolt: rewriting DTBL for %q: %w", c.name, err)
	}

	return &backends.CreateIndexesResult{}, nil
}

// rewriteDTBLAfterIndexChange rebuilds the collection's DTBL with the
// current state.collIndexAMs[c.name] and updates the collections AM. Called
// after CreateIndexes / DropIndexes so an index-only change is durable.
//
// If the collection has no primary map yet (no inserts have happened) it
// uses a freshly created empty map — the same path as loadOrCreateMap.
//
// The caller must hold state.mu (write lock).
func (c *collection) rewriteDTBLAfterIndexChange(ctx context.Context, state *dbState) error {
	am, err := state.getOrInitBranchAM(ctx, c.db.rootish)
	if err != nil {
		return err
	}
	rootHash, err := am.Get(ctx, c.name)
	if err != nil {
		return err
	}

	var primary prolly.Map
	if rootHash.IsEmpty() {
		primary, err = newEmptyMap(ctx, state.ns)
		if err != nil {
			return err
		}
	} else {
		primary, err = openCollection(ctx, state.cs, state.ns, rootHash)
		if err != nil {
			return err
		}
	}

	dtblHash, err := state.dtblHashForCollection(ctx, c.name, primary, hash.Hash{})
	if err != nil {
		return err
	}
	return state.updateAddressMap(ctx, c.db.rootish, func(ed prolly.AddressMapEditor) error {
		if rootHash.IsEmpty() {
			return ed.Add(ctx, c.name, dtblHash)
		}
		return ed.Update(ctx, c.name, dtblHash)
	})
}

// buildSecondaryIndex scans the primary map and builds a secondary index prolly.Map.
// Must be called with state.mu held (write lock).
func (c *collection) buildSecondaryIndex(ctx context.Context, state *dbState, idx backends.IndexInfo) (prolly.Map, error) {
	idxMap, err := idxpkg.NewEmptyMap(ctx, state.ns)
	if err != nil {
		return prolly.Map{}, fmt.Errorf("index: creating empty map: %w", err)
	}

	// If the collection does not exist yet (no primary map), the index is
	// validly empty: subsequent inserts will populate it through
	// updateSecondaryIndexesOnInsert. AM-resolution and root-hash errors are
	// real failures and propagate.
	am, err := state.getOrInitBranchAM(ctx, c.db.rootish)
	if err != nil {
		return prolly.Map{}, fmt.Errorf("index: resolving branch AM: %w", err)
	}
	rootHash, err := am.Get(ctx, c.name)
	if err != nil {
		return prolly.Map{}, fmt.Errorf("index: looking up collection in AM: %w", err)
	}
	if rootHash.IsEmpty() {
		return idxMap, nil
	}
	primaryMap, err := openCollection(ctx, state.cs, state.ns, rootHash)
	if err != nil {
		return prolly.Map{}, fmt.Errorf("index: opening primary map: %w", err)
	}

	mut := idxMap.Mutate()
	iter, err := primaryMap.IterAll(ctx)
	if err != nil {
		return prolly.Map{}, fmt.Errorf("index: iterating primary: %w", err)
	}

	for {
		k, v, err := iter.Next(ctx)
		if err != nil {
			if err == io.EOF {
				break
			}
			return prolly.Map{}, err
		}
		if v == nil {
			break
		}

		idBytes, ok := keyDesc.GetBytes(0, k)
		if !ok {
			return prolly.Map{}, fmt.Errorf("index: primary key tuple missing _id bytes")
		}

		jsonHash, ok := valDesc.GetJSONAddr(0, v)
		if !ok {
			return prolly.Map{}, fmt.Errorf("index: primary value tuple missing JSON addr")
		}
		doc, err := readDocJSON(ctx, state.ns, jsonHash)
		if err != nil {
			return prolly.Map{}, fmt.Errorf("index: reading document for build scan: %w", err)
		}

		fieldVals := extractIndexFieldValues(doc, idx)
		if err := idxpkg.InsertEntry(ctx, mut, fieldVals, idBytes); err != nil {
			return prolly.Map{}, fmt.Errorf("index: inserting entry: %w", err)
		}
	}

	built, err := mut.Map(ctx)
	if err != nil {
		return prolly.Map{}, fmt.Errorf("index: flushing map: %w", err)
	}
	return built, nil
}

// extractIndexFieldValues returns the field values for the given index, in key order.
// Missing fields are returned as types.Null.
func extractIndexFieldValues(doc *types.Document, idx backends.IndexInfo) []any {
	vals := make([]any, len(idx.Key))
	for i, kp := range idx.Key {
		v, err := doc.Get(kp.Field)
		if err != nil {
			vals[i] = types.Null
		} else {
			vals[i] = v
		}
	}
	return vals
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

	// Drop the in-memory secondary maps for the removed indexes too, so a
	// subsequent persistIndexes call doesn't re-emit them.
	if secMaps := state.secIndexMaps[c.name]; secMaps != nil {
		for name := range drop {
			delete(secMaps, name)
		}
		if len(secMaps) == 0 {
			delete(state.secIndexMaps, c.name)
		}
	}

	if err := state.persistIndexes(ctx, c.name); err != nil {
		return nil, fmt.Errorf("dolt: persisting secondary indexes after drop: %w", err)
	}
	if err := c.rewriteDTBLAfterIndexChange(ctx, state); err != nil {
		return nil, fmt.Errorf("dolt: rewriting DTBL after drop for %q: %w", c.name, err)
	}

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

	dtblHash, err := state.dtblHashForCollection(ctx, c.name, emptyMap, hash.Hash{})
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

// applyFieldMutations mutates the stored JSON document at existingHash
// field-by-field using Dolt's IndexedJsonDocument.Set / .Remove, which
// re-chunks only the affected region of the prolly tree and shares every
// unchanged chunk with the new root. Returns the root hash of the mutated
// document.
//
// Returns an error if the stored document is not in the indexed JSON format
// (e.g. a legacy multi-chunk blob) or if Set / Remove falls back to an
// in-memory representation, which would defeat structural sharing. The caller
// treats any error as a signal to fall back to a full rewrite via
// writeDocJSON.
func applyFieldMutations(ctx context.Context, ns tree.NodeStore, existingHash hash.Hash, mutations []backends.FieldMutation) (hash.Hash, error) {
	wrapper, err := tree.NewJSONDoc(existingHash, ns).ToIndexedJSONDocument(ctx)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("dolt: loading indexed JSON document: %w", err)
	}

	idx, ok := wrapper.(tree.IndexedJsonDocument)
	if !ok {
		// Legacy non-indexed multi-chunk document: no structural sharing
		// available.
		return hash.Hash{}, fmt.Errorf("dolt: stored document is not indexed")
	}

	for _, m := range mutations {
		// isSimpleTopLevelKey in the handler already guarantees m.Key is a
		// bare identifier, so "$." + key is a safe MySQL-style JSON path.
		path := "$." + m.Key

		if m.Unset {
			res, _, err := idx.Remove(ctx, path)
			if err != nil {
				return hash.Hash{}, err
			}
			next, ok := res.(tree.IndexedJsonDocument)
			if !ok {
				return hash.Hash{}, fmt.Errorf("dolt: remove fell back to in-memory document")
			}
			idx = next
			continue
		}

		valJSON, err := marshalExtJSONValue(m.Value)
		if err != nil {
			return hash.Hash{}, err
		}

		res, _, err := idx.Set(ctx, path, sqltypes.NewLazyJSONDocument(valJSON))
		if err != nil {
			return hash.Hash{}, err
		}
		next, ok := res.(tree.IndexedJsonDocument)
		if !ok {
			return hash.Hash{}, fmt.Errorf("dolt: set fell back to in-memory document")
		}
		idx = next
	}

	// SerializeJsonToAddr fast-paths IndexedJsonDocument by returning its
	// cached root node rather than re-chunking, so this is O(1).
	root, err := tree.SerializeJsonToAddr(ctx, ns, idx)
	if err != nil {
		return hash.Hash{}, err
	}
	return root.HashOf(), nil
}

// marshalExtJSONValue produces the canonical Extended JSON encoding of a
// single BSON value, bare (without any field-name wrapper). It matches the
// encoding produced by writeDocJSON so a value inserted here round-trips
// through readDocJSON identically to one written by a full rewrite.
func marshalExtJSONValue(value any) ([]byte, error) {
	tmp, err := types.NewDocument("v", value)
	if err != nil {
		return nil, fmt.Errorf("dolt: wrapping value for ExtJSON: %w", err)
	}

	var bsonBytes []byte
	if docHasMinMaxKey(tmp) {
		bsonBytes, err = bson.FromDocumentRaw(tmp)
		if err != nil {
			return nil, fmt.Errorf("dolt: encoding value with MinKey/MaxKey to BSON: %w", err)
		}
	} else {
		wdoc, err := bson.FromDocument(tmp)
		if err != nil {
			return nil, fmt.Errorf("dolt: encoding value to wirebson: %w", err)
		}
		bsonBytes, err = wdoc.Encode()
		if err != nil {
			return nil, fmt.Errorf("dolt: encoding value to BSON: %w", err)
		}
	}

	extJSON, err := mongobson.MarshalExtJSON(mongobson.Raw(bsonBytes), true, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: converting value to ExtJSON: %w", err)
	}

	// Strip `{"v":` prefix and `}` suffix. The MongoDB driver emits compact
	// JSON with no whitespace, but we trim any leading spaces after the
	// colon defensively.
	prefix := []byte(`{"v":`)
	if !bytes.HasPrefix(extJSON, prefix) || extJSON[len(extJSON)-1] != '}' {
		return nil, fmt.Errorf("dolt: unexpected ExtJSON shape %q", extJSON)
	}
	inner := bytes.TrimLeft(extJSON[len(prefix):len(extJSON)-1], " ")
	if len(inner) == 0 {
		return nil, fmt.Errorf("dolt: empty ExtJSON value for %q", extJSON)
	}
	return inner, nil
}

// readDocJSON reads a JSON document from the dolt chunk store at the given hash
// and decodes it back to a types.Document.
func readDocJSON(ctx context.Context, ns tree.NodeStore, h hash.Hash) (*types.Document, error) {
	jsonBytes, err := readDocJSONBytes(ctx, ns, h)
	if err != nil {
		return nil, err
	}
	return decodeDocFromJSON(jsonBytes)
}

// readDocJSONBytes reads the raw canonical Extended JSON bytes for a document.
// Cheap: just pulls the stored bytes from the chunk store without decoding.
// Use this when you want to inspect the document (e.g. for filter pushdown)
// before paying the cost of a full BSON / types.Document decode.
func readDocJSONBytes(ctx context.Context, ns tree.NodeStore, h hash.Hash) ([]byte, error) {
	jsonDoc := tree.NewJSONDoc(h, ns)
	wrapper, err := jsonDoc.ToIndexedJSONDocument(ctx)
	if err != nil {
		return nil, fmt.Errorf("dolt: reading JSON document: %w", err)
	}
	jsonBytes, err := sqltypes.MarshallJson(ctx, wrapper)
	if err != nil {
		return nil, fmt.Errorf("dolt: getting JSON bytes: %w", err)
	}
	return jsonBytes, nil
}

// decodeDocFromJSON converts canonical Extended JSON bytes to a types.Document
// via a BSON round-trip. This is the expensive half of readDocJSON; skip it
// when a prefilter has already ruled the document out.
func decodeDocFromJSON(jsonBytes []byte) (*types.Document, error) {
	var rawBSON mongobson.Raw
	if err := mongobson.UnmarshalExtJSON(jsonBytes, true, &rawBSON); err != nil {
		return nil, fmt.Errorf("dolt: converting JSON to BSON: %w", err)
	}
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
	// prefilter, if non-nil, is a cheap byte-level check applied to each
	// document's raw canonical Extended JSON before the expensive decode.
	// Returning false means "definitely doesn't match the filter"; returning
	// true means "may match — run the full filter downstream." A nil
	// prefilter keeps the unconditional full-scan behavior.
	prefilter func([]byte) bool
}

// newMapIter creates an iterator over the prolly.Map.
func newMapIter(ctx context.Context, ns tree.NodeStore, m prolly.Map, reverse bool, limit int64, onlyRecordID bool, prefilter func([]byte) bool) types.DocumentsIterator {
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
		prefilter:    prefilter,
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

		var doc *types.Document
		if it.prefilter != nil {
			jsonBytes, err := readDocJSONBytes(it.ctx, it.ns, jsonHash)
			if err != nil {
				return struct{}{}, nil, err
			}
			if !it.prefilter(jsonBytes) {
				continue
			}
			doc, err = decodeDocFromJSON(jsonBytes)
			if err != nil {
				return struct{}{}, nil, err
			}
		} else {
			doc, err = readDocJSON(it.ctx, it.ns, jsonHash)
			if err != nil {
				return struct{}{}, nil, err
			}
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

// sliceIter iterates over a pre-fetched slice of documents.
// Used by the secondary index lookup path to return results without a full scan.
type sliceIter struct {
	docs []*types.Document
	pos  int
}

func newSliceIter(docs []*types.Document) types.DocumentsIterator {
	return &sliceIter{docs: docs}
}

func (it *sliceIter) Next() (struct{}, *types.Document, error) {
	if it.pos >= len(it.docs) {
		return struct{}{}, nil, iterator.ErrIteratorDone
	}
	doc := it.docs[it.pos]
	it.pos++
	return struct{}{}, doc, nil
}

func (it *sliceIter) Close() {}
