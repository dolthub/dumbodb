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
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/bsonindexed"
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
// AM (state.branchAMs[defaultBranch]) is used. When the rootish is a bare commit hash or a tag name,
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
	// not a sound lower bound  -- skip the prefilter.
	var pf func([]byte) bool
	if params != nil && !onlyRecordIDs && !params.CaseInsensitive {
		pf = buildScanPrefilter(params.Filter)
	}

	return &backends.QueryResult{
		Iter: newMapIter(ctx, state.ns, m, reverse, limit, onlyRecordIDs, pf),
	}, nil
}

// buildScanPrefilter returns a byte-level predicate over a document's raw
// canonical Extended JSON bytes that is sound for the given filter  -- i.e. if
// the predicate returns false, the document is guaranteed not to match.
// Returns nil when no sound prefilter can be built (complex filter, null
// value, unsupported type, ambiguous numeric, etc.); in that case the scan
// falls back to decoding every document.
func buildScanPrefilter(filter *types.Document) func([]byte) bool {
	if filter == nil {
		return nil
	}

	keys := filter.Keys()
	// One predicate per top-level field clause. All AND-combined.
	preds := make([]func([]byte) bool, 0, len(keys))
	for _, field := range keys {
		// $and/$or/$comment/etc.  -- bail out and let the handler filter.
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
		pred := buildFieldPredicate(field, v)
		if pred == nil {
			return nil
		}
		preds = append(preds, pred)
	}
	if len(preds) == 0 {
		return nil
	}

	return func(jsonBytes []byte) bool {
		for _, pred := range preds {
			if !pred(jsonBytes) {
				return false
			}
		}
		return true
	}
}

func buildFieldPredicate(field string, value any) func([]byte) bool {
	return buildBSONFieldPredicate(field, value)
}

// rangeProbeStatus describes the outcome of probing a stored document's
// JSON for a top-level numeric field.
type rangeProbeStatus int

const (
	// rangeProbeFound  -- top-level field located and parsed as a finite
	// numeric. The float64 return is the parsed value.
	rangeProbeFound rangeProbeStatus = iota
	// rangeProbeMissing  -- top-level field is provably absent from the
	// document's outer object. A range filter never matches such docs.
	rangeProbeMissing
	// rangeProbeBail  -- the JSON shape, value type, or numeric precision
	// can't be modeled by the byte-level predicate. The caller must treat
	// the doc as a possible match (permissive true) and let the handler's
	// FilterIterator re-validate.
	rangeProbeBail
)


// simpleIDEquality reports whether filter contains an "_id" field bound to a
// concrete scalar value that can be hashed into a primary key. It rejects
// operator forms ({_id: {$eq: x}}), array equality ({_id: [1,2]}), and null.
// Other filter fields are allowed  -- the handler re-checks them on the result.
func simpleIDEquality(filter *types.Document) (any, bool) {
	v, err := filter.Get("_id")
	if err != nil {
		return nil, false
	}
	switch val := v.(type) {
	case *types.Document:
		// {$eq: x} / {$gt: x} / etc.  -- let the full scan handle it.
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

		d, err := readDocFromValue(ctx, ns, v)
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

// tryIndexLookup picks an indexed field constrained by the filter (equality or
// supported range operators) and uses its secondary index to narrow the result
// set. The returned docs are a superset of the matches: the handler's
// FilterIterator re-validates every document against the full filter, so any
// false positives the index lookup admits (e.g. compound filters where only
// one field is indexed) are filtered out downstream. The lookup is sound  -- it
// never drops a matching document.
//
// Returns (nil, false, nil) if no suitable index was found (caller should fall
// back to the full scan).
func (c *collection) tryIndexLookup(ctx context.Context, state *dbState, primary prolly.Map, filter *types.Document) ([]*types.Document, bool, error) {
	if filter == nil || filter.Len() == 0 {
		return nil, false, nil
	}

	state.mu.RLock()
	idxInfos, idxMaps, err := resolveIndexes(ctx, c, state)
	state.mu.RUnlock()
	if err != nil {
		return nil, false, err
	}
	if len(idxInfos) == 0 {
		return nil, false, nil
	}

	// Map indexed-leading-field -> (index name, map index). Only single-field
	// indexes are usable here; compound indexes require key-prefix matching
	// that the planner doesn't yet do.
	type indexedFieldEntry struct {
		idxName string
		mapIdx  int
	}
	indexedField := make(map[string]indexedFieldEntry, len(idxInfos))
	for i, idx := range idxInfos {
		if len(idx.Key) == 1 && idx.Key[0].Field != "" {
			indexedField[idx.Key[0].Field] = indexedFieldEntry{idxName: idx.Name, mapIdx: i}
		}
	}
	if len(indexedField) == 0 {
		return nil, false, nil
	}

	var (
		chosenMapIdx int
		startKey     []byte
		stopKey      []byte
		usable       bool
	)

	for _, k := range filter.Keys() {
		// Top-level $-operators ($and/$or/$nor/$comment/...) make the
		// per-field analysis below unsound  -- bail.
		if strings.HasPrefix(k, "$") {
			return nil, false, nil
		}
		// Dotted paths look up into sub-documents; the index is keyed on the
		// flat field, so it can't be used for those constraints.
		if strings.ContainsRune(k, '.') {
			continue
		}
		entry, ok := indexedField[k]
		if !ok {
			continue
		}
		v, err := filter.Get(k)
		if err != nil {
			continue
		}
		s, e, ok := indexBoundsForFilterValue(v)
		if !ok {
			continue
		}
		chosenMapIdx = entry.mapIdx
		startKey, stopKey = s, e
		usable = true
		break
	}

	if !usable {
		return nil, false, nil
	}

	idxMap := idxMaps[chosenMapIdx]

	// Cap the index range scan at half the collection. The point-fetch cost
	// per matching id (one prolly tree traversal) dominates the scan path's
	// per-doc cost (sequential leaf walk), so once a filter would route more
	// than ~50% of the collection through the index path it is strictly
	// slower than a full scan. m.Count is O(1) (prolly tree metadata), so
	// the gate is essentially free for the cases where the index does win.
	primaryCount, err := primary.Count()
	if err != nil {
		return nil, false, fmt.Errorf("index lookup counting primary: %w", err)
	}
	maxResults := -1
	if primaryCount > 0 {
		maxResults = int(primaryCount) / 2
	}
	primaryIDBytesList, exceeded, err := idxpkg.RangeLookupCapped(ctx, idxMap, startKey, stopKey, maxResults)
	if err != nil {
		return nil, false, err
	}
	if exceeded {
		// Caller will fall through to the sequential primary scan, which is
		// faster than per-id point lookups for low-selectivity ranges.
		return nil, false, nil
	}

	// Fetch the actual documents from the primary map. Errors propagate so
	// callers see real failures instead of empty results that look like a
	// missing match.
	docs := make([]*types.Document, 0, len(primaryIDBytesList))
	for _, idBytes := range primaryIDBytesList {
		key, err := buildKey(idBytes)
		if err != nil {
			return nil, true, fmt.Errorf("index lookup building key: %w", err)
		}
		var doc *types.Document
		if err := primary.Get(ctx, key, func(k, v val.Tuple) error {
			if v == nil {
				return nil
			}
			var decErr error
			doc, decErr = readDocFromValue(ctx, state.ns, v)
			return decErr
		}); err != nil {
			return nil, true, fmt.Errorf("index lookup primary fetch: %w", err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}

	return docs, true, nil
}

// indexBoundsForFilterValue translates the value side of a single
// {field: value} filter clause into a [startKey, stopKey) byte range over the
// secondary index. The bounds are always sound: any document matching the
// clause is guaranteed to lie in the returned range, though the range may
// include false positives (the handler will filter them out).
//
// Supported value shapes:
//   - bare scalar v                          -> [KS(v)+0x04, KS(v)+0x05)
//   - operator doc using only $eq / $gt /
//     $gte / $lt / $lte                      -> bounds derived from the
//                                              tightest clause on each side.
//
// Returns ok=false for value shapes the index path can't handle (regex,
// $in, $ne, nested operators, mixed comparisons against arrays, ...); the
// caller falls back to the full-scan path.
func indexBoundsForFilterValue(v any) (startKey, stopKey []byte, ok bool) {
	opDoc, isOp := v.(*types.Document)
	if !isOp {
		// Bare scalar equality. types.Null and array equality have semantics
		// (e.g. matching missing fields, element-wise array match) that the
		// byte-level index can't reproduce  -- skip and let the scan path
		// handle them.
		switch v.(type) {
		case nil, types.NullType, *types.Array:
			return nil, nil, false
		}
		return idxpkg.LowerBoundInclusive(v), idxpkg.UpperBoundInclusive(v), true
	}

	// Operator document. Every key must be a supported comparison operator
	// against a scalar; anything else (e.g. $regex, $in, $ne, $type, $exists)
	// makes the byte-level bounds unsound for this clause.
	var (
		hasLower, lowerInclusive bool
		lowerVal                 any
		hasUpper, upperInclusive bool
		upperVal                 any
		hasEq                    bool
		eqVal                    any
	)
	for _, opKey := range opDoc.Keys() {
		opVal, err := opDoc.Get(opKey)
		if err != nil {
			return nil, nil, false
		}
		switch opVal.(type) {
		case *types.Document, *types.Array, nil, types.NullType, types.Regex:
			return nil, nil, false
		}
		switch opKey {
		case "$eq":
			hasEq = true
			eqVal = opVal
		case "$gte":
			if !hasLower || compareScalars(opVal, lowerVal) > 0 {
				lowerVal, lowerInclusive, hasLower = opVal, true, true
			}
		case "$gt":
			if !hasLower || compareScalars(opVal, lowerVal) >= 0 {
				lowerVal, lowerInclusive, hasLower = opVal, false, true
			}
		case "$lte":
			if !hasUpper || compareScalars(opVal, upperVal) < 0 {
				upperVal, upperInclusive, hasUpper = opVal, true, true
			}
		case "$lt":
			if !hasUpper || compareScalars(opVal, upperVal) <= 0 {
				upperVal, upperInclusive, hasUpper = opVal, false, true
			}
		default:
			return nil, nil, false
		}
	}

	if hasEq {
		// $eq with a range narrows further, but for correctness we just
		// translate $eq alone to its tight equality range. The handler will
		// re-check any range constraint that may also be present.
		return idxpkg.LowerBoundInclusive(eqVal), idxpkg.UpperBoundInclusive(eqVal), true
	}

	if !hasLower && !hasUpper {
		return nil, nil, false
	}

	if hasLower {
		if lowerInclusive {
			startKey = idxpkg.LowerBoundInclusive(lowerVal)
		} else {
			startKey = idxpkg.LowerBoundExclusive(lowerVal)
		}
	}
	if hasUpper {
		if upperInclusive {
			stopKey = idxpkg.UpperBoundInclusive(upperVal)
		} else {
			stopKey = idxpkg.UpperBoundExclusive(upperVal)
		}
	}
	return startKey, stopKey, true
}

// compareScalars returns -1 / 0 / 1 using types.Compare. Used to merge
// multiple bounds on the same side ($gte:1, $gte:5 -> keep the tighter 5).
func compareScalars(a, b any) int {
	switch types.Compare(a, b) {
	case types.Less:
		return -1
	case types.Greater:
		return 1
	default:
		return 0
	}
}

// explainStage is the in-memory representation of one node in the explain
// winningPlan tree. The tree is built bottom-up (leaf -> outermost) and
// then rendered as nested *types.Document.
type explainStage struct {
	stage      string
	indexName  string
	keyPattern *types.Document
	input      *explainStage
}

// toDoc renders the stage tree as a winningPlan document with nested
// inputStage chain.
func (s *explainStage) toDoc() *types.Document {
	d := must.NotFail(types.NewDocument("stage", s.stage))
	if s.indexName != "" {
		d.Set("indexName", s.indexName)
	}
	if s.keyPattern != nil {
		d.Set("keyPattern", s.keyPattern)
	}
	if s.input != nil {
		d.Set("inputStage", s.input.toDoc())
	}
	return d
}

// pickIndexForFilter mirrors tryIndexLookup's index-selection rule for
// the planner side of explain: a single-field index whose leading field
// matches a top-level filter field with usable bounds. Returns the
// chosen IndexInfo (zero-value if none).
func pickIndexForFilter(filter *types.Document, idxInfos []backends.IndexInfo) (backends.IndexInfo, bool) {
	if filter == nil || filter.Len() == 0 {
		return backends.IndexInfo{}, false
	}
	for _, k := range filter.Keys() {
		if strings.HasPrefix(k, "$") {
			return backends.IndexInfo{}, false
		}
		if strings.ContainsRune(k, '.') {
			continue
		}
		v, err := filter.Get(k)
		if err != nil {
			continue
		}
		if _, _, ok := indexBoundsForFilterValue(v); !ok {
			continue
		}
		for _, idx := range idxInfos {
			if len(idx.Key) == 1 && idx.Key[0].Field == k {
				return idx, true
			}
		}
	}
	return backends.IndexInfo{}, false
}

// keyPatternOf renders an IndexInfo's key as a {field: 1|-1} document
// matching MongoDB's keyPattern shape.
func keyPatternOf(idx backends.IndexInfo) *types.Document {
	kp := types.MakeDocument(len(idx.Key))
	for _, k := range idx.Key {
		dir := int32(1)
		if k.Descending {
			dir = -1
		}
		kp.Set(k.Field, dir)
	}
	return kp
}

func (c *collection) Explain(ctx context.Context, params *backends.ExplainParams) (*backends.ExplainResult, error) {
	parsedQuery := types.MakeDocument(0)
	if params != nil && params.Filter != nil {
		parsedQuery = params.Filter.DeepCopy()
	}

	var idxInfos []backends.IndexInfo
	if state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, false); err == nil && state != nil {
		state.mu.RLock()
		resolvedInfos, _, rerr := resolveIndexes(ctx, c, state)
		state.mu.RUnlock()
		if rerr == nil {
			idxInfos = append(idxInfos, resolvedInfos...)
		}
	}

	// Picked index: explicit hint wins; otherwise an equality / range
	// filter on a single-field index.
	var picked backends.IndexInfo
	var indexPicked bool
	if params != nil {
		if hintName := pickHintedIndex(params.Hint, idxInfos); hintName != "" {
			for _, idx := range idxInfos {
				if idx.Name == hintName {
					picked = idx
					indexPicked = true
					break
				}
			}
		}
	}
	if !indexPicked && params != nil {
		picked, indexPicked = pickIndexForFilter(params.Filter, idxInfos)
	}
	if !indexPicked && params != nil && params.Command == "distinct" && params.DistinctKey != "" {
		// distinct uses a covered index on its key when one exists; the
		// filter (if any) does not drive selection.
		for _, idx := range idxInfos {
			if len(idx.Key) == 1 && idx.Key[0].Field == params.DistinctKey {
				picked = idx
				indexPicked = true
				break
			}
		}
	}

	command := ""
	if params != nil {
		command = params.Command
	}

	winningPlan := buildExplainPlan(command, params, picked, indexPicked)

	qp := must.NotFail(types.NewDocument(
		"namespace", c.db.name+"."+c.name,
		"parsedQuery", parsedQuery,
		"winningPlan", winningPlan.toDoc(),
	))
	return &backends.ExplainResult{
		QueryPlanner: qp,
	}, nil
}

// buildExplainPlan composes the stage tree for the given command and
// planning choice. Only shapes the planner choices DumboDB already makes
// (workspace-bbg scope) -- no new planner capabilities (covered queries,
// compound indexes, OR/AND multi-index, sort via index) are introduced
// here; those land in their own follow-ups.
func buildExplainPlan(command string, params *backends.ExplainParams, picked backends.IndexInfo, indexPicked bool) *explainStage {
	switch command {
	case "count":
		var leaf *explainStage
		if indexPicked {
			leaf = &explainStage{
				stage:      "COUNT_SCAN",
				indexName:  picked.Name,
				keyPattern: keyPatternOf(picked),
			}
		} else {
			leaf = &explainStage{stage: "COLLSCAN"}
		}
		return &explainStage{stage: "COUNT", input: leaf}

	case "distinct":
		// MongoDB emits PROJECTION_COVERED above DISTINCT_SCAN. DumboDB's
		// DistinctScan path uses the index covering the distinct key; if
		// no usable index, fall back to COLLSCAN (rare in practice; the
		// parity test always provides one).
		if indexPicked {
			leaf := &explainStage{
				stage:      "DISTINCT_SCAN",
				indexName:  picked.Name,
				keyPattern: keyPatternOf(picked),
			}
			return &explainStage{stage: "PROJECTION_COVERED", input: leaf}
		}
		return &explainStage{stage: "COLLSCAN"}

	default:
		// "find" and "aggregate" (the latter via pushed-down $match) share
		// the same shape rules.
		return buildFindExplainPlan(params, picked, indexPicked)
	}
}

// buildFindExplainPlan composes the stage tree for a find-like query
// (also used for aggregate with pushed-down $match). Wrapping order
// from inner to outer: leaf -> FETCH (if IXSCAN) -> SORT -> SKIP ->
// LIMIT (only when no SORT, because SORT absorbs LIMIT into a TopK
// internally) -> PROJECTION_SIMPLE. Covered-query (PROJECTION_COVERED
// without FETCH) is bucket B and not handled here.
func buildFindExplainPlan(params *backends.ExplainParams, picked backends.IndexInfo, indexPicked bool) *explainStage {
	var node *explainStage
	if indexPicked {
		node = &explainStage{
			stage:      "IXSCAN",
			indexName:  picked.Name,
			keyPattern: keyPatternOf(picked),
		}
		node = &explainStage{stage: "FETCH", input: node}
	} else {
		node = &explainStage{stage: "COLLSCAN"}
	}

	if params == nil {
		return node
	}

	sortPresent := params.Sort != nil && params.Sort.Len() > 0 && !sortIsNatural(params.Sort)
	if sortPresent {
		node = &explainStage{stage: "SORT", input: node}
	}
	if params.Skip > 0 {
		node = &explainStage{stage: "SKIP", input: node}
	}
	// MongoDB absorbs LIMIT into SORT (TopK), so it does not emit a
	// separate LIMIT stage when sort is present.
	if params.Limit > 0 && !sortPresent {
		node = &explainStage{stage: "LIMIT", input: node}
	}
	if params.Projection != nil && params.Projection.Len() > 0 {
		node = &explainStage{stage: "PROJECTION_SIMPLE", input: node}
	}
	return node
}

// sortIsNatural reports whether sort is the single-key {"$natural": ...}
// document, which MongoDB satisfies by collection order with no SORT
// stage.
func sortIsNatural(sort *types.Document) bool {
	if sort == nil || sort.Len() != 1 {
		return false
	}
	return sort.Keys()[0] == "$natural"
}

// pickHintedIndex resolves a hint value (either a name string or a key-pattern
// document) to a matching index name, or returns "" if no index matches.
func pickHintedIndex(hint any, idxInfos []backends.IndexInfo) string {
	if hint == nil {
		return ""
	}
	switch h := hint.(type) {
	case string:
		for _, idx := range idxInfos {
			if idx.Name == h {
				return idx.Name
			}
		}
	case *types.Document:
		if h == nil || h.Len() == 0 {
			return ""
		}
		hintKeys := h.Keys()
		for _, idx := range idxInfos {
			if len(idx.Key) != len(hintKeys) {
				continue
			}
			match := true
			for i, k := range hintKeys {
				if idx.Key[i].Field != k {
					match = false
					break
				}
			}
			if match {
				return idx.Name
			}
		}
	}
	return ""
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

func (c *collection) InsertAll(ctx context.Context, params *backends.InsertAllParams) (*backends.InsertAllResult, error) {
	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, true)
	if err != nil {
		return nil, err
	}

	// acquireInsertLocks may block waiting for a holding transaction to
	// commit/abort (non-txn callers); the holding txn's release path needs
	// state.mu, so the wait must happen before we take state.mu.
	if err := c.acquireInsertLocks(ctx, params.Docs); err != nil {
		return nil, err
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if c.db.isReadOnly(ctx, state) {
		return nil, backends.NewError(backends.ErrorCodeReadOnlyDatabase, fmt.Errorf("cannot write to a read-only database snapshot"))
	}

	// Load or create the collection's prolly map.
	m, err := c.loadOrCreateMap(ctx, state)
	if err != nil {
		return nil, err
	}

	// Collect unique secondary indexes for this collection. Resolve from
	// the per-branch DTBL so unique-constraint enforcement is consistent
	// with the rest of the write path.
	branchInfos, _, err := resolveBranchIndexState(ctx, c, state)
	if err != nil {
		return nil, fmt.Errorf("resolving branch index state: %w", err)
	}
	var uniqueIndexes []backends.IndexInfo
	for _, idx := range branchInfos {
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
			existingDoc, err := readDocFromValue(ctx, state.ns, v)
			if err != nil {
				continue
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
			return nil, fmt.Errorf("document missing _id: %w", err)
		}

		// Hash _id to get the fixed-size primary key.
		h, err := hashID(docID)
		if err != nil {
			return nil, fmt.Errorf("hashing _id: %w", err)
		}

		// Check against existing IDs in the collection (point lookup).
		exists, err := existsID(ctx, m, h)
		if err != nil {
			return nil, fmt.Errorf("checking existing _id: %w", err)
		}
		if exists {
			return nil, backends.NewError(
				backends.ErrorCodeInsertDuplicateID,
				fmt.Errorf("duplicate _id in collection"),
			)
		}

		// Check against IDs already inserted in this batch.
		if _, dup := batchHashSet[h]; dup {
			return nil, backends.NewError(
				backends.ErrorCodeInsertDuplicateID,
				fmt.Errorf("duplicate _id in batch"),
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
						fmt.Errorf("duplicate key for unique index %s", idx.Name),
					)
				}
			}
			for _, batchKey := range batchUniqueKeys[i] {
				if indexKeysEqual(newKey, batchKey) {
					return nil, backends.NewError(
						backends.ErrorCodeInsertDuplicateID,
						fmt.Errorf("duplicate key for unique index %s", idx.Name),
					)
				}
			}
			batchUniqueKeys[i] = append(batchUniqueKeys[i], newKey)
		}

		key, err := buildKey(h[:])
		if err != nil {
			return nil, err
		}

		v, err := writeDocToValue(ctx, state.ns, doc)
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
	// triggers its own NBS journal fsync  -- deferring the working-set update but
	// still committing synchronously would leave the history and working set
	// inconsistent, so we only honor SkipDurableSync when autoCommit is off.
	//
	// Maintain secondary indexes for any documents we just inserted. The
	// resolver path reads the per-branch index state from disk, so the
	// resulting AM reflects only this branch's writes -- no cross-branch
	// leakage into other branches' DTBLs.
	infos, idxMaps, err := resolveBranchIndexState(ctx, c, state)
	if err != nil {
		return nil, fmt.Errorf("resolving branch index state: %w", err)
	}
	updatedIdxMaps, err := applyInsertsToIndexes(ctx, infos, idxMaps, params.Docs)
	if err != nil {
		return nil, fmt.Errorf("updating secondary indexes: %w", err)
	}
	newIdxAM, err := buildIndexAM(ctx, state, infos, updatedIdxMaps)
	if err != nil {
		return nil, fmt.Errorf("building index AM: %w", err)
	}

	skipSync := params.SkipDurableSync && !c.db.backend.autoCommit
	dtblHash, err := state.dtblHashForCollection(ctx, c.name, newMap, newIdxAM, hash.Hash{})
	if err != nil {
		return nil, err
	}
	if err := state.updateAddressMapWithSync(ctx, c.db.rootish, func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, dtblHash)
	}, skipSync); err != nil {
		return nil, err
	}

	if c.db.backend.autoCommit {
		fallbackWS, fbErr := state.loadBranchWS(ctx, c.db.rootish)
		if fbErr != nil {
			return nil, fmt.Errorf("auto-commit: loading WS: %w", fbErr)
		}
		workingRV, rvErr := workingRootViaSession(ctx, sessionFromContext(ctx), fallbackWS, c.db.name, c.db.rootish)
		if rvErr != nil {
			return nil, fmt.Errorf("auto-commit: reading working root: %w", rvErr)
		}
		workingAMForAutoCommit, _ := amFromWorkingRoot(ctx, workingRV, state.ns)
		msg := fmt.Sprintf("auto: insert into %s", c.name)
		mainDS, dsErr := state.datasDB.GetDataset(ctx, mainDataset)
		if dsErr != nil {
			return nil, fmt.Errorf("auto-commit after insert: resolving main dataset: %w", dsErr)
		}
		if _, _, err := commitCollectionsAMAs(ctx, state.datasDB, mainDS, workingAMForAutoCommit, msg, "dumbodb <dumbodb@localhost>", time.Now()); err != nil {
			return nil, fmt.Errorf("auto-commit after insert: %w", err)
		}
	}

	return &backends.InsertAllResult{}, nil
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
			return fmt.Errorf("capped evict hashing _id: %w", err)
		}

		key, err := buildKey(h[:])
		if err != nil {
			return fmt.Errorf("capped evict building key: %w", err)
		}

		if err := mut.Delete(ctx, key); err != nil {
			return fmt.Errorf("capped evict delete: %w", err)
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

	// See Insert: acquireUpdateLocks may block on a holding transaction whose
	// release path needs state.mu; wait before taking state.mu.
	if err := c.acquireUpdateLocks(ctx, params.Docs); err != nil {
		return nil, err
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if c.db.isReadOnly(ctx, state) {
		return nil, backends.NewError(backends.ErrorCodeReadOnlyDatabase, fmt.Errorf("cannot write to a read-only database snapshot"))
	}

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
			return nil, fmt.Errorf("document missing _id: %w", err)
		}

		h, err := hashID(docID)
		if err != nil {
			return nil, fmt.Errorf("hashing _id: %w", err)
		}

		key, err := buildKey(h[:])
		if err != nil {
			return nil, err
		}

		// Locate the existing stored document so a partial update can
		// dispatch on its storage shape (inline []byte vs out-of-band
		// *val.JsonAdaptiveStorage).
		var (
			found       bool
			existingTup val.Tuple
		)

		if err := mut.Get(ctx, key, func(k, v val.Tuple) error {
			if v == nil {
				return nil
			}
			found = true
			// Clone: the tuple's backing buffer is owned by the map
			// iterator and may be reused after this callback returns.
			existingTup = append(val.Tuple(nil), v...)
			return nil
		}); err != nil {
			return nil, err
		}

		if !found {
			continue
		}

		// Prefer a partial update when the handler supplied a trackable
		// mutation list and the prior document is readable. Any error in
		// the partial path falls through to a full rewrite from doc.
		var newBytes []byte

		var mutations []backends.FieldMutation
		if i < len(params.FieldMutations) {
			mutations = params.FieldMutations[i]
		}

		if len(mutations) > 0 && existingTup != nil {
			if b, err := applyFieldMutations(ctx, state.ns, existingTup, mutations); err == nil {
				newBytes = b
			}
		}

		if newBytes == nil {
			newBytes, err = docToExtJSON(doc)
			if err != nil {
				return nil, err
			}
		}

		v, err := buildValue(ctx, state.ns, newBytes)
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

	// UpdateAll does not yet rebuild secondary-index entries (workspace-4ee).
	// Preserve the current per-branch index AM resolved from disk so the
	// new DTBL keeps the collection's existing indexes intact.
	curIdxAM, err := resolveCollIndexAM(ctx, c, state)
	if err != nil {
		return nil, fmt.Errorf("resolving current index AM: %w", err)
	}

	skipSync := params.SkipDurableSync && !c.db.backend.autoCommit
	dtblHash, err := state.dtblHashForCollection(ctx, c.name, newMap, curIdxAM, hash.Hash{})
	if err != nil {
		return nil, err
	}
	if err := state.updateAddressMapWithSync(ctx, c.db.rootish, func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, dtblHash)
	}, skipSync); err != nil {
		return nil, err
	}

	if c.db.backend.autoCommit {
		fallbackWS, fbErr := state.loadBranchWS(ctx, c.db.rootish)
		if fbErr != nil {
			return nil, fmt.Errorf("auto-commit: loading WS: %w", fbErr)
		}
		workingRV, rvErr := workingRootViaSession(ctx, sessionFromContext(ctx), fallbackWS, c.db.name, c.db.rootish)
		if rvErr != nil {
			return nil, fmt.Errorf("auto-commit: reading working root: %w", rvErr)
		}
		workingAMForAutoCommit, _ := amFromWorkingRoot(ctx, workingRV, state.ns)
		msg := fmt.Sprintf("auto: update %s", c.name)
		mainDS, dsErr := state.datasDB.GetDataset(ctx, mainDataset)
		if dsErr != nil {
			return nil, fmt.Errorf("auto-commit after update: resolving main dataset: %w", dsErr)
		}
		if _, _, err := commitCollectionsAMAs(ctx, state.datasDB, mainDS, workingAMForAutoCommit, msg, "dumbodb <dumbodb@localhost>", time.Now()); err != nil {
			return nil, fmt.Errorf("auto-commit after update: %w", err)
		}
	}

	return &backends.UpdateAllResult{Updated: updated}, nil
}

func (c *collection) DeleteAll(ctx context.Context, params *backends.DeleteAllParams) (*backends.DeleteAllResult, error) {
	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, false)
	if err != nil {
		return nil, err
	}

	if state == nil {
		return &backends.DeleteAllResult{}, nil
	}

	// See Insert: acquireDeleteLocks may block on a holding transaction whose
	// release path needs state.mu; wait before taking state.mu.
	if err := c.acquireDeleteLocks(ctx, params.IDs); err != nil {
		return nil, err
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if c.db.isReadOnly(ctx, state) {
		return nil, backends.NewError(backends.ErrorCodeReadOnlyDatabase, fmt.Errorf("cannot write to a read-only database snapshot"))
	}

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

	// DeleteAll does not yet rebuild secondary-index entries (workspace-4ee).
	// Preserve the current per-branch index AM so the new DTBL keeps the
	// collection's existing indexes intact.
	curIdxAM, err := resolveCollIndexAM(ctx, c, state)
	if err != nil {
		return nil, fmt.Errorf("resolving current index AM: %w", err)
	}

	skipSync := params.SkipDurableSync && !c.db.backend.autoCommit
	dtblHash, err := state.dtblHashForCollection(ctx, c.name, newMap, curIdxAM, hash.Hash{})
	if err != nil {
		return nil, err
	}
	if err := state.updateAddressMapWithSync(ctx, c.db.rootish, func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, dtblHash)
	}, skipSync); err != nil {
		return nil, err
	}

	if c.db.backend.autoCommit {
		fallbackWS, fbErr := state.loadBranchWS(ctx, c.db.rootish)
		if fbErr != nil {
			return nil, fmt.Errorf("auto-commit: loading WS: %w", fbErr)
		}
		workingRV, rvErr := workingRootViaSession(ctx, sessionFromContext(ctx), fallbackWS, c.db.name, c.db.rootish)
		if rvErr != nil {
			return nil, fmt.Errorf("auto-commit: reading working root: %w", rvErr)
		}
		workingAMForAutoCommit, _ := amFromWorkingRoot(ctx, workingRV, state.ns)
		msg := fmt.Sprintf("auto: delete from %s", c.name)
		mainDS, dsErr := state.datasDB.GetDataset(ctx, mainDataset)
		if dsErr != nil {
			return nil, fmt.Errorf("auto-commit after delete: resolving main dataset: %w", dsErr)
		}
		if _, _, err := commitCollectionsAMAs(ctx, state.datasDB, mainDS, workingAMForAutoCommit, msg, "dumbodb <dumbodb@localhost>", time.Now()); err != nil {
			return nil, fmt.Errorf("auto-commit after delete: %w", err)
		}
	}

	return &backends.DeleteAllResult{Deleted: deleted}, nil
}

// Count implements backends.Collection.
//
// Unfiltered (params.Filter empty/nil) returns the entry count from prolly
// tree metadata in O(1).
//
// Filtered counts attempt an index-only path: when the filter is exactly one
// top-level field whose value shape produces sound index bounds (equality or
// supported range), and that field has a single-field secondary index, the
// count is the size of the index range  -- no documents are fetched or
// decoded. Filtered=true is set on the result. Any other shape (compound
// filter, dotted paths, $and/$or, no covering index, ...) is declined with
// Filtered=false and Count=0; the handler must scan.
func (c *collection) Count(ctx context.Context, params *backends.CountParams) (*backends.CountResult, error) {
	m, exists, state, err := c.getMap(ctx)
	if err != nil {
		return nil, err
	}

	if !exists {
		// Honor a filter on a missing collection by reporting 0 matches  --
		// avoids an unnecessary fallback scan in the handler.
		if params != nil && params.Filter != nil && params.Filter.Len() > 0 {
			return &backends.CountResult{Count: 0, Filtered: true}, nil
		}
		return &backends.CountResult{Count: 0}, nil
	}

	if params != nil && params.Filter != nil && params.Filter.Len() > 0 {
		n, used, ferr := c.tryIndexedCount(ctx, state, params.Filter)
		if ferr != nil {
			return nil, ferr
		}
		if used {
			return &backends.CountResult{Count: n, Filtered: true}, nil
		}
		// Decline: handler will scan.
		return &backends.CountResult{Count: 0, Filtered: false}, nil
	}

	n, err := m.Count()
	if err != nil {
		return nil, err
	}

	return &backends.CountResult{Count: int64(n), Filtered: true}, nil
}

// tryIndexedCount attempts to satisfy a filtered count from a single
// secondary index without fetching any documents. It returns (n, true, nil)
// only when the filter is exactly one top-level field with a value shape that
// produces sound bounds, and that field has a covering single-field index;
// any false positive in the bounds would corrupt the count, so the
// requirements are stricter than tryIndexLookup (which can rely on the
// handler's FilterIterator for re-validation).
//
// Returns (0, false, nil) when the filter shape is unsupported  -- caller
// falls back to the scan path.
func (c *collection) tryIndexedCount(ctx context.Context, state *dbState, filter *types.Document) (int64, bool, error) {
	if filter == nil || filter.Len() != 1 {
		return 0, false, nil
	}

	field := filter.Keys()[0]
	if strings.HasPrefix(field, "$") || strings.ContainsRune(field, '.') {
		return 0, false, nil
	}

	state.mu.RLock()
	idxInfos, idxMaps, err := resolveIndexes(ctx, c, state)
	state.mu.RUnlock()
	if err != nil {
		return 0, false, err
	}
	if len(idxInfos) == 0 {
		return 0, false, nil
	}

	var idxMap prolly.Map
	found := false
	for i, idx := range idxInfos {
		if len(idx.Key) == 1 && idx.Key[0].Field == field {
			idxMap = idxMaps[i]
			found = true
			break
		}
	}
	if !found {
		return 0, false, nil
	}

	v, err := filter.Get(field)
	if err != nil {
		return 0, false, nil
	}

	startKey, stopKey, ok := indexBoundsForFilterValue(v)
	if !ok {
		return 0, false, nil
	}

	// indexBoundsForFilterValue admits operator docs that combine bounds
	// like {$gte: 1, $lt: 10}. For a count those are still sound  -- the
	// returned range is exactly the matching index entries. But it also
	// admits the bare-scalar case where the index range may include
	// false positives if the index encoding loses information. Today
	// indexBoundsForFilterValue's bare-scalar path defers to
	// idxpkg.LowerBoundInclusive / UpperBoundInclusive which produce a
	// tight equality range, so the count is exact.
	n, err := idxpkg.RangeCount(ctx, idxMap, startKey, stopKey)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

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
				fmt.Errorf("database %q does not exist", c.db.name))
		}

		return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("collection %q does not exist", c.name))
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
		// Must be >= 250 so that for 4-doc test collections totalIndexSize/1000 >= 1.
		avgIndexEntSize = 256
		// minStoragePage is the minimum allocated storage for a non-empty collection,
		// matching MongoDB's page-granular allocation behavior (typically >= 4KB).
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

func (c *collection) Compact(ctx context.Context, params *backends.CompactParams) (*backends.CompactResult, error) {
	return &backends.CompactResult{}, nil
}

// DistinctScan implements backends.DistinctScanner.
//
// Walks the leading-field secondary index for `key` in sorted order,
// emitting one primary lookup per unique KeyString prefix to recover the
// original BSON value. Avoids reading every document and the O(n) primary
// fetches a full collection scan would do.
//
// Returns (nil, nil) when the request can't be served from an index  -- no
// matching single-field index, partial filter that would drop rows, dotted
// key, geospatial / text / hashed key. The handler falls back to Query in
// that case.
func (c *collection) DistinctScan(ctx context.Context, params *backends.DistinctParams) (*backends.DistinctResult, error) {
	if params == nil || params.Key == "" {
		return nil, nil
	}
	// Dotted paths address sub-document fields; the index is keyed on the
	// flat top-level field, so it can't answer the request soundly.
	if strings.ContainsRune(params.Key, '.') {
		return nil, nil
	}

	m, exists, state, err := c.getMap(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		return &backends.DistinctResult{}, nil
	}

	state.mu.RLock()
	idxInfos, idxMaps, err := resolveIndexes(ctx, c, state)
	state.mu.RUnlock()
	if err != nil {
		return nil, err
	}

	var (
		idxMap prolly.Map
		found  bool
	)
	for i, idx := range idxInfos {
		if len(idx.Key) != 1 {
			continue
		}
		kp := idx.Key[0]
		if kp.Field != params.Key {
			continue
		}
		// Hashed / text / geo indexes don't store the original value's
		// KeyString and can't drive a sorted distinct scan.
		if kp.Hashed || kp.Text || kp.Geo2D || kp.Geo2DSphere {
			continue
		}
		// Partial indexes only cover a subset of documents  -- a distinct
		// scan over them would silently drop values from non-matching docs.
		// Sparse indexes are fine because distinct already ignores missing
		// fields.
		if idx.PartialFilterExpression != nil {
			continue
		}
		idxMap = idxMaps[i]
		found = true
		break
	}
	if !found {
		return nil, nil
	}

	values, err := scanDistinctFromIndex(ctx, idxMap, m, state.ns, params.Key)
	if err != nil {
		return nil, err
	}

	return &backends.DistinctResult{Values: values}, nil
}

// scanDistinctFromIndex walks idxMap in key order and emits one primary lookup
// per unique KeyString prefix. The secondary index key layout is
// `[KeyString(value)][0x04][primaryID(20 bytes)]`; values with the same
// KeyString prefix share a distinct value, so a single doc fetch per group
// suffices to recover the original typed value.
func scanDistinctFromIndex(
	ctx context.Context,
	idxMap prolly.Map,
	primary prolly.Map,
	ns tree.NodeStore,
	field string,
) ([]any, error) {
	const primaryIDLen = 20

	iter, err := idxMap.IterAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("distinct scan iterating index: %w", err)
	}

	idxKeyDesc := idxpkg.KeyDescriptor()

	var (
		prevPrefix []byte
		havePrev   bool
		out        []any
	)

	for {
		k, _, err := iter.Next(ctx)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("distinct scan reading index: %w", err)
		}
		if k == nil {
			break
		}

		composite, ok := idxKeyDesc.GetBytes(0, k)
		if !ok {
			continue
		}
		if len(composite) < primaryIDLen+1 {
			continue
		}
		idStart := len(composite) - primaryIDLen
		if composite[idStart-1] != 0x04 {
			continue
		}
		prefix := composite[:idStart-1]

		if havePrev && bytes.Equal(prefix, prevPrefix) {
			continue
		}

		idBytes := make([]byte, primaryIDLen)
		copy(idBytes, composite[idStart:])

		val, err := lookupFieldFromPrimary(ctx, primary, ns, idBytes, field)
		if err != nil {
			return nil, err
		}
		// A nil val means the document is missing the indexed field  --
		// distinct ignores that case (matching FilterDistinctValues
		// semantics). Sparse indexes won't produce these entries; for
		// non-sparse indexes we still skip them.
		if val != types.Null && val != nil {
			out = append(out, val)
		}

		prevPrefix = append(prevPrefix[:0], prefix...)
		havePrev = true
	}

	return out, nil
}

// lookupFieldFromPrimary fetches the primary document by encoded _id bytes and
// returns the value of `field`. Returns types.Null if the field is missing.
func lookupFieldFromPrimary(
	ctx context.Context,
	primary prolly.Map,
	ns tree.NodeStore,
	idBytes []byte,
	field string,
) (any, error) {
	key, err := buildKey(idBytes)
	if err != nil {
		return nil, fmt.Errorf("distinct scan building key: %w", err)
	}

	var doc *types.Document
	if err := primary.Get(ctx, key, func(_, v val.Tuple) error {
		if v == nil {
			return nil
		}
		d, decErr := readDocFromValue(ctx, ns, v)
		if decErr != nil {
			return decErr
		}
		doc = d
		return nil
	}); err != nil {
		return nil, fmt.Errorf("distinct scan primary fetch: %w", err)
	}
	if doc == nil {
		return types.Null, nil
	}
	v, err := doc.Get(field)
	if err != nil {
		return types.Null, nil
	}
	return v, nil
}

func (c *collection) ListIndexes(ctx context.Context, params *backends.ListIndexesParams) (*backends.ListIndexesResult, error) {
	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, false)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("collection %q does not exist", c.name))
	}

	// Only the AM resolution needs state.mu (resolveAM reads state.workingSets).
	// The downstream chunk-store reads operate on immutable content-addressed
	// chunks and need no dbState lock.
	state.mu.RLock()
	am, err := c.db.resolveAM(ctx, state)
	state.mu.RUnlock()
	if err != nil {
		return nil, err
	}

	dtblHash, err := am.Get(ctx, c.name)
	if err != nil {
		return nil, err
	}
	if dtblHash.IsEmpty() {
		return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("collection %q does not exist", c.name))
	}

	idxAM, err := indexAMForDTBL(ctx, state.cs, state.ns, dtblHash)
	if err != nil {
		return nil, err
	}

	indexes := []backends.IndexInfo{
		{
			Name: backends.DefaultIndexName,
			Key:  []backends.IndexKeyPair{{Field: "_id", Descending: false}},
		},
	}

	if err := idxAM.IterAll(ctx, func(name string, entryHash hash.Hash) error {
		if entryHash.IsEmpty() {
			return nil
		}
		resolved, rerr := resolveIndexEntry(ctx, state.ns, entryHash)
		if rerr != nil {
			return rerr
		}
		indexes = append(indexes, resolved.info)
		return nil
	}); err != nil {
		return nil, err
	}

	slices.SortFunc(indexes, func(a, b backends.IndexInfo) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return &backends.ListIndexesResult{Indexes: indexes}, nil
}

func (c *collection) CreateIndexes(ctx context.Context, params *backends.CreateIndexesParams) (*backends.CreateIndexesResult, error) {
	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, true)
	if err != nil {
		return nil, err
	}

	if state == nil {
		return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("collection %q does not exist", c.name))
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	// Resolve the current per-branch index state from disk. Mutations
	// happen on a local copy; no shared dbState fields are touched.
	curInfos, curMaps, err := resolveBranchIndexState(ctx, c, state)
	if err != nil {
		return nil, fmt.Errorf("resolving current index state: %w", err)
	}

	infoByName := make(map[string]backends.IndexInfo, len(curInfos)+len(params.Indexes))
	for _, info := range curInfos {
		infoByName[info.Name] = info
	}

	for _, idx := range params.Indexes {
		if idx.Name == backends.DefaultIndexName {
			continue
		}
		if _, exists := infoByName[idx.Name]; exists {
			continue
		}
		infoByName[idx.Name] = idx
		// Build prolly.Map for the new index by scanning the primary map.
		idxMap, buildErr := c.buildSecondaryIndex(ctx, state, idx)
		if buildErr != nil {
			return nil, fmt.Errorf("building secondary index %q on %q: %w", idx.Name, c.name, buildErr)
		}
		curMaps[idx.Name] = idxMap
	}

	newInfos := make([]backends.IndexInfo, 0, len(infoByName))
	for _, info := range infoByName {
		newInfos = append(newInfos, info)
	}
	slices.SortFunc(newInfos, func(a, b backends.IndexInfo) int {
		return cmp.Compare(a.Name, b.Name)
	})

	newIdxAM, err := buildIndexAM(ctx, state, newInfos, curMaps)
	if err != nil {
		return nil, fmt.Errorf("building index AM: %w", err)
	}

	if err := c.rewriteDTBLAfterIndexChange(ctx, state, newIdxAM); err != nil {
		return nil, fmt.Errorf("rewriting DTBL for %q: %w", c.name, err)
	}

	return &backends.CreateIndexesResult{}, nil
}

// rewriteDTBLAfterIndexChange rebuilds the collection's DTBL with the
// given indexAM and updates the collections AM. Called after CreateIndexes
// / DropIndexes so an index-only change is durable.
//
// If the collection has no primary map yet (no inserts have happened) it
// uses a freshly created empty map  -- the same path as loadOrCreateMap.
//
// The caller must hold state.mu (write lock).
func (c *collection) rewriteDTBLAfterIndexChange(ctx context.Context, state *dbState, indexAM prolly.AddressMap) error {
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

	dtblHash, err := state.dtblHashForCollection(ctx, c.name, primary, indexAM, hash.Hash{})
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

		doc, err := readDocFromValue(ctx, state.ns, v)
		if err != nil {
			return prolly.Map{}, fmt.Errorf("index: reading document for build scan: %w", err)
		}

		fieldVals := extractIndexFieldValues(doc, idx)
		for _, fv := range expandMultiKeyValues(fieldVals) {
			if err := idxpkg.InsertEntry(ctx, mut, fv, idBytes); err != nil {
				return prolly.Map{}, fmt.Errorf("index: inserting entry: %w", err)
			}
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

// expandMultiKeyValues expands field values for multi-key indexing. If any
// field value is an array, one set of field values is returned per element.
// MongoDB allows at most one array field per compound index; we expand the
// first array encountered. Scalar-only inputs return a single entry.
func expandMultiKeyValues(fieldVals []any) [][]any {
	for i, v := range fieldVals {
		arr, ok := v.(*types.Array)
		if !ok || arr == nil {
			continue
		}
		if arr.Len() == 0 {
			// Empty array: index the array itself (matches MongoDB behavior
			// for queries like {field: {$size: 0}}).
			return [][]any{fieldVals}
		}
		expanded := make([][]any, arr.Len())
		for j := 0; j < arr.Len(); j++ {
			elem, _ := arr.Get(j)
			row := make([]any, len(fieldVals))
			copy(row, fieldVals)
			row[i] = elem
			expanded[j] = row
		}
		return expanded
	}
	return [][]any{fieldVals}
}

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

	curInfos, curMaps, err := resolveBranchIndexState(ctx, c, state)
	if err != nil {
		return nil, fmt.Errorf("resolving current index state: %w", err)
	}

	keptInfos := make([]backends.IndexInfo, 0, len(curInfos))
	for _, info := range curInfos {
		if _, remove := drop[info.Name]; remove {
			delete(curMaps, info.Name)
			continue
		}
		keptInfos = append(keptInfos, info)
	}

	newIdxAM, err := buildIndexAM(ctx, state, keptInfos, curMaps)
	if err != nil {
		return nil, fmt.Errorf("building index AM after drop: %w", err)
	}
	if err := c.rewriteDTBLAfterIndexChange(ctx, state, newIdxAM); err != nil {
		return nil, fmt.Errorf("rewriting DTBL after drop for %q: %w", c.name, err)
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

	emptyAM, err := emptyIndexAM(state.ns)
	if err != nil {
		return prolly.Map{}, err
	}
	dtblHash, err := state.dtblHashForCollection(ctx, c.name, emptyMap, emptyAM, hash.Hash{})
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

func readJSONBytesFromValue(ctx context.Context, ns tree.NodeStore, v val.Tuple) ([]byte, error) {
	return getBSONStoredBytes(ctx, ns, v)
}

func readDocFromValue(ctx context.Context, ns tree.NodeStore, v val.Tuple) (*types.Document, error) {
	return readBSONDocFromValue(ctx, ns, v)
}

func docToExtJSON(doc *types.Document) ([]byte, error) {
	return docToBSON(doc)
}

func writeDocToValue(ctx context.Context, ns tree.NodeStore, doc *types.Document) (val.Tuple, error) {
	return writeBSONDocToValue(ctx, ns, doc)
}

// applyFieldMutations dispatches on the adaptive-bytes storage shape:
// inline []byte takes the parse / mutate / re-encode path; out-of-band
// *val.ByteArray takes the byte-level splice path so unchanged chunks
// stay deduplicated.
func applyFieldMutations(ctx context.Context, ns tree.NodeStore, v val.Tuple, mutations []backends.FieldMutation) ([]byte, error) {
	result, ok, err := valDesc.GetBytesAdaptiveValue(ctx, 0, ns, v)
	if err != nil {
		return nil, fmt.Errorf("reading bytes value from tuple: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("value tuple missing bytes field")
	}

	switch existing := result.(type) {
	case []byte:
		return applyFieldMutationsInline(existing, mutations)
	case *val.ByteArray:
		b, err := existing.GetBytes(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading out-of-band bytes: %w", err)
		}
		return applyFieldMutationsOutOfBand(ctx, ns, b, mutations)
	default:
		return nil, fmt.Errorf("unexpected BytesAdaptiveValue type %T", result)
	}
}

func applyFieldMutationsInline(existingBytes []byte, mutations []backends.FieldMutation) ([]byte, error) {
	doc, err := bsonToDoc(existingBytes)
	if err != nil {
		return nil, fmt.Errorf("decoding inline document: %w", err)
	}
	if err := applyMutationsToDoc(doc, mutations); err != nil {
		return nil, err
	}
	return docToBSON(doc)
}

// applyFieldMutationsOutOfBand uses the byte-level splice path so each
// mutation patches only the affected container and its ancestor length
// prefixes; unchanged chunks stay deduplicated.
func applyFieldMutationsOutOfBand(ctx context.Context, ns tree.NodeStore, existingBytes []byte, mutations []backends.FieldMutation) ([]byte, error) {
	rawBSON, err := stripVersion(existingBytes)
	if err != nil {
		return nil, fmt.Errorf("decoding out-of-band document version: %w", err)
	}
	idx, err := bsonindexed.Serialize(ctx, ns, rawBSON)
	if err != nil {
		return nil, fmt.Errorf("serialising out-of-band document for mutation: %w", err)
	}
	for _, m := range mutations {
		if m.Unset {
			idx, err = idx.UnsetField(ctx, m.Key)
			if err != nil {
				return nil, fmt.Errorf("unset field %q: %w", m.Key, err)
			}
			continue
		}
		typeByte, valueBytes, err := encodeBSONValue(m.Value)
		if err != nil {
			return nil, fmt.Errorf("encoding mutation value for %q: %w", m.Key, err)
		}
		idx, err = idx.SetField(ctx, m.Key, typeByte, valueBytes)
		if err != nil {
			return nil, fmt.Errorf("set field %q: %w", m.Key, err)
		}
	}
	merged, err := idx.Bytes(ctx)
	if err != nil {
		return nil, fmt.Errorf("materialising mutated document: %w", err)
	}
	return prependVersion(merged), nil
}

// applyMutationsToDoc relies on the handler restricting Key to
// top-level field names (no dot paths).
func applyMutationsToDoc(doc *types.Document, mutations []backends.FieldMutation) error {
	for _, m := range mutations {
		if m.Unset {
			doc.Remove(m.Key)
			continue
		}
		doc.Set(m.Key, m.Value)
	}
	return nil
}

func decodeDocFromJSON(storedBytes []byte) (*types.Document, error) {
	return bsonToDoc(storedBytes)
}

func decodeDocument(data []byte) (*types.Document, error) {
	doc, err := bson.ToDocumentHandlingMinMaxKey(wirebson.RawDocument(data))
	if err != nil {
		return nil, fmt.Errorf("decoding document: %w", err)
	}
	if doc != nil {
		return doc, nil
	}
	doc, err = bson.ToDocument(wirebson.RawDocument(data))
	if err != nil {
		return nil, fmt.Errorf("decoding document: %w", err)
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
	// true means "may match  -- run the full filter downstream." A nil
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

		// Read JSON bytes from the JsonAdaptiveEnc value tuple.
		jsonBytes, err := readJSONBytesFromValue(it.ctx, it.ns, v)
		if err != nil {
			continue
		}

		var doc *types.Document
		if it.prefilter != nil {
			if !it.prefilter(jsonBytes) {
				continue
			}
		}
		doc, err = decodeDocFromJSON(jsonBytes)
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
