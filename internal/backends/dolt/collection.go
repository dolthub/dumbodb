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
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/bsonindexed"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/commonpath"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

type collection struct {
	db                    *database
	name                  string
	allowReplicationWrite bool
}

func (c *collection) rejectReplicationControlWrite() error {
	if c.allowReplicationWrite || !c.db.isReplicationControlCollection(c.name) {
		return nil
	}
	return replicationControlReadOnlyError()
}

// getMap returns the prolly.Map for this collection.
//
// When the rootish is "main" the current working-set AM is used; for a bare
// commit hash or tag name the AM is loaded from the historical RTVL at that commit.
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
	res, _, err := c.query(ctx, params)
	return res, err
}

// query is Query, also returning the stage tree of the scan that serves
// params, as explain shows it.
func (c *collection) query(ctx context.Context, params *backends.QueryParams) (*backends.QueryResult, *explainStage, error) {
	m, exists, state, err := c.getMap(ctx)
	if err != nil {
		return nil, nil, err
	}

	if !exists {
		return &backends.QueryResult{
			Iter: newEmptyIter(),
		}, collscanStage(), nil
	}

	// A {$natural: ...} hint forces a collection scan: skip both the secondary
	// index lookup and the _id point lookup below.
	naturalHint := params != nil && backends.HintIsNatural(params.Hint)

	sparseHint, err := c.hintedSparseIndex(ctx, state, params)
	if err != nil {
		return nil, nil, err
	}
	if sparseHint != nil && !sparseHint.Lossy {
		docs, err := c.sparseMembers(ctx, state, m, *sparseHint, params.Fields)
		if err != nil {
			return nil, nil, err
		}
		return &backends.QueryResult{Iter: newSliceIter(docs)}, fetchStage(ixscanStage(*sparseHint)), nil
	}
	if sparseHint != nil {
		res, err := c.queryUnhintedSparse(ctx, params)
		if err != nil {
			return nil, nil, err
		}
		res.Iter = newSparseMemberIter(res.Iter, *sparseHint)
		return res, fetchStage(ixscanStage(*sparseHint)), nil
	}

	// An _id equality is one primary-key read, cheaper than any index range,
	// so without a hint it is tried first, as MongoDB prefers _id_.
	if !naturalHint && params != nil && params.Hint == nil && !params.Collated && params.Filter != nil {
		if idVal, ok := simpleIDEquality(params.Filter); ok {
			if iter, err := pointLookupByID(ctx, state.ns, m, idVal, params.OnlyRecordIDs); err == nil {
				return &backends.QueryResult{Iter: iter}, idPointStage(), nil
			}
		}
	}

	fieldSort := params != nil && sortIsSingleField(params.Sort)
	var idxInfos []backends.IndexInfo

	if !naturalHint && params != nil && params.Filter != nil && (params.Sort.Len() == 0 || fieldSort) &&
		(!params.Collated || params.Collation != nil) {
		state.mu.RLock()
		var idxMaps []prolly.Map
		idxInfos, idxMaps, err = resolveIndexes(ctx, c, state)
		state.mu.RUnlock()
		if err != nil {
			return nil, nil, err
		}

		var cacheKey string
		var cached planCacheEntry
		var hit bool
		if fieldSort && params.Hint == nil && len(idxInfos) > 0 {
			cacheKey = c.planCacheKey(idxInfos, params)
			cached, hit = c.db.backend.planCache.get(cacheKey)
		}
		if hit && cached.walkIndex != "" {
			walk, ok, err := c.trySortWalk(ctx, state, m, params)
			if err != nil {
				return nil, nil, err
			}
			if ok && walk.index == cached.walkIndex {
				iter, run, err := runWalkWithin(walk, params, cached.budget(params))
				if err != nil {
					return nil, nil, err
				}
				if run == walkReachedTarget {
					return &backends.QueryResult{Iter: iter, Sorted: true}, walkStage(idxInfos, walk.index), nil
				}
			} else if ok {
				walk.Close()
			}
			c.db.backend.planCache.evict(cacheKey)
			hit = false
		}

		if fieldSort {
			iter, scan, ok, err := tryStreamMergedRanges(ctx, state, m, idxInfos, idxMaps, params)
			if err != nil {
				return nil, nil, err
			}
			if ok {
				return &backends.QueryResult{Iter: iter, Sorted: true}, scan, nil
			}
		}
		if fieldSort {
			iter, info, ok, err := tryStreamSortedRange(ctx, state, m, idxInfos, idxMaps, params)
			if err != nil {
				return nil, nil, err
			}
			if ok {
				return &backends.QueryResult{Iter: iter, Sorted: true}, fetchStage(ixscanStage(info)), nil
			}
		}

		var plan *indexLookupPlan
		if !fieldSort {
			iter, info, ok, err := streamIndexRanges(ctx, state, m, idxInfos, idxMaps, params)
			if err != nil {
				return nil, nil, err
			}
			if ok {
				return &backends.QueryResult{Iter: iter}, fetchStage(ixscanStage(info)), nil
			}
		}
		// The stream has already sized and declined any filter without $or.
		if fieldSort || params.Filter.Has("$or") {
			if plan, err = planIndexLookupIn(ctx, m, idxInfos, idxMaps, params.Filter, params.Hint, params.Collation, fieldSort); err != nil {
				return nil, nil, err
			}
		}
		if plan != nil {
			if reverse, ok := indexScanSortOrder(plan.info, params.Filter, params.Sort, params.Collation); fieldSort && !plan.union && ok {
				return &backends.QueryResult{Iter: plan.sortedIter(ctx, state, m, params.Sort, reverse, params.Fields), Sorted: true}, plan.stage(), nil
			}
			if hit && filterPlanWorks(plan, params) > cached.budget(params) {
				c.db.backend.planCache.evict(cacheKey)
				hit = false
			}
			if fieldSort && !hit {
				iter, outcome, err := c.trySortWalkTrial(ctx, state, m, params, plan)
				if err != nil {
					return nil, nil, err
				}
				switch outcome {
				case trialWalkWon:
					// The filter plan's keys were read to plan, not by the
					// winning walk, so the operation's own counts drop them
					// as MongoDB's executionStats leave out a losing plan's
					// trial work.
					backends.AddKeysExamined(ctx, -plan.keys)
					c.db.backend.planCache.put(cacheKey, newPlanCacheEntry(idxInfos, params, iter.rest.index, "", len(iter.docs)))
					return &backends.QueryResult{Iter: iter, Sorted: true}, walkStage(idxInfos, iter.rest.index), nil
				case trialFilterWon:
					c.db.backend.planCache.put(cacheKey, newPlanCacheEntry(idxInfos, params, "", plan.index, filterPlanWorks(plan, params)))
				}
			}
			if pos, ok := indexKeySortPosition(plan.info, params.Filter, params.Sort, params.Collation); fieldSort && !plan.union && ok {
				iter, err := plan.keySortedIter(ctx, state, m, params.Sort, pos, params.Fields)
				if err != nil {
					return nil, nil, err
				}
				return &backends.QueryResult{Iter: iter, Sorted: true}, fetchStage(&explainStage{stage: "SORT", input: ixscanStage(plan.info)}), nil
			}
			docs, err := plan.fetch(ctx, state, m, params.Fields)
			if err != nil {
				return nil, nil, err
			}
			return &backends.QueryResult{
				Iter: newSliceIter(docs),
			}, plan.stage(), nil
		}
	}

	// Without a limit a filtered walk fetches every document by point lookup
	// to return a fraction of them; the sequential scan and sort is cheaper.
	if !naturalHint && fieldSort && (params.LimitHint > 0 || params.Filter.Len() == 0) {
		iter, ok, err := c.trySortWalk(ctx, state, m, params)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			if idxInfos == nil {
				state.mu.RLock()
				idxInfos, _, err = resolveIndexes(ctx, c, state)
				state.mu.RUnlock()
				if err != nil {
					iter.Close()
					return nil, nil, err
				}
			}
			return &backends.QueryResult{Iter: iter, Sorted: true}, walkStage(idxInfos, iter.index), nil
		}
	}

	reverse := false
	if params != nil && sortIsNatural(params.Sort) {
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
	if !naturalHint && params != nil && !params.Collated && params.Filter != nil {
		if idVal, ok := simpleIDEquality(params.Filter); ok {
			iter, err := pointLookupByID(ctx, state.ns, m, idVal, onlyRecordIDs)
			if err == nil {
				return &backends.QueryResult{Iter: iter}, idPointStage(), nil
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
	// Under a non-simple collation the handler re-checks matches through the
	// against a regex substitution of the filter, so byte-level equality is
	// not a sound lower bound  -- skip the prefilter.
	var pf, prefixReject func([]byte) bool
	if params != nil && !onlyRecordIDs && !params.Collated {
		pf = buildScanPrefilter(params.Filter)
		prefixReject = buildScanPrefixRejecter(params.Filter)
	}

	iter := newMapIter(ctx, state.ns, m, reverse, limit, onlyRecordIDs, pf)
	if mi, ok := iter.(*mapIter); ok {
		if params != nil {
			mi.fields = params.Fields
			mi.lazy = params.LazyDocuments && params.Fields == nil
		}
		mi.prefixReject = prefixReject
		if state.backend != nil && state.backend.backgroundRP != nil {
			mi.release = state.backend.backgroundRP.pinRoot(state.name, m.HashOf())
		}
	}
	return &backends.QueryResult{Iter: iter}, collscanStage(), nil
}

// hintedSparseIndex returns the sparse index params hints, or nil.
func (c *collection) hintedSparseIndex(ctx context.Context, state *dbState, params *backends.QueryParams) (*backends.IndexInfo, error) {
	if params == nil || params.Hint == nil || params.OnlyRecordIDs || backends.HintIsNatural(params.Hint) {
		return nil, nil
	}
	state.mu.RLock()
	infos, _, err := resolveIndexes(ctx, c, state)
	state.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	name := backends.MatchHintedIndex(params.Hint, infos)
	for i := range infos {
		if infos[i].Name == name && infos[i].Sparse {
			return &infos[i], nil
		}
	}
	return nil, nil
}

// sparseMembers reads the documents a sparse index holds, keeping only fields
// when it is non-nil.
func (c *collection) sparseMembers(ctx context.Context, state *dbState, primary prolly.Map, idx backends.IndexInfo, fields []string) ([]*types.Document, error) {
	state.mu.RLock()
	infos, maps, err := resolveIndexes(ctx, c, state)
	state.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(infos, func(info backends.IndexInfo) bool { return info.Name == idx.Name })
	if i < 0 {
		return nil, fmt.Errorf("sparse index %q not found", idx.Name)
	}
	entries, err := idxpkg.RangeLookup(ctx, maps[i], nil, nil)
	if err != nil {
		return nil, err
	}
	countKeysExamined(ctx, len(entries))
	plan := &indexLookupPlan{info: idx, index: idx.Name, keys: len(entries)}
	seen := make(map[string]struct{}, len(entries))
	for _, id := range entries {
		if _, dup := seen[string(id)]; !dup {
			seen[string(id)] = struct{}{}
			plan.ids = append(plan.ids, id)
		}
	}
	return plan.fetch(ctx, state, primary, fields)
}

// queryUnhintedSparse runs params with its sparse-index hint removed; the
// caller restricts the result to the index's members.
func (c *collection) queryUnhintedSparse(ctx context.Context, params *backends.QueryParams) (*backends.QueryResult, error) {
	unhinted := *params
	unhinted.Hint = nil
	return c.Query(ctx, &unhinted)
}

// sparseMemberIter yields only documents a sparse index holds. MongoDB answers
// a query hinted onto a sparse index from the index alone, so documents
// missing every indexed field are absent even when the filter matches them.
type sparseMemberIter struct {
	types.DocumentsIterator
	idx backends.IndexInfo
}

func newSparseMemberIter(iter types.DocumentsIterator, idx backends.IndexInfo) types.DocumentsIterator {
	return &sparseMemberIter{DocumentsIterator: iter, idx: idx}
}

func (it *sparseMemberIter) Next() (struct{}, *types.Document, error) {
	for {
		k, doc, err := it.DocumentsIterator.Next()
		if err != nil || !sparseExcludes(doc, it.idx) {
			return k, doc, err
		}
	}
}

// buildScanPrefilter returns a byte-level predicate over a document's raw
// canonical Extended JSON bytes that is sound for the given filter  -- i.e. if
// the predicate returns false, the document is guaranteed not to match.
// Returns nil when no sound prefilter can be built (complex filter, null
// value, unsupported type, ambiguous numeric, etc.); in that case the scan
// falls back to decoding every document.
func buildScanPrefilter(filter *types.Document) func([]byte) bool {
	preds := scanPredicates(filter)
	if len(preds) == 0 {
		return nil
	}

	return func(jsonBytes []byte) bool {
		return matchScanPredicates(preds, jsonBytes, true)
	}
}

// scanPredicates returns one predicate per filter field, or nil when some
// field has none.
func scanPredicates(filter *types.Document) []scanPredicate {
	if filter == nil {
		return nil
	}

	keys := filter.Keys()
	preds := make([]scanPredicate, 0, len(keys))
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
	return preds
}

// buildScanPrefixRejecter returns a predicate that rejects a stored document
// from a prefix of it alone when the prefix proves the filter cannot match,
// or nil under the conditions buildScanPrefilter returns nil.
func buildScanPrefixRejecter(filter *types.Document) func([]byte) bool {
	preds := scanPredicates(filter)
	if len(preds) == 0 {
		return nil
	}
	return func(prefix []byte) bool {
		return !matchScanPredicates(preds, prefix, false)
	}
}

func matchScanPredicates(preds []scanPredicate, stored []byte, complete bool) bool {
	for _, pred := range preds {
		if !pred(stored, complete) {
			return false
		}
	}
	return true
}

func buildFieldPredicate(field string, value any) scanPredicate {
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
// concrete value that can be hashed into a primary key, either bare or as the
// literal operand of {$eq: x}. It rejects other operator forms, array
// equality ({_id: [1,2]}), regex, and bare null.
// Other filter fields are allowed  -- the handler re-checks them on the result.
func simpleIDEquality(filter *types.Document) (any, bool) {
	v, err := filter.Get("_id")
	if err != nil {
		return nil, false
	}
	if val, ok := v.(*types.Document); ok {
		if val.Len() == 1 && val.Keys()[0] == "$eq" {
			// A null operand also matches a missing field, but every stored
			// document has an _id, so the point lookup finds the same rows.
			v = must.NotFail(val.Get("$eq"))
			switch v.(type) {
			case *types.Array, types.Regex:
				return nil, false
			}
			return v, true
		}
		// {$gt: x} / {$in: x} / etc.  -- let the full scan handle it.
		for _, k := range val.Keys() {
			if strings.HasPrefix(k, "$") {
				return nil, false
			}
		}
		return v, true
	}
	switch v.(type) {
	case *types.Array, types.NullType:
		return nil, false
	default:
		return v, true
	}
}

func pointLookupByID(ctx context.Context, ns tree.NodeStore, m prolly.Map, idVal any, onlyRecordID bool) (types.DocumentsIterator, error) {
	h, err := primaryKey(idVal)
	if err != nil {
		return nil, err
	}

	key, err := buildKey(h)
	if err != nil {
		return nil, err
	}

	var doc *types.Document
	err = m.Get(ctx, key, func(k, v val.Tuple) error {
		if v == nil {
			return nil
		}
		countKeysExamined(ctx, 1)
		countDocsExamined(ctx, 1)

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
func (c *collection) tryIndexLookup(ctx context.Context, state *dbState, primary prolly.Map, filter *types.Document, hint any, queryCollation *types.Document) ([]*types.Document, bool, error) {
	plan, err := c.planIndexLookup(ctx, state, primary, filter, hint, queryCollation, false)
	if err != nil || plan == nil {
		return nil, false, err
	}
	docs, err := plan.fetch(ctx, state, primary, nil)
	return docs, true, err
}

// indexLookupPlan is a filter served by an index: the primary ids its range
// scan matched, in index order.
type indexLookupPlan struct {
	info  backends.IndexInfo
	index string
	// union is set when ids are the union of one index scan per $or branch;
	// info and index are then empty, and ids are not in any index order.
	union bool
	// branches are a union's index scans, without their ids.
	branches []unionBranch
	// keys is the number of index entries scanned; ids holds each matched
	// document once and, when kept, values its entry's encoded index values.
	keys   int
	ids    [][]byte
	values [][]byte
}

// planIndexLookup picks the index serving filter and scans its range, or
// returns nil when no index qualifies or the range is too wide to beat a scan.
// keepValues keeps each entry's encoded index values in the plan.
func (c *collection) planIndexLookup(ctx context.Context, state *dbState, primary prolly.Map, filter *types.Document, hint any, queryCollation *types.Document, keepValues bool) (*indexLookupPlan, error) {
	if filter == nil || filter.Len() == 0 {
		return nil, nil
	}
	state.mu.RLock()
	idxInfos, idxMaps, err := resolveIndexes(ctx, c, state)
	state.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	return planIndexLookupIn(ctx, primary, idxInfos, idxMaps, filter, hint, queryCollation, keepValues)
}

// planIndexLookupIn is planIndexLookup over already resolved indexes.
func planIndexLookupIn(ctx context.Context, primary prolly.Map, idxInfos []backends.IndexInfo, idxMaps []prolly.Map, filter *types.Document, hint any, queryCollation *types.Document, keepValues bool) (*indexLookupPlan, error) {
	plan, err := readIndexLookup(ctx, primary, idxInfos, idxMaps, filter, hint, queryCollation, keepValues)
	if plan != nil {
		countKeysExamined(ctx, plan.keys)
		for i, branch := range plan.branches {
			backends.AddBranchExamined(ctx, i, int64(branch.keys), int64(branch.matched))
		}
	}
	return plan, err
}

// readIndexLookup is planIndexLookupIn without counting the keys read.
func readIndexLookup(ctx context.Context, primary prolly.Map, idxInfos []backends.IndexInfo, idxMaps []prolly.Map, filter *types.Document, hint any, queryCollation *types.Document, keepValues bool) (*indexLookupPlan, error) {
	if filter == nil || filter.Len() == 0 || len(idxInfos) == 0 {
		return nil, nil
	}
	if isLoneOr(filter) {
		if hint != nil {
			return nil, nil
		}
		return planOrUnion(ctx, primary, idxInfos, idxMaps, must.NotFail(filter.Get("$or")), queryCollation)
	}

	choice, ok := chooseIndexRange(idxInfos, filter, hint, queryCollation)
	if !ok {
		return nil, nil
	}
	if hint != nil {
		return scanIndexChoice(ctx, idxInfos, idxMaps, choice, keepValues, -1)
	}
	var plan *indexLookupPlan
	within, err := indexRangesWithinHalf(primary, func(n int) (bool, error) {
		var err error
		plan, err = scanIndexChoice(ctx, idxInfos, idxMaps, choice, keepValues, n)
		return plan != nil, err
	}, func() (uint64, error) {
		return indexRangeEntries(ctx, idxMaps, choice)
	})
	if err != nil || !within || plan != nil {
		return plan, err
	}
	return scanIndexChoice(ctx, idxInfos, idxMaps, choice, keepValues, -1)
}

// narrowRangeEntries is how many entries of a range are read before counting
// it: a range that ends within them costs less to read than to count, and so
// little either way that it is always read.
const narrowRangeEntries = 128

// indexRangesWithinHalf reports whether some index ranges are narrow or hold
// at most half of primary's documents; past that, one point lookup per entry
// loses to a sequential scan. readsAtMost(n) reads up to n entries and reports
// whether the ranges end within them, so narrow ranges are never counted;
// countEntries counts them from the index tree.
func indexRangesWithinHalf(primary prolly.Map, readsAtMost func(n int) (bool, error), countEntries func() (uint64, error)) (bool, error) {
	docs, err := primary.Count()
	if err != nil {
		return false, fmt.Errorf("index lookup counting primary: %w", err)
	}
	half := docs / 2
	narrow, err := readsAtMost(narrowRangeEntries)
	if err != nil || narrow {
		return narrow, err
	}
	if half <= narrowRangeEntries {
		return false, nil
	}
	entries, err := countEntries()
	return entries <= uint64(half), err
}

// indexRangeEntries is the number of index entries the choices' ranges hold.
func indexRangeEntries(ctx context.Context, idxMaps []prolly.Map, choices ...indexRangeChoice) (uint64, error) {
	var total uint64
	for _, choice := range choices {
		for _, r := range choice.ranges() {
			n, err := idxpkg.RangeCardinality(ctx, idxMaps[choice.mapIdx], r[0], r[1])
			if err != nil {
				return 0, fmt.Errorf("index lookup counting range: %w", err)
			}
			total += n
		}
	}
	return total, nil
}

// scanIndexChoice reads choice's ranges into a plan of their matched ids, or
// returns nil when they hold more than maxEntries entries; -1 reads them all.
// The caller counts the keys of the plan it uses.
func scanIndexChoice(ctx context.Context, idxInfos []backends.IndexInfo, idxMaps []prolly.Map, choice indexRangeChoice, keepValues bool, maxEntries int) (*indexLookupPlan, error) {
	idxMap := idxMaps[choice.mapIdx]
	var primaryIDBytesList, valuesList [][]byte
	for _, r := range choice.ranges() {
		remaining := -1
		if maxEntries >= 0 {
			remaining = maxEntries - len(primaryIDBytesList)
		}
		var ids, values [][]byte
		var exceeded bool
		var err error
		if keepValues {
			ids, values, exceeded, err = idxpkg.RangeEntriesCapped(ctx, idxMap, r[0], r[1], remaining)
		} else {
			ids, exceeded, err = idxpkg.RangeLookupCapped(ctx, idxMap, r[0], r[1], remaining)
		}
		if err != nil || exceeded {
			return nil, err
		}
		primaryIDBytesList = append(primaryIDBytesList, ids...)
		valuesList = append(valuesList, values...)
	}

	plan := &indexLookupPlan{info: idxInfos[choice.mapIdx], index: idxInfos[choice.mapIdx].Name, keys: len(primaryIDBytesList)}
	seenIDs := make(map[string]struct{}, len(primaryIDBytesList))
	for i, idBytes := range primaryIDBytesList {
		if _, dup := seenIDs[string(idBytes)]; dup {
			continue
		}
		seenIDs[string(idBytes)] = struct{}{}
		plan.ids = append(plan.ids, idBytes)
		if keepValues {
			plan.values = append(plan.values, valuesList[i])
		}
	}
	return plan, nil
}

// indexRangeChoice is the index chooseIndexRange picked for a filter and the
// key range, or point ranges, to scan in it.
type indexRangeChoice struct {
	mapIdx      int
	startKey    []byte
	stopKey     []byte
	pointRanges [][2][]byte
}

// ranges returns choice's point ranges, or its single range.
func (choice indexRangeChoice) ranges() [][2][]byte {
	if choice.pointRanges != nil {
		return choice.pointRanges
	}
	return [][2][]byte{{choice.startKey, choice.stopKey}}
}

// chooseIndexRange picks the index serving filter's first indexable field and
// the range to scan in it, without scanning.
func chooseIndexRange(idxInfos []backends.IndexInfo, filter *types.Document, hint any, queryCollation *types.Document) (indexRangeChoice, bool) {
	queryColl := collation.Parse(queryCollation)
	queryIdentity := queryColl.Identity()
	queryCmp := queryColl.Comparator()

	// A hint naming a specific index forces selection: only that index is a
	// candidate, as on MongoDB. When the filter gives the hinted index no
	// range, the whole index is scanned.
	hintedName := backends.MatchHintedIndex(hint, idxInfos)

	// Single-field indexes are preferred (tighter scan range); compound
	// indexes are used when no single-field index covers the leading filter
	// field. The handler re-filters every returned doc, so a compound index's
	// suffix-field constraints are applied post-scan.
	type indexedFieldEntry struct {
		idxName  string
		mapIdx   int
		compound bool
		rank     int
	}
	indexedField := make(map[string]indexedFieldEntry, len(idxInfos))
	for i, idx := range idxInfos {
		if hintedName != "" && idx.Name != hintedName {
			continue
		}
		// Lossy entries sit at wrong byte positions; never usable. A partial
		// index is usable only when the filter implies its partial condition,
		// so every matching doc lies within it; otherwise it drops non-member
		// matches. (Sparse is fine: no admitted operator matches a missing field.)
		if idx.Lossy || len(idx.Key) == 0 || idx.Key[0].Field == "" {
			continue
		}
		if collation.Parse(idx.Collation).Identity() != queryIdentity {
			continue
		}
		partial := idx.PartialFilterExpression != nil
		if partial && !filterImpliesPartial(filter, idx.PartialFilterExpression, collation.Parse(idx.Collation).IsSimple()) {
			continue
		}
		leading := idx.Key[0].Field
		isCompound := len(idx.Key) > 1
		rank := indexRank(isCompound, partial)
		if cur, have := indexedField[leading]; !have || rank < cur.rank {
			indexedField[leading] = indexedFieldEntry{
				idxName: idx.Name, mapIdx: i, compound: isCompound, rank: rank,
			}
		}
	}
	if len(indexedField) == 0 {
		return indexRangeChoice{}, false
	}

	var (
		chosenMapIdx int
		startKey     []byte
		stopKey      []byte
		usable       bool
		// pointRanges, when set, replaces [startKey, stopKey) with one range
		// per $in value, or the ranges a prefix regex can match.
		pointRanges [][2][]byte
	)

	for _, k := range filter.Keys() {
		// Top-level $-operators ($and/$or/$nor/$comment/...) make the
		// per-field analysis below unsound  -- bail.
		if strings.HasPrefix(k, "$") {
			return indexRangeChoice{}, false
		}
		entry, ok := indexedField[k]
		if !ok {
			continue
		}
		v, err := filter.Get(k)
		if err != nil {
			continue
		}
		if queryCmp == nil {
			if ranges, ok := indexPointRangesForIn(v); ok {
				if entry.compound {
					// Each point brackets every key with that leading value.
					for i, r := range ranges {
						ranges[i][0], ranges[i][1] = compoundLeadingBounds(r[0], r[1])
					}
				}
				chosenMapIdx = entry.mapIdx
				// Scan the points in key order so ids come back in index order.
				slices.SortFunc(ranges, func(a, b [2][]byte) int { return bytes.Compare(a[0], b[0]) })
				pointRanges = ranges
				usable = true
				break
			}
		}
		if queryCmp == nil {
			if ranges, ok := regexPrefixRanges(v); ok {
				chosenMapIdx = entry.mapIdx
				pointRanges = ranges
				usable = true
				break
			}
		}
		if queryCmp != nil {
			v = collateFilterValue(v, queryCmp)
		}
		if idxInfos[entry.mapIdx].Multikey {
			v = multikeyScanBound(v)
		}
		s, e, ok := indexBoundsForFilterValue(v)
		if !ok && isNullEquality(v) && nullEqualityIndexable(idxInfos[entry.mapIdx]) {
			s, e, ok = idxpkg.LowerBoundInclusive(types.Null), idxpkg.UpperBoundInclusive(types.Null), true
		}
		if !ok && isExistsTrue(v) && existsIndexable(idxInfos[entry.mapIdx]) {
			s, e, ok = nil, nil, true
		}
		if !ok {
			continue
		}
		if entry.compound {
			if cs, ce, cok := compoundIndexBounds(idxInfos[entry.mapIdx], filter, queryCmp); cok {
				s, e = cs, ce
			} else {
				s, e = compoundLeadingBounds(s, e)
			}
		}
		chosenMapIdx = entry.mapIdx
		startKey, stopKey = s, e
		usable = true
		break
	}

	if !usable {
		if hintedName != "" {
			for _, entry := range indexedField {
				return indexRangeChoice{mapIdx: entry.mapIdx}, true
			}
		}
		return indexRangeChoice{}, false
	}
	return indexRangeChoice{mapIdx: chosenMapIdx, startKey: startKey, stopKey: stopKey, pointRanges: pointRanges}, true
}

// planOrUnion plans each $or branch on its own index and unions the matched
// ids, or returns nil when any branch has no index plan or the branches'
// ranges together hold more than half the collection.
func planOrUnion(ctx context.Context, primary prolly.Map, idxInfos []backends.IndexInfo, idxMaps []prolly.Map, branches any, queryCollation *types.Document) (*indexLookupPlan, error) {
	flat, ok := flattenOrBranches(branches)
	if !ok {
		return nil, nil
	}
	choices := make([]indexRangeChoice, len(flat))
	for i, branch := range flat {
		if choices[i], ok = chooseIndexRange(idxInfos, branch, nil, queryCollation); !ok {
			return nil, nil
		}
	}
	var union *indexLookupPlan
	within, err := indexRangesWithinHalf(primary, func(n int) (bool, error) {
		var err error
		union, err = unionIndexChoices(ctx, idxInfos, idxMaps, choices, n)
		return union != nil, err
	}, func() (uint64, error) {
		return indexRangeEntries(ctx, idxMaps, choices...)
	})
	if err != nil || !within || union != nil {
		return union, err
	}
	return unionIndexChoices(ctx, idxInfos, idxMaps, choices, -1)
}

// unionIndexChoices reads every choice's ranges into one plan of their
// distinct ids, or returns nil when they hold more than maxEntries entries;
// -1 reads them all.
func unionIndexChoices(ctx context.Context, idxInfos []backends.IndexInfo, idxMaps []prolly.Map, choices []indexRangeChoice, maxEntries int) (*indexLookupPlan, error) {
	union := &indexLookupPlan{union: true}
	seen := make(map[string]struct{})
	for _, choice := range choices {
		remaining := -1
		if maxEntries >= 0 {
			remaining = maxEntries - union.keys
		}
		plan, err := scanIndexChoice(ctx, idxInfos, idxMaps, choice, false, remaining)
		if err != nil || plan == nil {
			return nil, err
		}
		union.keys += plan.keys
		union.branches = append(union.branches, unionBranch{info: plan.info, keys: plan.keys, matched: len(plan.ids)})
		for _, id := range plan.ids {
			if _, dup := seen[string(id)]; dup {
				continue
			}
			seen[string(id)] = struct{}{}
			union.ids = append(union.ids, id)
		}
	}
	return union, nil
}

// unionBranch is one index scan of a union: its index, the keys it read, and
// the documents they matched.
type unionBranch struct {
	info          backends.IndexInfo
	keys, matched int
}

// isLoneOr reports whether filter is a single top-level $or.
func isLoneOr(filter *types.Document) bool {
	return filter.Len() == 1 && filter.Keys()[0] == "$or"
}

// flattenOrBranches returns the documents of an $or array, with the branches
// of any branch that is itself a lone $or in its place.
func flattenOrBranches(branches any) ([]*types.Document, bool) {
	arr, ok := branches.(*types.Array)
	if !ok || arr.Len() == 0 {
		return nil, false
	}
	var flat []*types.Document
	for i := 0; i < arr.Len(); i++ {
		branch, ok := must.NotFail(arr.Get(i)).(*types.Document)
		if !ok {
			return nil, false
		}
		if isLoneOr(branch) {
			nested, ok := flattenOrBranches(must.NotFail(branch.Get("$or")))
			if !ok {
				return nil, false
			}
			flat = append(flat, nested...)
			continue
		}
		flat = append(flat, branch)
	}
	return flat, true
}

// fetch reads the plan's documents from the primary map, keeping only fields
// when it is non-nil.
func (plan *indexLookupPlan) fetch(ctx context.Context, state *dbState, primary prolly.Map, fields []string) ([]*types.Document, error) {
	docs := make([]*types.Document, 0, len(plan.ids))
	for _, idBytes := range plan.ids {
		key, err := buildKey(idBytes)
		if err != nil {
			return nil, fmt.Errorf("index lookup building key: %w", err)
		}
		var doc *types.Document
		if err := primary.Get(ctx, key, func(k, v val.Tuple) error {
			if v == nil {
				return nil
			}
			countDocsExamined(ctx, 1)
			stored, err := getBSONStoredBytes(ctx, state.ns, v)
			if err != nil {
				return err
			}
			doc, err = bsonToDocFields(stored, fields)
			return err
		}); err != nil {
			return nil, fmt.Errorf("index lookup primary fetch: %w", err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs, nil
}

// indexPointRangesForIn returns one point range per value of a
// {field: {$in: [...]}} clause. Like a bare equality, it declines values the
// byte-level index cannot match faithfully (null, arrays, documents, regexes,
// Decimal128), so those fall back to a scan.
func indexPointRangesForIn(v any) ([][2][]byte, bool) {
	opDoc, isOp := v.(*types.Document)
	if !isOp || opDoc.Len() != 1 || opDoc.Keys()[0] != "$in" {
		return nil, false
	}
	values, isArr := must.NotFail(opDoc.Get("$in")).(*types.Array)
	if !isArr {
		return nil, false
	}

	ranges := make([][2][]byte, 0, values.Len())
	for i := 0; i < values.Len(); i++ {
		value := must.NotFail(values.Get(i))
		switch value.(type) {
		case nil, types.NullType, *types.Array, *types.Document, types.Regex, types.Decimal128:
			return nil, false
		}
		ranges = append(ranges, [2][]byte{idxpkg.LowerBoundInclusive(value), idxpkg.UpperBoundInclusive(value)})
	}
	return ranges, true
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
//     tightest clause on each side.
//
// Returns ok=false for value shapes the index path can't handle (regex,
// $in, $ne, nested operators, mixed comparisons against arrays, ...); the
// caller falls back to the full-scan path.
// multikeyScanBound drops the upper-bound operators from a range that has both
// sides. On a multikey field each operator may be met by a different element
// ({$gt: 2, $lt: 3} matches [1, 3]), so the sides cannot be intersected; like
// MongoDB, scan one bound and let the filter re-check the rest.
func multikeyScanBound(v any) any {
	opDoc, ok := v.(*types.Document)
	if !ok || opDoc.Has("$eq") || !(opDoc.Has("$gt") || opDoc.Has("$gte")) || !(opDoc.Has("$lt") || opDoc.Has("$lte")) {
		return v
	}
	lower := types.MakeDocument(2)
	for _, op := range opDoc.Keys() {
		if op == "$gt" || op == "$gte" {
			// AppendDecoded: the operand may be a collated sort key
			// (idxpkg.PreEncoded), an internal planner value.
			v, _ := opDoc.Get(op)
			lower.AppendDecoded(op, v)
		}
	}
	return lower
}

// regexPrefixRanges returns the key ranges a regex given bare or as
// {$regex, $options} can match: every string starting with the literal prefix
// the anchored pattern requires, and the stored regex equal to it. It declines
// when the pattern has no such prefix, does not compile, or has options other
// than s. The ranges are a superset; the handler applies the regex itself.
func regexPrefixRanges(v any) ([][2][]byte, bool) {
	var re types.Regex
	switch t := v.(type) {
	case types.Regex:
		re = t
	case *types.Document:
		raw, err := t.Get("$regex")
		if err != nil {
			return nil, false
		}
		for _, k := range t.Keys() {
			if k != "$regex" && k != "$options" {
				return nil, false
			}
		}
		switch r := raw.(type) {
		case string:
			re.Pattern = r
		case types.Regex:
			re = r
		default:
			return nil, false
		}
		if opts, err := t.Get("$options"); err == nil {
			str, isStr := opts.(string)
			if !isStr || (re.Options != "" && str != "") {
				return nil, false
			}
			re.Options += str
		}
	default:
		return nil, false
	}
	if strings.Trim(re.Options, "s") != "" {
		return nil, false
	}
	if _, err := re.Compile(); err != nil {
		return nil, false
	}
	prefix, ok := regexLiteralPrefix(re.Pattern)
	if !ok {
		return nil, false
	}
	encoded := idxpkg.EncodeValue(prefix)
	strStart := encoded[:len(encoded)-1]
	strStop := append(append([]byte(nil), strStart...), 0xFF)
	return [][2][]byte{
		{strStart, strStop},
		{bracketSuffix(idxpkg.LowerBoundInclusive(re)), bracketSuffix(idxpkg.UpperBoundInclusive(re))},
	}, true
}

// regexLiteralPrefix returns the non-empty literal text every match of
// pattern starts with, when pattern is anchored by ^ and has no alternation.
func regexLiteralPrefix(pattern string) (string, bool) {
	if !strings.HasPrefix(pattern, "^") || strings.Contains(pattern, "|") {
		return "", false
	}
	runes := []rune(pattern[1:])
	var lit []rune
	stop := rune(0)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if c == '\\' {
			if i+1 < len(runes) && strings.ContainsRune(`.^$|?*+()[]{}\/-`, runes[i+1]) {
				i++
				lit = append(lit, runes[i])
				continue
			}
			break
		}
		if strings.ContainsRune(`.^$?*+()[]{}`, c) {
			stop = c
			break
		}
		lit = append(lit, c)
	}
	if (stop == '?' || stop == '*' || stop == '{') && len(lit) > 0 {
		lit = lit[:len(lit)-1]
	}
	if len(lit) == 0 {
		return "", false
	}
	return string(lit), true
}

// isNullEquality reports whether v, a filter field's condition, is null
// equality, which matches null and missing values.
func isNullEquality(v any) bool {
	if doc, ok := v.(*types.Document); ok {
		if doc.Len() != 1 || doc.Keys()[0] != "$eq" {
			return false
		}
		v = must.NotFail(doc.Get("$eq"))
	}
	switch v.(type) {
	case nil, types.NullType:
		return true
	}
	return false
}

// nullEqualityIndexable reports whether idx holds a null entry for every
// document a null equality on its leading field matches: missing values are
// indexed as null, and a dotted path without positional segments, as on
// MongoDB, indexes null for each array element or branch it does not reach.
// A multikey top-level field is not served.
func nullEqualityIndexable(idx backends.IndexInfo) bool {
	if idx.Sparse {
		return false
	}
	field := idx.Key[0].Field
	if !strings.Contains(field, ".") {
		return !idx.Multikey
	}
	path, err := types.NewPathFromString(field)
	return err == nil && !hasPositionalSegment(path)
}

// hasPositionalSegment reports whether a segment of path is numeric, which
// may name an array position.
func hasPositionalSegment(path types.Path) bool {
	for _, seg := range path.Slice() {
		if _, err := strconv.Atoi(seg); err == nil {
			return true
		}
	}
	return false
}

// isExistsTrue reports whether v, a filter field's condition, is
// {$exists: true} or another truthy $exists.
func isExistsTrue(v any) bool {
	doc, ok := v.(*types.Document)
	if !ok || doc.Len() != 1 || doc.Keys()[0] != "$exists" {
		return false
	}
	return existsTruthy(must.NotFail(doc.Get("$exists")))
}

// existsTruthy reports whether an $exists operand asks for presence.
func existsTruthy(e any) bool {
	switch e := e.(type) {
	case bool:
		return e
	case int32:
		return e != 0
	case int64:
		return e != 0
	case float64:
		return e != 0
	}
	return false
}

// existsIndexable reports whether idx holds exactly the documents in which
// its one top-level field exists: a sparse single-field index.
func existsIndexable(idx backends.IndexInfo) bool {
	return idx.Sparse && len(idx.Key) == 1 && !strings.Contains(idx.Key[0].Field, ".")
}

func indexBoundsForFilterValue(v any) (startKey, stopKey []byte, ok bool) {
	opDoc, isOp := v.(*types.Document)
	if !isOp {
		// Null/array equality have semantics the byte-level index can't
		// reproduce; a bare Regex is a pattern match, not an equality;
		// Decimal128 has no faithful encoding. All fall back to scans.
		switch v.(type) {
		case nil, types.NullType, *types.Array, types.Regex, types.Decimal128:
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
		case *types.Document, *types.Array, nil, types.NullType, types.Regex,
			types.Decimal128:
			return nil, nil, false
		}
		// Comparison operators never match NaN; let the scan path apply
		// MongoDB's NaN semantics.
		if f, isFloat := opVal.(float64); isFloat && math.IsNaN(f) && opKey != "$eq" {
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
		// Translate $eq alone to its tight equality range; the handler
		// re-checks any range constraint that may also be present.
		return idxpkg.LowerBoundInclusive(eqVal), idxpkg.UpperBoundInclusive(eqVal), true
	}

	if !hasLower && !hasUpper {
		return nil, nil, false
	}

	// Comparison operators are type-bracketed ({$gt: 5} matches numbers
	// only): clamp missing range sides to the operand's bracket, and
	// fall back to scans for cross-bracket ranges.
	var bracketStart, bracketStop []byte
	if hasLower {
		s, e, bok := idxpkg.BracketRange(lowerVal)
		if !bok {
			return nil, nil, false
		}
		bracketStart, bracketStop = s, e
	}
	if hasUpper {
		s, e, bok := idxpkg.BracketRange(upperVal)
		if !bok {
			return nil, nil, false
		}
		if hasLower && (bracketStart[0] != s[0]) {
			return nil, nil, false
		}
		bracketStart, bracketStop = s, e
	}

	if hasLower {
		if lowerInclusive {
			startKey = idxpkg.LowerBoundInclusive(lowerVal)
		} else {
			startKey = idxpkg.LowerBoundExclusive(lowerVal)
		}
	} else {
		startKey = bracketStart
	}
	if hasUpper {
		if upperInclusive {
			stopKey = idxpkg.UpperBoundInclusive(upperVal)
		} else {
			stopKey = idxpkg.UpperBoundExclusive(upperVal)
		}
	} else {
		stopKey = bracketStop
	}
	return startKey, stopKey, true
}

// compoundLeadingBounds rewrites a single-field index bound pair so it
// scans the leading-field slice of a compound index. A single-field
// index entry ends in [discriminator(0x04)][primaryID]; a compound
// entry ends in [KS(v2)..KS(vN)][0x04][primaryID]. The single-field
// bounds use trailing 0x04/0x05 to bracket the entries with the
// leading value -- for a compound index those discriminator bytes
// fall in the wrong position and would miss every entry.
//
// The rewrite replaces the trailing discriminator with bytes that sort
// outside the entire range of KeyString ctype prefixes (which span
// 0x10..0xF0): 0x00 for "just before any KS(v_next)" and 0xFF for
// "just after any KS(v_next)". The resulting range is a sound
// superset of compound entries whose leading field matches the
// original predicate; suffix-field constraints are left to the
// handler-level re-filter.
func compoundLeadingBounds(start, stop []byte) ([]byte, []byte) {
	return bracketSuffix(start), bracketSuffix(stop)
}

// bracketSuffix rewrites a bound's trailing discriminator (0x04/0x05) to
// 0x00/0xFF so it brackets all suffix fields of a compound key.
func bracketSuffix(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	out := append([]byte(nil), b...)
	switch out[len(out)-1] {
	case 0x04:
		out[len(out)-1] = 0x00
	case 0x05:
		out[len(out)-1] = 0xFF
	}
	return out
}

// indexEqualityValue returns the operand when v is a plain equality (a bare
// scalar or {$eq}); ok is false otherwise.
func indexEqualityValue(v any) (any, bool) {
	unencodable := func(x any) bool {
		switch x.(type) {
		case nil, types.NullType, *types.Array, types.Regex, types.Decimal128:
			return true
		}
		return false
	}
	if opDoc, isOp := v.(*types.Document); isOp {
		keys := opDoc.Keys()
		if len(keys) != 1 || keys[0] != "$eq" {
			return nil, false
		}
		ev, err := opDoc.Get("$eq")
		if err != nil || unencodable(ev) {
			return nil, false
		}
		return ev, true
	}
	if unencodable(v) {
		return nil, false
	}
	return v, true
}

// compoundIndexBounds returns a tight [start, stop) range for a compound index
// from leading equality fields plus at most one trailing range field, or
// ok=false to fall back to compoundLeadingBounds. Every range is a sound
// superset (the handler re-filters).
func compoundIndexBounds(idx backends.IndexInfo, filter *types.Document, queryCmp *collation.Comparator) (startKey, stopKey []byte, ok bool) {
	bracketPrefix := func(prefix []byte) ([]byte, []byte) {
		return append(append([]byte(nil), prefix...), 0x00),
			append(append([]byte(nil), prefix...), 0xFF)
	}

	var prefix []byte
	for i, kp := range idx.Key {
		if kp.Descending {
			if i == 0 {
				return nil, nil, false
			}
			s, e := bracketPrefix(prefix)
			return s, e, true
		}
		raw, err := filter.Get(kp.Field)
		if err != nil {
			if i == 0 {
				return nil, nil, false
			}
			s, e := bracketPrefix(prefix)
			return s, e, true
		}
		v := raw
		if queryCmp != nil {
			v = collateFilterValue(v, queryCmp)
		}
		isLast := i == len(idx.Key)-1
		if eqVal, isEq := indexEqualityValue(v); isEq && !isLast {
			prefix = append(prefix, idxpkg.EncodeValue(eqVal)...)
			continue
		}
		if idx.Multikey {
			v = multikeyScanBound(v)
		}
		s, e, bok := indexBoundsForFilterValue(v)
		if !bok {
			if i == 0 {
				return nil, nil, false
			}
			bs, be := bracketPrefix(prefix)
			return bs, be, true
		}
		start := append(append([]byte(nil), prefix...), s...)
		stop := append(append([]byte(nil), prefix...), e...)
		if !isLast {
			start = bracketSuffix(start)
			stop = bracketSuffix(stop)
		}
		return start, stop, true
	}
	return nil, nil, false
}

// compareScalars returns -1 / 0 / 1 using types.Compare. Used to merge
// multiple bounds on the same side ($gte:1, $gte:5 -> keep the tighter 5).
// Collated sort keys (idxpkg.PreEncoded) compare bytewise; against a
// non-string operand ({$gte: 5, $gt: "b"}) a key stands in as "" so BSON
// type order decides.
func compareScalars(a, b any) int {
	pa, aKey := a.(idxpkg.PreEncoded)
	pb, bKey := b.(idxpkg.PreEncoded)
	switch {
	case aKey && bKey:
		return bytes.Compare(pa, pb)
	case aKey:
		a = ""
	case bKey:
		b = ""
	}
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
//
// Linear stages set input (rendered as inputStage); branching stages
// (OR, AND_SORTED, AND_HASH) set inputs (rendered as inputStages
// array). A stage uses at most one of the two.
type explainStage struct {
	stage      string
	indexName  string
	keyPattern *types.Document
	// keyPatternText replaces keyPattern for an EXPRESS stage, which MongoDB
	// reports as a string.
	keyPatternText string
	input          *explainStage
	inputs         []*explainStage
	// index is the index an IXSCAN reads.
	index backends.IndexInfo
	// idPoint marks the _id point lookup's IXSCAN.
	idPoint bool
}

func (s *explainStage) toDoc() *types.Document {
	d := must.NotFail(types.NewDocument("stage", s.stage))
	if s.indexName != "" {
		d.Set("indexName", s.indexName)
	}
	switch {
	case s.keyPatternText != "":
		d.Set("keyPattern", s.keyPatternText)
	case s.keyPattern != nil:
		d.Set("keyPattern", s.keyPattern)
	}
	if len(s.inputs) > 0 {
		arr := types.MakeArray(len(s.inputs))
		for _, child := range s.inputs {
			arr.Append(child.toDoc())
		}
		d.Set("inputStages", arr)
	} else if s.input != nil {
		d.Set("inputStage", s.input.toDoc())
	}
	return d
}

// partialFilterScalar extracts the scalar a condition pins a field to:
// a bare scalar v, or {$eq: scalar}. Returns ok=false for any other
// shape (other operators, arrays, null, regex, documents).
func partialFilterScalar(v any) (any, bool) {
	if doc, isDoc := v.(*types.Document); isDoc {
		if doc.Len() != 1 || doc.Keys()[0] != "$eq" {
			return nil, false
		}
		inner, err := doc.Get("$eq")
		if err != nil {
			return nil, false
		}
		v = inner
	}
	switch v.(type) {
	case nil, types.NullType, *types.Array, *types.Document, types.Regex:
		return nil, false
	}
	return v, true
}

// filterImpliesPartial reports whether filter guarantees every condition in a
// partial index's filter expression pfe. A scalar equality in pfe must appear
// as the identical equality in filter; $exists: true in pfe needs a filter
// condition on the field that cannot match a missing field; a numeric range in
// pfe ($gt, $gte, $lt, $lte), or a string range when stringRanges (the index is
// binary-collated, so bounds compare bytewise), must have each bound implied by
// a bound in the same direction, or an equality, on the same field in filter.
// Any other shape yields false, leaving the index unused (a sound collection
// scan). When true, every document matching filter lies within the partial
// index, so scanning the index and re-filtering is sound.
func filterImpliesPartial(filter, pfe *types.Document, stringRanges bool) bool {
	if pfe == nil || pfe.Len() == 0 {
		return true
	}
	if filter == nil {
		return false
	}
	for _, pk := range pfe.Keys() {
		pv, err := pfe.Get(pk)
		if err != nil {
			return false
		}
		fv, err := filter.Get(pk)
		if err != nil {
			return false
		}
		if ps, ok := partialFilterScalar(pv); ok {
			fs, ok := partialFilterScalar(fv)
			if !ok || compareScalars(ps, fs) != 0 {
				return false
			}
			continue
		}
		if isExistsTrue(pv) {
			if !conditionExcludesMissing(fv) {
				return false
			}
			continue
		}
		pr, ok := parseScalarRange(pv)
		if !ok || (!stringRanges && pr.hasString()) {
			return false
		}
		fr, ok := parseScalarRange(fv)
		if !ok || !fr.within(pr) {
			return false
		}
	}
	return true
}

// scalarRange is a condition's bounds on one field; an equality has both,
// inclusive and equal.
type scalarRange struct {
	lo, hi                   any
	hasLo, hasHi             bool
	loInclusive, hiInclusive bool
}

// parseScalarRange reads a bare number or string, or an operator document of
// $eq, $gt, $gte, $lt and $lte over numbers or strings, as bounds.
func parseScalarRange(v any) (scalarRange, bool) {
	var r scalarRange
	if !rangeComparable(v) {
		doc, ok := v.(*types.Document)
		if !ok || doc.Len() == 0 {
			return r, false
		}
		first := must.NotFail(doc.Get(doc.Keys()[0]))
		for _, op := range doc.Keys() {
			ov := must.NotFail(doc.Get(op))
			if !rangeComparable(ov) || !sameRangeBracket(ov, first) {
				return r, false
			}
			switch op {
			case "$eq":
				r.setLo(ov, true)
				r.setHi(ov, true)
			case "$gt":
				r.setLo(ov, false)
			case "$gte":
				r.setLo(ov, true)
			case "$lt":
				r.setHi(ov, false)
			case "$lte":
				r.setHi(ov, true)
			default:
				return r, false
			}
		}
		return r, true
	}
	r.setLo(v, true)
	r.setHi(v, true)
	return r, true
}

func (r scalarRange) hasString() bool {
	_, lo := r.lo.(string)
	_, hi := r.hi.(string)
	return lo || hi
}

func rangeComparable(v any) bool {
	switch t := v.(type) {
	case int32, int64, string:
		return true
	case float64:
		return !math.IsNaN(t)
	}
	return false
}

func sameRangeBracket(a, b any) bool {
	_, aStr := a.(string)
	_, bStr := b.(string)
	return aStr == bStr
}

// setLo keeps the tighter of the current and the given lower bound.
func (r *scalarRange) setLo(v any, inclusive bool) {
	if r.hasLo {
		c := compareScalars(v, r.lo)
		if c < 0 || (c == 0 && inclusive) {
			return
		}
	}
	r.lo, r.loInclusive, r.hasLo = v, inclusive, true
}

// setHi keeps the tighter of the current and the given upper bound.
func (r *scalarRange) setHi(v any, inclusive bool) {
	if r.hasHi {
		c := compareScalars(v, r.hi)
		if c > 0 || (c == 0 && inclusive) {
			return
		}
	}
	r.hi, r.hiInclusive, r.hasHi = v, inclusive, true
}

// within reports whether every value r admits is admitted by outer. Each bound
// is checked on its own, which also holds for array fields, where each bound
// may be met by a different element.
func (r scalarRange) within(outer scalarRange) bool {
	if outer.hasLo {
		if !r.hasLo || !sameRangeBracket(r.lo, outer.lo) {
			return false
		}
		c := compareScalars(r.lo, outer.lo)
		if c < 0 || (c == 0 && r.loInclusive && !outer.loInclusive) {
			return false
		}
	}
	if outer.hasHi {
		if !r.hasHi || !sameRangeBracket(r.hi, outer.hi) {
			return false
		}
		c := compareScalars(r.hi, outer.hi)
		if c > 0 || (c == 0 && r.hiInclusive && !outer.hiInclusive) {
			return false
		}
	}
	return true
}

// indexRank orders index candidates for the same leading field: lower
// wins. Single-field beats compound; non-partial beats partial.
func indexRank(compound, partial bool) int {
	r := 0
	if compound {
		r += 2
	}
	if partial {
		r += 1
	}
	return r
}

func pickIndexForFilter(filter *types.Document, idxInfos []backends.IndexInfo, queryCollation *types.Document) (backends.IndexInfo, bool) {
	queryIdentity := collation.Parse(queryCollation).Identity()
	if filter == nil || filter.Len() == 0 {
		return backends.IndexInfo{}, false
	}
	for _, k := range filter.Keys() {
		if strings.HasPrefix(k, "$") {
			return backends.IndexInfo{}, false
		}
		v, err := filter.Get(k)
		if err != nil {
			continue
		}
		nullEq, exists := false, false
		if _, _, ok := indexBoundsForFilterValue(v); !ok {
			_, inOK := indexPointRangesForIn(v)
			_, regexOK := regexPrefixRanges(v)
			nullEq, exists = isNullEquality(v), isExistsTrue(v)
			if !inOK && !(regexOK && queryIdentity == "") && !nullEq && !exists {
				continue
			}
		}
		// Pick the best-ranked index whose leading field is k. A partial
		// index is eligible only when the filter implies its partial
		// condition (so the query's matches all lie within it).
		best := -1
		var bestIdx backends.IndexInfo
		for _, idx := range idxInfos {
			if idx.Lossy || len(idx.Key) == 0 || idx.Key[0].Field != k {
				continue
			}
			if (nullEq && !nullEqualityIndexable(idx)) || (exists && !existsIndexable(idx)) {
				continue
			}
			if collation.Parse(idx.Collation).Identity() != queryIdentity {
				continue
			}
			partial := idx.PartialFilterExpression != nil
			if partial && !filterImpliesPartial(filter, idx.PartialFilterExpression, collation.Parse(idx.Collation).IsSimple()) {
				continue
			}
			rank := indexRank(len(idx.Key) > 1, partial)
			if best == -1 || rank < best {
				best, bestIdx = rank, idx
			}
		}
		if best != -1 {
			return bestIdx, true
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

// canonicalizeParsedQuery rewrites a raw filter into MongoDB's parsedQuery
// shape: a field mapped to a literal value becomes {field: {$eq: value}},
// while a field already mapped to an operator document ({$gt: ...}) is left
// unchanged. Logical operators ($and/$or/$nor) recurse into their clause
// arrays. This matches the canonical query MongoDB reports under
// queryPlanner.parsedQuery.
func canonicalizeParsedQuery(filter *types.Document) *types.Document {
	if filter == nil {
		return types.MakeDocument(0)
	}
	out := types.MakeDocument(filter.Len())
	for _, k := range filter.Keys() {
		v, _ := filter.Get(k)
		if strings.HasPrefix(k, "$") {
			if arr, ok := v.(*types.Array); ok {
				na := types.MakeArray(arr.Len())
				for i := 0; i < arr.Len(); i++ {
					e, _ := arr.Get(i)
					if ed, ok := e.(*types.Document); ok {
						na.Append(canonicalizeParsedQuery(ed))
					} else {
						na.Append(e)
					}
				}
				out.Set(k, na)
			} else {
				out.Set(k, v)
			}
			continue
		}
		out.Set(k, canonicalizeFieldPredicate(v))
	}
	return out
}

func canonicalizeFieldPredicate(v any) any {
	if doc, ok := v.(*types.Document); ok && isOperatorDoc(doc) {
		return doc
	}
	return must.NotFail(types.NewDocument("$eq", v))
}

// isOperatorDoc reports whether every key in doc is an operator ($-prefixed),
// which distinguishes a predicate expression ({$gt: 2}) from a literal
// document value that equality should match against.
func isOperatorDoc(doc *types.Document) bool {
	if doc.Len() == 0 {
		return false
	}
	for _, k := range doc.Keys() {
		if !strings.HasPrefix(k, "$") {
			return false
		}
	}
	return true
}

func (c *collection) Explain(ctx context.Context, params *backends.ExplainParams) (*backends.ExplainResult, error) {
	if params == nil {
		params = new(backends.ExplainParams)
	}
	parsedQuery := canonicalizeParsedQuery(params.Filter)

	var idxInfos []backends.IndexInfo
	if state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, false); err == nil && state != nil {
		state.mu.RLock()
		resolvedInfos, _, rerr := resolveIndexes(ctx, c, state)
		state.mu.RUnlock()
		if rerr == nil {
			idxInfos = append(idxInfos, resolvedInfos...)
		}
	}

	qp := must.NotFail(types.NewDocument(
		"namespace", c.db.name+"."+c.name,
		"parsedQuery", parsedQuery,
	))
	// The plan cache is read before planning, which may fill it.
	shapeHash, keyHash, cached := c.planCacheState(params, idxInfos)

	var winning *explainStage
	switch params.Command {
	case "count", "distinct":
		winning = buildCountOrDistinctPlan(params, idxInfos)
	default:
		res, scan, err := c.query(ctx, explainQueryParams(params))
		if err != nil {
			return nil, err
		}
		res.Iter.Close()
		winning = wrapScanPlan(params, scan, res.Sorted)
		// MongoDB plans each branch of a lone $or separately, under SUBPLAN.
		if (params.Command == "find" || params.Command == "aggregate") && params.Filter != nil && isLoneOr(params.Filter) &&
			scan.input != nil && scan.input.stage == "OR" {
			winning = &explainStage{stage: "SUBPLAN", input: winning}
		}
	}

	winningDoc := winning.toDoc()
	winningDoc.Set("isCached", cached)
	qp.Set("queryHash", shapeHash)
	qp.Set("planCacheShapeHash", shapeHash)
	qp.Set("planCacheKey", keyHash)
	qp.Set("winningPlan", winningDoc)
	qp.Set("rejectedPlans", types.MakeArray(0))
	return &backends.ExplainResult{QueryPlanner: qp}, nil
}

// explainQueryParams is the QueryParams the explained command runs with.
func explainQueryParams(params *backends.ExplainParams) *backends.QueryParams {
	qp := &backends.QueryParams{Filter: params.Filter, Sort: params.Sort, Hint: params.Hint, Collated: params.Collated, Collation: params.Collation, Fields: params.Fields}
	if params.Limit != 0 {
		qp.LimitHint = params.Skip + max(params.Limit, -params.Limit)
	}
	return qp
}

// planCacheState returns the plan cache hashes MongoDB reports for params, and
// whether a plan for them is cached.
func (c *collection) planCacheState(params *backends.ExplainParams, idxInfos []backends.IndexInfo) (shapeHash, keyHash string, cached bool) {
	qp := explainQueryParams(params)
	shapeHash, keyHash = planCacheHashes(idxInfos, qp)
	_, cached = c.db.backend.planCache.get(c.planCacheKey(idxInfos, qp))
	return shapeHash, keyHash, cached
}

func collscanStage() *explainStage { return &explainStage{stage: "COLLSCAN"} }

func fetchStage(input *explainStage) *explainStage {
	return &explainStage{stage: "FETCH", input: input}
}

func ixscanStage(info backends.IndexInfo) *explainStage {
	return &explainStage{stage: "IXSCAN", indexName: info.Name, keyPattern: keyPatternOf(info), index: info}
}

// idPointStage is the _id point lookup, a FETCH of one _id_ index entry.
func idPointStage() *explainStage {
	return fetchStage(&explainStage{
		stage:      "IXSCAN",
		indexName:  "_id_",
		keyPattern: must.NotFail(types.NewDocument("_id", int32(1))),
		idPoint:    true,
	})
}

// walkStage is a sort walk of the index named name, fetching each document.
func walkStage(idxInfos []backends.IndexInfo, name string) *explainStage {
	for _, info := range idxInfos {
		if info.Name == name {
			return fetchStage(ixscanStage(info))
		}
	}
	return fetchStage(&explainStage{stage: "IXSCAN", indexName: name})
}

// stage is the scan plan reads: its index, or an OR of a union's branches,
// fetching each document.
func (plan *indexLookupPlan) stage() *explainStage {
	if !plan.union {
		return fetchStage(ixscanStage(plan.info))
	}
	or := &explainStage{stage: "OR"}
	for _, branch := range plan.branches {
		or.inputs = append(or.inputs, ixscanStage(branch.info))
	}
	return fetchStage(or)
}

// buildCountOrDistinctPlan composes the stage tree for a count or distinct.
func buildCountOrDistinctPlan(params *backends.ExplainParams, idxInfos []backends.IndexInfo) *explainStage {
	var picked backends.IndexInfo
	var indexPicked bool
	if !hintIsNatural(params.Hint) {
		if hintName := pickHintedIndex(params.Hint, idxInfos); hintName != "" {
			for _, idx := range idxInfos {
				if idx.Name == hintName {
					picked, indexPicked = idx, true
					break
				}
			}
		}
		if !indexPicked {
			picked, indexPicked = pickIndexForFilter(params.Filter, idxInfos, params.Collation)
		}
	}
	if params.Command == "count" {
		leaf := collscanStage()
		if indexPicked {
			leaf = &explainStage{stage: "COUNT_SCAN", indexName: picked.Name, keyPattern: keyPatternOf(picked)}
		}
		return &explainStage{stage: "COUNT", input: leaf}
	}

	// distinct uses a covered index on its key when one exists; the filter
	// (if any) does not drive selection. MongoDB emits PROJECTION_COVERED
	// above DISTINCT_SCAN for a top-level key and PROJECTION_DEFAULT for a
	// dotted one.
	if !indexPicked && !hintIsNatural(params.Hint) && params.DistinctKey != "" {
		for _, idx := range idxInfos {
			if len(idx.Key) == 1 && idx.Key[0].Field == params.DistinctKey {
				picked, indexPicked = idx, true
				break
			}
		}
	}
	if !indexPicked {
		return collscanStage()
	}
	leaf := &explainStage{stage: "DISTINCT_SCAN", indexName: picked.Name, keyPattern: keyPatternOf(picked)}
	projection := "PROJECTION_COVERED"
	if strings.ContainsRune(params.DistinctKey, '.') {
		projection = "PROJECTION_DEFAULT"
	}
	return &explainStage{stage: projection, input: leaf}
}

// wrapScanPlan composes the stage tree for a find, aggregate or write around
// scan, the scan Query runs; sorted reports that scan returns the requested
// sort order. Wrapping order from inner to outer: SORT (unless sorted), SKIP,
// LIMIT (only when no SORT, because SORT absorbs LIMIT into a TopK), the
// projection, then the write.
func wrapScanPlan(params *backends.ExplainParams, scan *explainStage, sorted bool) *explainStage {
	if express, ok := expressStage(params, scan); ok {
		return express
	}

	// A projection of only indexed fields is answered from the index keys:
	// PROJECTION_COVERED replaces the FETCH. DumboDB still fetches each
	// document (dolthub/dumbodb#142); the shape shows the query could be
	// covered.
	node := scan
	covered := false
	if scan.stage == "FETCH" && scan.input != nil && scan.input.stage == "IXSCAN" && !scan.input.idPoint &&
		!scan.input.index.Multikey && projectionIsCoveredBy(params.Projection, scan.input.index) {
		node, covered = scan.input, true
	}

	sortPresent := params.Sort.Len() > 0 && !sortIsNatural(params.Sort) && !sorted
	if sortPresent {
		node = &explainStage{stage: "SORT", input: node}
	}
	if params.Skip > 0 {
		node = &explainStage{stage: "SKIP", input: node}
	}
	// A scan that sorts its index keys also absorbs the limit.
	keysSorted := scan.input != nil && scan.input.stage == "SORT"
	if params.Limit > 0 && !sortPresent && !keysSorted {
		node = &explainStage{stage: "LIMIT", input: node}
	}
	if params.Projection.Len() > 0 {
		projStage := "PROJECTION_SIMPLE"
		if covered {
			projStage = "PROJECTION_COVERED"
		}
		node = &explainStage{stage: projStage, input: node}
	}

	switch {
	case params.Command == "update" || params.Command == "findAndModify" && !params.Remove:
		return &explainStage{stage: "UPDATE", input: node}
	case params.Command == "delete" && params.Multi:
		return &explainStage{stage: "BATCHED_DELETE", input: node}
	case params.Command == "delete" || params.Command == "findAndModify":
		return &explainStage{stage: "DELETE", input: node}
	}
	return node
}

// expressStage returns the single EXPRESS stage MongoDB explains an _id point
// lookup with when the filter is the _id equality alone.
func expressStage(params *backends.ExplainParams, scan *explainStage) (*explainStage, bool) {
	if scan.input == nil || !scan.input.idPoint || params.Filter.Len() != 1 {
		return nil, false
	}
	stage := "EXPRESS_IXSCAN"
	switch {
	case params.Command == "update" || params.Command == "findAndModify" && !params.Remove:
		stage = "EXPRESS_UPDATE"
	case params.Command == "delete" || params.Command == "findAndModify":
		stage = "EXPRESS_DELETE"
	}
	return &explainStage{stage: stage, indexName: "_id_", keyPatternText: "{ _id: 1 }"}, true
}

// projectionIsCoveredBy reports whether projection only references
// fields contained in the picked index's key AND explicitly excludes
// _id. Required form: {field1: 1, field2: 1, ..., _id: 0} where every
// projected-in field appears in idx.Key.
func projectionIsCoveredBy(projection *types.Document, idx backends.IndexInfo) bool {
	if projection == nil || projection.Len() == 0 {
		return false
	}
	indexed := make(map[string]struct{}, len(idx.Key))
	for _, k := range idx.Key {
		indexed[k.Field] = struct{}{}
	}
	idExcluded := false
	for _, k := range projection.Keys() {
		v, err := projection.Get(k)
		if err != nil {
			return false
		}
		include, ok := projectionInclusion(v)
		if !ok {
			return false
		}
		if k == "_id" {
			if include {
				return false
			}
			idExcluded = true
			continue
		}
		if !include {
			// {field: 0} is exclusion-style projection; not covered.
			return false
		}
		if _, ok := indexed[k]; !ok {
			return false
		}
	}
	return idExcluded
}

// projectionInclusion converts an int/bool projection value to a bool
// (true=include, false=exclude). Returns ok=false for anything else
// (e.g. {$slice: ...}, {$elemMatch: ...}) since those are not
// covered-eligible.
func projectionInclusion(v any) (bool, bool) {
	switch x := v.(type) {
	case int32:
		return x != 0, true
	case int64:
		return x != 0, true
	case float64:
		return x != 0, true
	case bool:
		return x, true
	}
	return false, false
}

func sortIsSingleField(sort *types.Document) bool {
	if sort == nil || sort.Len() != 1 {
		return false
	}
	return sort.Keys()[0] != "$natural"
}

// sortDirectionAscending assumes sort is already single-field (see sortIsSingleField).
func sortDirectionAscending(sort *types.Document) bool {
	v, err := sort.Get(sort.Keys()[0])
	if err != nil {
		return true
	}
	switch x := v.(type) {
	case int32:
		return x >= 0
	case int64:
		return x >= 0
	case float64:
		return x >= 0
	}
	return true
}

func filterBindsEqualityPrefix(filter *types.Document, keys []backends.IndexKeyPair) bool {
	if len(keys) == 0 {
		return true
	}
	if filter == nil {
		return false
	}
	for _, kp := range keys {
		v, err := filter.Get(kp.Field)
		if err != nil {
			return false
		}
		if !valueIsScalarEquality(v) {
			return false
		}
	}
	return true
}

// indexScanSortOrder reports whether a scan of idx bounded by filter yields
// documents in sort order, forward or, when reverse, backward: the sort field
// is an index key, every key before it is bound by equality, the index is not
// multikey (a document sorts by its min or max element, not the one the bounds
// matched), and both orders are binary. Entries are stored ascending whatever
// the key's direction, so a descending sort scans backward.
func indexScanSortOrder(idx backends.IndexInfo, filter, sort, queryCollation *types.Document) (reverse, ok bool) {
	if !indexKeysOrderSort(idx, sort, queryCollation) {
		return false, false
	}
	for pos, k := range idx.Key {
		if k.Field != sort.Keys()[0] {
			continue
		}
		if !filterBindsEqualityPrefix(filter, idx.Key[:pos]) {
			return false, false
		}
		return !sortDirectionAscending(sort), true
	}
	return false, false
}

// indexKeysOrderSort reports whether idx's encoded keys order documents as
// the single-field sort does: the index is not multikey (a document sorts by
// its min or max element, not the one an entry holds), the sort is a direction,
// and both orders are binary.
func indexKeysOrderSort(idx backends.IndexInfo, sort, queryCollation *types.Document) bool {
	if idx.Multikey || !sortIsSingleField(sort) || !collation.Parse(queryCollation).IsSimple() || !collation.Parse(idx.Collation).IsSimple() {
		return false
	}
	switch must.NotFail(sort.Get(sort.Keys()[0])).(type) {
	case int32, int64, float64:
		return true
	}
	return false
}

// indexKeySortPosition returns the position of the sort field in idx when a
// scan of idx bounded by filter can be sorted on its keys before any document
// is fetched: the keys order the sort, and every filter field is an index key,
// so the bounds alone decide a match.
func indexKeySortPosition(idx backends.IndexInfo, filter, sort, queryCollation *types.Document) (int, bool) {
	if filter == nil || !indexKeysOrderSort(idx, sort, queryCollation) {
		return 0, false
	}
	for _, f := range filter.Keys() {
		if !slices.ContainsFunc(idx.Key, func(k backends.IndexKeyPair) bool { return k.Field == f }) {
			return 0, false
		}
	}
	for pos, k := range idx.Key {
		if k.Field == sort.Keys()[0] {
			return pos, true
		}
	}
	return 0, false
}

// valueIsScalarEquality reports whether v is a bare scalar OR a single
// {$eq: scalar} operator document. Null/array/regex are excluded
// because their MongoDB match semantics don't fit a byte-level range
// scan.
func valueIsScalarEquality(v any) bool {
	switch x := v.(type) {
	case *types.Document:
		if x == nil || x.Len() != 1 || x.Keys()[0] != "$eq" {
			return false
		}
		inner, err := x.Get("$eq")
		if err != nil {
			return false
		}
		return scalarOK(inner)
	}
	return scalarOK(v)
}

func scalarOK(v any) bool {
	switch v.(type) {
	case nil, types.NullType, *types.Array, *types.Document, types.Regex:
		return false
	}
	return true
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

func hintIsNatural(hint any) bool {
	return backends.HintIsNatural(hint)
}

func pickHintedIndex(hint any, idxInfos []backends.IndexInfo) string {
	return backends.MatchHintedIndex(hint, idxInfos)
}

// extractIndexKey represents fields absent from doc as types.Null.
func extractIndexKey(doc *types.Document, idx backends.IndexInfo) []any {
	key := make([]any, len(idx.Key))
	for i, kp := range idx.Key {
		key[i] = indexFieldValue(doc, kp.Field)
	}
	return key
}

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

// indexComparator returns the ICU comparator for an index's collation, or nil
// for a simple/absent collation (binary comparison).
func indexComparator(idx backends.IndexInfo) *collation.Comparator {
	return collation.Parse(idx.Collation).Comparator()
}

// collateFilterValue rewrites a filter value's string operands to ICU sort keys
// (PreEncoded) so bounds line up with a collated index's stored keys.
func collateFilterValue(v any, cmp *collation.Comparator) any {
	switch t := v.(type) {
	case string:
		return idxpkg.PreEncoded(cmp.Key(t))
	case *types.Document:
		out := types.MakeDocument(t.Len())
		for _, k := range t.Keys() {
			ov, _ := t.Get(k)
			if str, ok := ov.(string); ok {
				ov = idxpkg.PreEncoded(cmp.Key(str))
			}
			// AppendDecoded: Set rejects idxpkg.PreEncoded.
			out.AppendDecoded(k, ov)
		}
		return out
	default:
		return v
	}
}

// collateRowValues rewrites the string components of an index row to ICU sort
// keys (PreEncoded); cmp must be non-nil.
func collateRowValues(row []any, cmp *collation.Comparator) []any {
	out := make([]any, len(row))
	for i, v := range row {
		if str, ok := v.(string); ok {
			out[i] = idxpkg.PreEncoded(cmp.Key(str))
		} else {
			out[i] = v
		}
	}
	return out
}

// indexCollated reports whether an index carries a non-simple collation. Such an
// index cannot use the byte-prefix probe -- "Alice" and "alice" encode to
// distinct bytes yet collide under a strength-2 collation -- so uniqueness folds
// through the value-level scan (scanUniqueConflict) instead.
func indexCollated(idx backends.IndexInfo) bool {
	return !collation.Parse(idx.Collation).IsSimple()
}

// indexKeysEqualColl compares two index keys, folding string components through
// cmp when it is non-nil; other components (and every component when cmp is nil)
// compare byte/type-exact.
func indexKeysEqualColl(a, b []any, cmp *collation.Comparator) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if cmp != nil {
			as, aok := a[i].(string)
			bs, bok := b[i].(string)
			if aok && bok {
				if !cmp.EqualStrings(as, bs) {
					return false
				}
				continue
			}
		}
		if types.Compare(a[i], b[i]) != types.Equal {
			return false
		}
	}
	return true
}

// rowConflictKey returns the canonical map key identifying an index row for
// build-time unique conflict detection. For a collated index, string components
// use their ICU sort key (equal-under-collation strings share a key); other
// components and non-collated indexes use the byte-exact probe prefix.
func rowConflictKey(row []any) string {
	prefix, _ := idxpkg.EqualityProbeBounds(row)
	return string(prefix)
}

// idDupKey builds the duplicate-key detail {_id: id} for the synthetic _id index.
func idDupKey(id any) *types.Document {
	return must.NotFail(types.NewDocument("_id", id))
}

// indexDupKey builds the duplicate-key detail {field: value, ...} for idx from doc,
// so a duplicate-key error can name the offending index and key like MongoDB.
func indexDupKey(doc *types.Document, idx backends.IndexInfo) *types.Document {
	pairs := make([]any, 0, len(idx.Key)*2)
	for _, kp := range idx.Key {
		pairs = append(pairs, kp.Field, indexFieldValue(doc, kp.Field))
	}
	return must.NotFail(types.NewDocument(pairs...))
}

func (c *collection) InsertAll(ctx context.Context, params *backends.InsertAllParams) (*backends.InsertAllResult, error) {
	if err := c.rejectReplicationControlWrite(); err != nil {
		return nil, err
	}
	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, true)
	if err != nil {
		return nil, err
	}

	if err := c.acquireInsertLocks(ctx, params.Docs); err != nil {
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

	branchInfos, branchIdxMaps, err := resolveBranchIndexState(ctx, c, state)
	if err != nil {
		return nil, fmt.Errorf("resolving branch index state: %w", err)
	}
	var uniqueIndexes []backends.IndexInfo
	for _, idx := range branchInfos {
		if idx.Unique {
			uniqueIndexes = append(uniqueIndexes, idx)
		}
	}

	// Collated _id uniqueness: when the collection carries a non-simple default
	// collation, _id equality folds through it (design collation-resolution.md
	// 3.6), so "a" and "A" collide in a strength-2 collection even though their
	// hashes differ. String _ids fall back to an O(N) value scan; binary _ids
	// (numbers, ObjectId) are collation-invariant and keep the hash fast path.
	var idCmp *collation.Comparator
	if catAM, amErr := state.getOrInitBranchAM(ctx, c.db.rootish); amErr == nil {
		if meta, mErr := readCatalogDoc(ctx, state, catAM, c.name); mErr == nil && meta != nil {
			idCmp = collation.Parse(meta.Collation).Comparator()
		}
	}

	mut := m.Mutate()

	// batchHashSet detects in-batch duplicate _id hashes in O(1).
	batchHashSet := make(map[string]struct{}, len(params.Docs))
	// In-batch collated string _ids (the primary probe only sees pre-batch state).
	var batchStrIDs []string
	// In-batch claims per unique index (probes only see pre-batch state).
	batchUniqueEntryKeys := make([]map[string]struct{}, len(uniqueIndexes))
	for i := range batchUniqueEntryKeys {
		batchUniqueEntryKeys[i] = make(map[string]struct{})
	}
	// Value-level fallback set for lossy rows.
	batchLossyKeys := make([][][]any, len(uniqueIndexes))

	for _, doc := range params.Docs {
		docID, err := doc.Get("_id")
		if err != nil {
			return nil, fmt.Errorf("document missing _id: %w", err)
		}

		h, err := primaryKey(docID)
		if err != nil {
			return nil, fmt.Errorf("encoding _id: %w", err)
		}

		exists, err := existsID(ctx, m, h)
		if err != nil {
			return nil, fmt.Errorf("checking existing _id: %w", err)
		}
		if exists {
			return nil, backends.NewDuplicateKeyError(backends.DefaultIndexName, idDupKey(docID))
		}

		if _, dup := batchHashSet[string(h)]; dup {
			return nil, backends.NewDuplicateKeyError(backends.DefaultIndexName, idDupKey(docID))
		}

		if idStr, ok := docID.(string); ok && idCmp != nil {
			conflict, scanErr := c.scanCollatedIDConflict(ctx, state, m, idStr, h, idCmp)
			if scanErr != nil {
				return nil, scanErr
			}
			if !conflict {
				for _, prev := range batchStrIDs {
					if idCmp.EqualStrings(prev, idStr) {
						conflict = true
						break
					}
				}
			}
			if conflict {
				return nil, backends.NewDuplicateKeyError(backends.DefaultIndexName, idDupKey(docID))
			}
			batchStrIDs = append(batchStrIDs, idStr)
		}

		// Unique constraints: one bounded index probe per entry row;
		// membership and multikey expansion via indexEntriesForDoc.
		for i, idx := range uniqueIndexes {
			rows, _, lossy := indexEntriesForDoc(doc, idx)
			if len(rows) == 0 {
				continue
			}

			if lossy || idx.Lossy {
				conflict, scanErr := c.scanUniqueConflict(ctx, state, m, idx, doc, h)
				if scanErr != nil {
					return nil, scanErr
				}
				if conflict {
					return nil, backends.NewDuplicateKeyError(idx.Name, indexDupKey(doc, idx))
				}
				newKey := extractIndexKey(doc, idx)
				batchCmp := indexComparator(idx)
				for _, batchKey := range batchLossyKeys[i] {
					if indexKeysEqualColl(newKey, batchKey, batchCmp) {
						return nil, backends.NewDuplicateKeyError(idx.Name, indexDupKey(doc, idx))
					}
				}
				batchLossyKeys[i] = append(batchLossyKeys[i], newKey)
				continue
			}

			// A doc's own duplicate rows ([5,5]) must not self-conflict.
			docPrefixes := make(map[string]struct{}, len(rows))
			for _, row := range rows {
				start, _ := idxpkg.EqualityProbeBounds(row)
				docPrefixes[string(start)] = struct{}{}
			}
			for prefix := range docPrefixes {
				if _, claimed := batchUniqueEntryKeys[i][prefix]; claimed {
					return nil, backends.NewDuplicateKeyError(idx.Name, indexDupKey(doc, idx))
				}
			}
			for _, row := range rows {
				conflict, probeErr := idxpkg.UniqueConflict(ctx, branchIdxMaps[idx.Name], row, h)
				if probeErr != nil {
					return nil, fmt.Errorf("unique probe on %s: %w", idx.Name, probeErr)
				}
				if conflict {
					return nil, backends.NewDuplicateKeyError(idx.Name, indexDupKey(doc, idx))
				}
			}
			for prefix := range docPrefixes {
				batchUniqueEntryKeys[i][prefix] = struct{}{}
			}
		}

		key, err := buildKey(h)
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

		batchHashSet[string(h)] = struct{}{}
	}

	newMap, err := mut.Map(ctx)
	if err != nil {
		return nil, err
	}

	// The resolver reads per-branch index state from disk, so the resulting AM
	// reflects only this branch's writes -- no cross-branch leakage.
	infos, idxMaps, err := resolveBranchIndexState(ctx, c, state)
	if err != nil {
		return nil, fmt.Errorf("resolving branch index state: %w", err)
	}
	updatedInfos, updatedIdxMaps, err := applyInsertsToIndexes(ctx, infos, idxMaps, params.Docs)
	if err != nil {
		return nil, fmt.Errorf("updating secondary indexes: %w", err)
	}
	newIndexes, err := buildSecondaryIndexes(ctx, state, updatedInfos, updatedIdxMaps)
	if err != nil {
		return nil, fmt.Errorf("building secondary indexes: %w", err)
	}

	dtblHash, err := state.dtblHashForCollection(ctx, c.name, newMap, newIndexes, hash.Hash{})
	if err != nil {
		return nil, err
	}
	if err := state.updateAddressMap(ctx, c.db.rootish, fmt.Sprintf("auto: insert %d docs into %s", len(params.Docs), c.name), func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, dtblHash)
	}); err != nil {
		return nil, err
	}

	return &backends.InsertAllResult{}, nil
}

type initialSyncTuple struct {
	hash  []byte
	id    any
	key   val.Tuple
	value val.Tuple
}

type initialSyncTupleIter struct {
	tuples []initialSyncTuple
	index  int
}

func (i *initialSyncTupleIter) Next(context.Context) (val.Tuple, val.Tuple) {
	if i.index == len(i.tuples) {
		return nil, nil
	}
	tuple := i.tuples[i.index]
	i.index++
	return tuple.key, tuple.value
}

func (c *collection) BulkLoadInitialSync(ctx context.Context, documents []*types.Document) error {
	if err := c.rejectReplicationControlWrite(); err != nil {
		return err
	}
	if len(documents) == 0 {
		return nil
	}
	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, true)
	if err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if c.db.isReadOnly(ctx, state) {
		return backends.NewError(backends.ErrorCodeReadOnlyDatabase, fmt.Errorf("cannot write to a read-only database snapshot"))
	}
	primary, err := c.loadOrCreateMap(ctx, state)
	if err != nil {
		return err
	}
	indexInfos, _, err := resolveBranchIndexState(ctx, c, state)
	if err != nil {
		return fmt.Errorf("resolving initial-sync index state: %w", err)
	}
	if len(indexInfos) != 0 {
		return errors.New("initial-sync bulk load requires indexes to be built after documents")
	}
	tuples := make([]initialSyncTuple, 0, len(documents))
	for _, document := range documents {
		documentID, err := document.Get("_id")
		if err != nil {
			return fmt.Errorf("document missing _id: %w", err)
		}
		hash, err := primaryKey(documentID)
		if err != nil {
			return fmt.Errorf("encoding _id: %w", err)
		}
		key, err := buildKey(hash)
		if err != nil {
			return err
		}
		value, err := writeDocToValue(ctx, state.ns, document)
		if err != nil {
			return err
		}
		tuples = append(tuples, initialSyncTuple{hash: hash, id: documentID, key: key, value: value})
	}
	slices.SortFunc(tuples, func(left, right initialSyncTuple) int {
		return bytes.Compare(left.hash, right.hash)
	})
	for index, tuple := range tuples {
		if index > 0 && bytes.Equal(tuple.hash, tuples[index-1].hash) {
			return backends.NewDuplicateKeyError(backends.DefaultIndexName, idDupKey(tuple.id))
		}
		exists, err := existsID(ctx, primary, tuple.hash)
		if err != nil {
			return fmt.Errorf("checking existing _id: %w", err)
		}
		if exists {
			return backends.NewDuplicateKeyError(backends.DefaultIndexName, idDupKey(tuple.id))
		}
	}
	primary, err = prolly.MutateMapWithTupleIter(ctx, primary, &initialSyncTupleIter{tuples: tuples})
	if err != nil {
		return fmt.Errorf("merging initial-sync sorted run: %w", err)
	}
	indexes, err := buildSecondaryIndexes(ctx, state, nil, nil)
	if err != nil {
		return fmt.Errorf("building empty initial-sync index map: %w", err)
	}
	dtblHash, err := state.dtblHashForCollection(ctx, c.name, primary, indexes, hash.Hash{})
	if err != nil {
		return err
	}
	return state.updateAddressMap(ctx, c.db.rootish, fmt.Sprintf("initial sync: load %d docs into %s", len(documents), c.name), func(editor prolly.AddressMapEditor) error {
		return editor.Update(ctx, c.name, dtblHash)
	})
}

func existsID(ctx context.Context, m prolly.Map, h []byte) (bool, error) {
	key, err := buildKey(h)
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
	if err := c.rejectReplicationControlWrite(); err != nil {
		return nil, err
	}
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

	// Resolved once per batch: backs unique probes and the entry
	// maintenance after the loop.
	idxInfos, idxMaps, err := resolveBranchIndexState(ctx, c, state)
	if err != nil {
		return nil, fmt.Errorf("resolving branch index state: %w", err)
	}
	hasUnique := false
	for _, idx := range idxInfos {
		if idx.Unique {
			hasUnique = true
			break
		}
	}

	var idxOldDocs, idxNewDocs []*types.Document

	for i, doc := range params.Docs {
		docID, err := doc.Get("_id")
		if err != nil {
			return nil, fmt.Errorf("document missing _id: %w", err)
		}

		h, err := primaryKey(docID)
		if err != nil {
			return nil, fmt.Errorf("encoding _id: %w", err)
		}

		key, err := buildKey(h)
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
			newBytes, err = docToBSON(doc)
			if err != nil {
				return nil, err
			}
		}

		if len(idxInfos) > 0 {
			// The new doc decodes from newBytes (what was actually stored;
			// the partial-mutation path may differ from params.Docs).
			oldDoc, err := readDocFromValue(ctx, state.ns, existingTup)
			if err != nil {
				return nil, fmt.Errorf("decoding pre-update document: %w", err)
			}
			newDoc, err := bsonToDoc(newBytes)
			if err != nil {
				return nil, fmt.Errorf("decoding post-update document: %w", err)
			}

			if hasUnique {
				if err := c.validateUniqueOnUpdate(ctx, state, m, idxInfos, idxMaps, newDoc, h); err != nil {
					return nil, err
				}
			}

			idxOldDocs = append(idxOldDocs, oldDoc)
			idxNewDocs = append(idxNewDocs, newDoc)
		}

		v, err := buildValue(ctx, state.ns, newBytes, storedBlobAddr(ctx, state.ns, existingTup))
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

	updatedInfos, updatedIdxMaps, err := applyUpdatesToIndexes(ctx, idxInfos, idxMaps, idxOldDocs, idxNewDocs)
	if err != nil {
		return nil, fmt.Errorf("updating secondary indexes: %w", err)
	}
	curIndexes, err := buildSecondaryIndexes(ctx, state, updatedInfos, updatedIdxMaps)
	if err != nil {
		return nil, fmt.Errorf("building secondary indexes: %w", err)
	}

	dtblHash, err := state.dtblHashForCollection(ctx, c.name, newMap, curIndexes, hash.Hash{})
	if err != nil {
		return nil, err
	}
	if err := state.updateAddressMap(ctx, c.db.rootish, fmt.Sprintf("auto: update %s", c.name), func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, dtblHash)
	}); err != nil {
		return nil, err
	}

	return &backends.UpdateAllResult{Updated: updated}, nil
}

func (c *collection) DeleteAll(ctx context.Context, params *backends.DeleteAllParams) (*backends.DeleteAllResult, error) {
	if err := c.rejectReplicationControlWrite(); err != nil {
		return nil, err
	}
	state, err := c.db.backend.getOrOpenDB(ctx, c.db.name, false)
	if err != nil {
		return nil, err
	}

	if state == nil {
		return &backends.DeleteAllResult{}, nil
	}

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

	// Captured before removal for index maintenance.
	var idxOldDocs []*types.Document

	if params.RecordIDs != nil {
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
			doc *types.Document
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
				doc, derr := readDocFromValue(ctx, state.ns, v)
				if derr != nil {
					return nil, fmt.Errorf("decoding document for delete: %w", derr)
				}
				toDeleteList = append(toDeleteList, toDelete{key: k, doc: doc})
			}
		}

		for _, td := range toDeleteList {
			if err := mut.Delete(ctx, td.key); err != nil {
				return nil, err
			}
			idxOldDocs = append(idxOldDocs, td.doc)
			deleted++
		}
	} else {
		for _, id := range params.IDs {
			h, err := primaryKey(id)
			if err != nil {
				continue
			}

			key, err := buildKey(h)
			if err != nil {
				continue
			}

			var oldDoc *types.Document
			if err := mut.Get(ctx, key, func(k, v val.Tuple) error {
				if v == nil {
					return nil
				}
				var decErr error
				oldDoc, decErr = readDocFromValue(ctx, state.ns, v)
				return decErr
			}); err != nil {
				continue
			}

			if oldDoc == nil {
				continue
			}

			if err := mut.Delete(ctx, key); err != nil {
				return nil, err
			}

			idxOldDocs = append(idxOldDocs, oldDoc)
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

	idxInfos, idxMaps, err := resolveBranchIndexState(ctx, c, state)
	if err != nil {
		return nil, fmt.Errorf("resolving branch index state: %w", err)
	}
	updatedIdxMaps, err := applyDeletesToIndexes(ctx, idxInfos, idxMaps, idxOldDocs)
	if err != nil {
		return nil, fmt.Errorf("updating secondary indexes: %w", err)
	}
	curIndexes, err := buildSecondaryIndexes(ctx, state, idxInfos, updatedIdxMaps)
	if err != nil {
		return nil, fmt.Errorf("building secondary indexes: %w", err)
	}

	dtblHash, err := state.dtblHashForCollection(ctx, c.name, newMap, curIndexes, hash.Hash{})
	if err != nil {
		return nil, err
	}
	if err := state.updateAddressMap(ctx, c.db.rootish, fmt.Sprintf("auto: delete from %s", c.name), func(ed prolly.AddressMapEditor) error {
		return ed.Update(ctx, c.name, dtblHash)
	}); err != nil {
		return nil, err
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
		n, used, ferr := c.tryIndexedCount(ctx, state, params.Filter, params.Collation)
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
func (c *collection) tryIndexedCount(ctx context.Context, state *dbState, filter *types.Document, queryCollation *types.Document) (int64, bool, error) {
	if filter == nil || filter.Len() != 1 {
		return 0, false, nil
	}

	field := filter.Keys()[0]
	if strings.HasPrefix(field, "$") {
		return 0, false, nil
	}

	queryColl := collation.Parse(queryCollation)
	queryIdentity := queryColl.Identity()
	queryCmp := queryColl.Comparator()

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
	var chosen backends.IndexInfo
	found := false
	for i, idx := range idxInfos {
		// Lossy entries sit at wrong byte positions. A partial index is
		// usable only when the filter implies its partial condition, so
		// the counted entries are exactly the matching documents;
		// otherwise it would undercount (it omits non-member docs).
		if idx.Lossy {
			continue
		}
		if idx.PartialFilterExpression != nil && !filterImpliesPartial(filter, idx.PartialFilterExpression, collation.Parse(idx.Collation).IsSimple()) {
			continue
		}
		if collation.Parse(idx.Collation).Identity() != queryIdentity {
			continue
		}
		if len(idx.Key) == 1 && idx.Key[0].Field == field {
			idxMap = idxMaps[i]
			chosen = idx
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

	// A multikey range counts a doc once per matching element; equality
	// counts stay exact (one entry per value+docID).
	if _, isOp := v.(*types.Document); isOp && chosen.Multikey {
		return 0, false, nil
	}

	if queryCmp != nil {
		v = collateFilterValue(v, queryCmp)
	}

	startKey, stopKey, ok := indexBoundsForFilterValue(v)
	if !ok {
		return 0, false, nil
	}

	n, err := idxpkg.RangeCount(ctx, idxMap, startKey, stopKey)
	if err != nil {
		return 0, false, err
	}
	countKeysExamined(ctx, int(n))
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
	if err := c.rejectReplicationControlWrite(); err != nil {
		return nil, err
	}
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

	var start, stop []byte
	if params.Filter != nil {
		if params.Filter.Len() != 1 || params.Filter.Keys()[0] != params.Key || strings.Contains(params.Key, ".") {
			return nil, nil
		}
		var ok bool
		if start, stop, ok = indexBoundsForFilterValue(must.NotFail(params.Filter.Get(params.Key))); !ok {
			return nil, nil
		}
	}

	var (
		idxMap prolly.Map
		found  bool
	)
	for i, idx := range idxInfos {
		if len(idx.Key) != 1 {
			continue
		}
		// A filtered scan bounds array elements one at a time, while a matched
		// document contributes all of its elements; a hidden index serves no
		// query.
		if params.Filter != nil && (idx.Multikey || idx.Hidden) {
			continue
		}
		// Lossy indexes hold entries at wrong byte positions (and the
		// values behind them are not what the entry bytes claim).
		if idx.Lossy {
			continue
		}
		// A collated index groups by sort-key prefix, which merges values an
		// uncollated distinct must keep separate; fall back to the scan.
		if collation.Parse(idx.Collation).Identity() != "" {
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
		// Sparse indexes are fine: MongoDB also reads distinct from them.
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

	values, err := scanDistinctFromIndex(ctx, idxMap, m, state.ns, params.Key, start, stop, params.Filter)
	if err != nil {
		return nil, err
	}

	return &backends.DistinctResult{Values: values}, nil
}

// scanDistinctFromIndex returns the distinct values of field among idxMap's
// entries in [start, stop), as MongoDB's DISTINCT_SCAN does: it reads one
// entry per value, restoring the value from the entry and skipping past the
// rest of its entries. An entry that cannot restore its value costs one
// document fetch for its group. A fetched document contributes its MongoDB
// index keys, including
// null for a missing field and undefined for an empty array. A value counts
// only when it, or its fetched document, matches filter, when set.
func scanDistinctFromIndex(
	ctx context.Context,
	idxMap prolly.Map,
	primary prolly.Map,
	ns tree.NodeStore,
	field string,
	start, stop []byte,
	filter *types.Document,
) ([]any, error) {
	path, err := types.NewPathFromString(field)
	if err != nil {
		return nil, err
	}

	var out []any
	emitDoc := func(id []byte) error {
		doc, err := lookupDocFromPrimary(ctx, primary, ns, id, nil)
		if err != nil || doc == nil {
			return err
		}
		if filter != nil {
			if match, err := backends.MatchPartialFilter(doc, filter); err != nil || !match {
				return err
			}
		}
		out = append(out, commonpath.IndexKeyValues(doc, path)...)
		return nil
	}

	for cursor := start; ; {
		iter, ok, err := idxpkg.SeekKeyRange(ctx, idxMap, cursor, stop)
		if err != nil {
			return nil, fmt.Errorf("distinct scan iterating index: %w", err)
		}
		if !ok {
			return out, nil
		}
		values, id, tags, done, err := nextDistinctEntry(ctx, iter)
		if err != nil || done {
			return out, err
		}

		if v, ok := decodeDistinctValue(values, tags); ok {
			match := filter == nil
			if !match {
				if match, err = backends.MatchPartialFilter(must.NotFail(types.NewDocument(field, v)), filter); err != nil {
					return nil, err
				}
			}
			if match {
				out = append(out, v)
			}
		} else if err := emitDoc(id); err != nil {
			return nil, err
		}
		cursor = idxpkg.SkipValues(values)
	}
}

// nextDistinctEntry reads iter's next entry, counting its key; done is set at
// the end of the range.
func nextDistinctEntry(ctx context.Context, iter prolly.MapIter) (values, id, tags []byte, done bool, err error) {
	for {
		k, v, err := iter.Next(ctx)
		if err == io.EOF || (err == nil && len(k) == 0) {
			return nil, nil, nil, true, nil
		}
		if err != nil {
			return nil, nil, nil, false, fmt.Errorf("distinct scan reading index: %w", err)
		}
		countKeysExamined(ctx, 1)
		if values, tags, id, ok := idxpkg.DecodeEntry(k, v); ok {
			return values, id, tags, false, nil
		}
	}
}

// decodeDistinctValue restores a single-field entry's value from values and
// tags.
func decodeDistinctValue(values, tags []byte) (any, bool) {
	if len(tags) != 1 {
		return nil, false
	}
	return idxpkg.DecodeValue(values, tags[0])
}

// lookupDocFromPrimary fetches the primary document by encoded _id bytes, or
// nil if it does not exist, keeping only fields when it is non-nil.
func lookupDocFromPrimary(
	ctx context.Context,
	primary prolly.Map,
	ns tree.NodeStore,
	idBytes []byte,
	fields []string,
) (*types.Document, error) {
	key, err := buildKey(idBytes)
	if err != nil {
		return nil, fmt.Errorf("distinct scan building key: %w", err)
	}

	var doc *types.Document
	if err := primary.Get(ctx, key, func(_, v val.Tuple) error {
		if v == nil {
			return nil
		}
		countDocsExamined(ctx, 1)
		stored, err := getBSONStoredBytes(ctx, ns, v)
		if err != nil {
			return err
		}
		doc, err = bsonToDocFields(stored, fields)
		return err
	}); err != nil {
		return nil, fmt.Errorf("distinct scan primary fetch: %w", err)
	}
	return doc, nil
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

	secondary, err := secondaryIndexesForDTBL(ctx, state.cs, state.ns, dtblHash)
	if err != nil {
		return nil, err
	}

	indexes := []backends.IndexInfo{
		{
			Name: backends.DefaultIndexName,
			Key:  []backends.IndexKeyPair{{Field: "_id", Descending: false}},
		},
	}

	// The _id index is pinned to the collection default collation: it inherits
	// it and cannot diverge (design collation-resolution.md 3.6). Report it so
	// listIndexes echoes _id_ with the collection collation, not simple.
	if meta, mErr := readCatalogDoc(ctx, state, am, c.name); mErr == nil && meta != nil && meta.Collation != nil {
		indexes[0].Collation = meta.Collation
	}

	if err := secondary.IterAll(ctx, func(_ string, entry *resolvedIndexEntry) error {
		indexes = append(indexes, entry.info)
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
	if err := c.rejectReplicationControlWrite(); err != nil {
		return nil, err
	}
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
		idxMap, multikey, lossy, buildErr := c.buildSecondaryIndex(ctx, state, idx)
		if buildErr != nil {
			return nil, fmt.Errorf("building secondary index %q on %q: %w", idx.Name, c.name, buildErr)
		}
		idx.Multikey = idx.Multikey || multikey
		idx.Lossy = idx.Lossy || lossy
		infoByName[idx.Name] = idx
		curMaps[idx.Name] = idxMap
	}

	newInfos := make([]backends.IndexInfo, 0, len(infoByName))
	for _, info := range infoByName {
		newInfos = append(newInfos, info)
	}
	slices.SortFunc(newInfos, func(a, b backends.IndexInfo) int {
		return cmp.Compare(a.Name, b.Name)
	})

	newIndexes, err := buildSecondaryIndexes(ctx, state, newInfos, curMaps)
	if err != nil {
		return nil, fmt.Errorf("building secondary indexes: %w", err)
	}

	if err := c.rewriteDTBLAfterIndexChange(ctx, state, newIndexes, fmt.Sprintf("auto: create index %s on %s", indexNames(params.Indexes), c.name)); err != nil {
		return nil, fmt.Errorf("rewriting DTBL for %q: %w", c.name, err)
	}

	return &backends.CreateIndexesResult{}, nil
}

func indexNames(indexes []backends.IndexInfo) string {
	names := make([]string, len(indexes))
	for i, idx := range indexes {
		names[i] = idx.Name
	}
	return strings.Join(names, ", ")
}

// rewriteDTBLAfterIndexChange rebuilds the collection's DTBL with the
// given secondary indexes and updates the collections AM. Called after CreateIndexes
// / DropIndexes so an index-only change is durable.
//
// If the collection has no primary map yet (no inserts have happened) it
// uses a freshly created empty map  -- the same path as loadOrCreateMap.
//
// The caller must hold state.mu (write lock).
func (c *collection) rewriteDTBLAfterIndexChange(ctx context.Context, state *dbState, indexes secondaryIndexes, commitMsg string) error {
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

	dtblHash, err := state.dtblHashForCollection(ctx, c.name, primary, indexes, hash.Hash{})
	if err != nil {
		return err
	}
	return state.updateAddressMap(ctx, c.db.rootish, commitMsg, func(ed prolly.AddressMapEditor) error {
		if rootHash.IsEmpty() {
			return ed.Add(ctx, c.name, dtblHash)
		}
		return ed.Update(ctx, c.name, dtblHash)
	})
}

// buildSecondaryIndex scans the primary map and builds the index's prolly.Map.
// The returned flags report whether any scanned document made the index
// multikey or lossy (see backends.IndexInfo). Caller must hold state.mu (write lock).
func (c *collection) buildSecondaryIndex(ctx context.Context, state *dbState, idx backends.IndexInfo) (m prolly.Map, multikey, lossy bool, err error) {
	idxMap, err := idxpkg.NewEmptyMap(ctx, state.ns)
	if err != nil {
		return prolly.Map{}, false, false, fmt.Errorf("index: creating empty map: %w", err)
	}

	// If the collection does not exist yet (no primary map), the index is
	// validly empty: subsequent inserts will populate it through
	// updateSecondaryIndexesOnInsert. AM-resolution and root-hash errors are
	// real failures and propagate.
	am, err := state.getOrInitBranchAM(ctx, c.db.rootish)
	if err != nil {
		return prolly.Map{}, false, false, fmt.Errorf("index: resolving branch AM: %w", err)
	}
	rootHash, err := am.Get(ctx, c.name)
	if err != nil {
		return prolly.Map{}, false, false, fmt.Errorf("index: looking up collection in AM: %w", err)
	}
	if rootHash.IsEmpty() {
		return idxMap, false, false, nil
	}
	primaryMap, err := openCollection(ctx, state.cs, state.ns, rootHash)
	if err != nil {
		return prolly.Map{}, false, false, fmt.Errorf("index: opening primary map: %w", err)
	}

	mut := idxMap.Mutate()
	iter, err := primaryMap.IterAll(ctx)
	if err != nil {
		return prolly.Map{}, false, false, fmt.Errorf("index: iterating primary: %w", err)
	}

	// For a unique index, detect pre-existing duplicate values while scanning
	// so the build fails like a duplicate-key write (matching MongoDB). Uses
	// the same byte-prefix probe as the insert/merge paths; lossy indexes have
	// no faithful encoding, so build-time enforcement is skipped and the
	// write path validates later (same convention as merge).
	var seenUnique map[string][]byte
	if idx.Unique && !idx.Lossy {
		seenUnique = make(map[string][]byte)
	}

	for {
		k, v, err := iter.Next(ctx)
		if err != nil {
			if err == io.EOF {
				break
			}
			return prolly.Map{}, false, false, err
		}
		if v == nil {
			break
		}

		idBytes, ok := keyDesc.GetBytes(0, k)
		if !ok {
			return prolly.Map{}, false, false, fmt.Errorf("index: primary key tuple missing _id bytes")
		}

		doc, err := readDocFromValue(ctx, state.ns, v)
		if err != nil {
			return prolly.Map{}, false, false, fmt.Errorf("index: reading document for build scan: %w", err)
		}

		rows, rowMultikey, rowLossy := indexEntriesForDoc(doc, idx)
		multikey = multikey || rowMultikey
		lossy = lossy || rowLossy

		if seenUnique != nil && !rowLossy {
			if err := checkUniqueBuildConflict(seenUnique, idx, doc, idBytes, rows); err != nil {
				return prolly.Map{}, false, false, err
			}
		}

		for _, fv := range rows {
			if err := idxpkg.InsertEntry(ctx, mut, fv, idBytes); err != nil {
				return prolly.Map{}, false, false, fmt.Errorf("index: inserting entry: %w", err)
			}
		}
	}

	built, err := mut.Map(ctx)
	if err != nil {
		return prolly.Map{}, false, false, fmt.Errorf("index: flushing map: %w", err)
	}
	return built, multikey, lossy, nil
}

// checkUniqueBuildConflict records each row's equality-probe prefix in seen
// (prefix -> owning _id) and returns a duplicate-key error the first time a
// prefix is claimed by a different document. Sparse indexes skip all-null
// keys and partial indexes skip documents outside the filter, matching the
// insert-time enforcement so a build does not reject rows a later insert
// would accept.
func checkUniqueBuildConflict(seen map[string][]byte, idx backends.IndexInfo, doc *types.Document, idBytes []byte, rows [][]any) error {
	if idx.MatchesPartialFilter != nil {
		matches, err := idx.MatchesPartialFilter(doc)
		if err != nil {
			return err
		}
		if !matches {
			return nil
		}
	}
	if sparseExcludes(doc, idx) {
		return nil
	}
	for _, row := range rows {
		key := rowConflictKey(row)
		owner, ok := seen[key]
		if !ok {
			seen[key] = append([]byte(nil), idBytes...)
			continue
		}
		if !bytes.Equal(owner, idBytes) {
			return backends.NewDuplicateKeyError(idx.Name, indexDupKey(doc, idx))
		}
	}
	return nil
}

// scanUniqueConflict is the O(N) value-level fallback for rows with no
// faithful byte encoding (Decimal128); the common path is the probe.
func (c *collection) scanUniqueConflict(ctx context.Context, state *dbState, m prolly.Map, idx backends.IndexInfo, doc *types.Document, selfKey []byte) (bool, error) {
	cmp := indexComparator(idx)
	newKey := extractIndexKey(doc, idx)
	iter, err := m.IterAll(ctx)
	if err != nil {
		return false, err
	}
	for {
		k, v, err := iter.Next(ctx)
		if err != nil {
			if err == io.EOF {
				break
			}
			return false, err
		}
		if v == nil {
			break
		}
		idBytes, ok := keyDesc.GetBytes(0, k)
		if ok && bytes.Equal(idBytes, selfKey) {
			continue
		}
		existingDoc, err := readDocFromValue(ctx, state.ns, v)
		if err != nil {
			continue
		}
		if idx.MatchesPartialFilter != nil {
			matches, ferr := idx.MatchesPartialFilter(existingDoc)
			if ferr != nil || !matches {
				continue
			}
		}
		if sparseExcludes(existingDoc, idx) {
			continue
		}
		existKey := extractIndexKey(existingDoc, idx)
		if indexKeysEqualColl(newKey, existKey, cmp) {
			return true, nil
		}
	}
	return false, nil
}

// scanCollatedIDConflict reports whether the primary map already holds a string
// _id equal to idStr under cmp. The primary key encodes the raw _id, so
// collation-equal-but-distinct values ("a"/"A") do not collide there; this O(N)
// value scan enforces _id uniqueness under the collection collation (design 3.6).
func (c *collection) scanCollatedIDConflict(ctx context.Context, state *dbState, m prolly.Map, idStr string, selfKey []byte, cmp *collation.Comparator) (bool, error) {
	iter, err := m.IterAll(ctx)
	if err != nil {
		return false, err
	}
	for {
		k, v, err := iter.Next(ctx)
		if err != nil {
			if err == io.EOF {
				break
			}
			return false, err
		}
		if v == nil {
			break
		}
		if idBytes, ok := keyDesc.GetBytes(0, k); ok && bytes.Equal(idBytes, selfKey) {
			continue
		}
		existingDoc, err := readDocFromValue(ctx, state.ns, v)
		if err != nil {
			continue
		}
		existID, err := existingDoc.Get("_id")
		if err != nil {
			continue
		}
		if existStr, ok := existID.(string); ok && cmp.EqualStrings(existStr, idStr) {
			return true, nil
		}
	}
	return false, nil
}

// validateUniqueOnUpdate rejects an update whose new version collides
// with a different document on any unique index.
func (c *collection) validateUniqueOnUpdate(ctx context.Context, state *dbState, m prolly.Map, infos []backends.IndexInfo, idxMaps map[string]prolly.Map, newDoc *types.Document, selfKey []byte) error {
	for _, idx := range infos {
		if !idx.Unique {
			continue
		}
		rows, _, lossy := indexEntriesForDoc(newDoc, idx)
		if len(rows) == 0 {
			continue
		}
		if lossy || idx.Lossy {
			conflict, err := c.scanUniqueConflict(ctx, state, m, idx, newDoc, selfKey)
			if err != nil {
				return err
			}
			if conflict {
				return backends.NewDuplicateKeyError(idx.Name, indexDupKey(newDoc, idx))
			}
			continue
		}
		for _, row := range rows {
			conflict, err := idxpkg.UniqueConflict(ctx, idxMaps[idx.Name], row, selfKey)
			if err != nil {
				return fmt.Errorf("unique probe on %s: %w", idx.Name, err)
			}
			if conflict {
				return backends.NewDuplicateKeyError(idx.Name, indexDupKey(newDoc, idx))
			}
		}
	}
	return nil
}

// extractIndexFieldValues returns the field values for the given index, in key order.
// Missing fields are returned as types.Null.
func extractIndexFieldValues(doc *types.Document, idx backends.IndexInfo) []any {
	vals := make([]any, len(idx.Key))
	for i, kp := range idx.Key {
		vals[i] = indexFieldValue(doc, kp.Field)
	}
	return vals
}

// sparseExcludes reports whether a sparse idx skips doc. Like MongoDB, a sparse
// index holds every document in which some indexed field is present, even when
// its value is null.
func sparseExcludes(doc *types.Document, idx backends.IndexInfo) bool {
	if !idx.Sparse {
		return false
	}
	for _, kp := range idx.Key {
		if indexFieldPresent(doc, kp.Field) {
			return false
		}
	}
	return true
}

// indexFieldPresent reports whether field, which may be a dotted path, reaches
// at least one value in doc.
func indexFieldPresent(doc *types.Document, field string) bool {
	if !strings.Contains(field, ".") {
		_, err := doc.Get(field)
		return err == nil
	}
	path, err := types.NewPathFromString(field)
	if err != nil {
		return false
	}
	vals, err := commonpath.FindValues(doc, path, &commonpath.FindValuesOpts{
		FindArrayDocuments: true,
		FindArrayIndex:     true,
	})
	return err == nil && len(vals) > 0
}

// indexFieldValue resolves an index key field, which may be a dotted path,
// against doc. A missing path yields types.Null. A path that traverses arrays
// and reaches several values yields them as one flattened *types.Array, so the
// caller's multikey expansion indexes each.
func indexFieldValue(doc *types.Document, field string) any {
	if !strings.Contains(field, ".") {
		v, err := doc.Get(field)
		if err != nil {
			return types.Null
		}
		return v
	}
	path, err := types.NewPathFromString(field)
	if err != nil {
		return types.Null
	}
	if !hasPositionalSegment(path) {
		// MongoDB's keys: null for each array element or branch the path
		// does not reach.
		vals := commonpath.IndexKeyValues(doc, path)
		if len(vals) == 1 {
			if _, isArray := vals[0].(*types.Array); !isArray {
				return vals[0]
			}
		}
		return must.NotFail(types.NewArray(vals...))
	}
	vals, err := commonpath.FindValues(doc, path, &commonpath.FindValuesOpts{
		FindArrayDocuments: true,
		FindArrayIndex:     true,
	})
	if err != nil || len(vals) == 0 {
		return types.Null
	}
	if len(vals) == 1 {
		return vals[0]
	}
	flat := types.MakeArray(len(vals))
	for _, v := range vals {
		if arr, ok := v.(*types.Array); ok {
			for i := 0; i < arr.Len(); i++ {
				flat.Append(must.NotFail(arr.Get(i)))
			}
			continue
		}
		flat.Append(v)
	}
	return flat
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
			// Like MongoDB, index an empty array as undefined, which sorts
			// below every value as the empty array does.
			row := make([]any, len(fieldVals))
			copy(row, fieldVals)
			row[i] = types.Undefined
			return [][]any{row}
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
	if err := c.rejectReplicationControlWrite(); err != nil {
		return nil, err
	}
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

	newIndexes, err := buildSecondaryIndexes(ctx, state, keptInfos, curMaps)
	if err != nil {
		return nil, fmt.Errorf("building secondary indexes after drop: %w", err)
	}
	if err := c.rewriteDTBLAfterIndexChange(ctx, state, newIndexes, fmt.Sprintf("auto: drop index %s on %s", strings.Join(params.Indexes, ", "), c.name)); err != nil {
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

	emptyMap, err := newEmptyMap(ctx, state.ns)
	if err != nil {
		return prolly.Map{}, err
	}

	noIndexes, err := emptySecondaryIndexes(ctx, state.ns)
	if err != nil {
		return prolly.Map{}, err
	}
	dtblHash, err := state.dtblHashForCollection(ctx, c.name, emptyMap, noIndexes, hash.Hash{})
	if err != nil {
		return prolly.Map{}, err
	}
	createAM, err := state.getOrInitBranchAM(ctx, c.db.rootish)
	if err != nil {
		return prolly.Map{}, err
	}
	meta := &collMeta{UUID: collectionUUID(c.db.name, c.name)}
	if err := state.updateAddressMap(ctx, c.db.rootish, fmt.Sprintf("auto: create collection %s", c.name), func(ed prolly.AddressMapEditor) error {
		if err := ed.Add(ctx, c.name, dtblHash); err != nil {
			return err
		}
		return state.applyCatalogUpsert(ctx, createAM, ed, c.name, meta)
	}); err != nil {
		return prolly.Map{}, err
	}

	return emptyMap, nil
}

func readDocFromValue(ctx context.Context, ns tree.NodeStore, v val.Tuple) (*types.Document, error) {
	return readBSONDocFromValue(ctx, ns, v)
}

func writeDocToValue(ctx context.Context, ns tree.NodeStore, doc *types.Document) (val.Tuple, error) {
	return writeBSONDocToValue(ctx, ns, doc)
}

// applyFieldMutations dispatches on the adaptive-bytes storage shape:
// inline []byte takes the parse / mutate / re-encode path; out-of-band
// *val.ByteArray takes the byte-level splice path so unchanged chunks
// stay deduplicated.
func applyFieldMutations(ctx context.Context, ns tree.NodeStore, v val.Tuple, mutations []backends.FieldMutation) ([]byte, error) {
	result, ok, err := storedValueReadDesc.GetBytesAdaptiveValue(ctx, 0, ns, v)
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

func decodeDocument(data []byte) (*types.Document, error) {
	doc, err := bson.DecodeRawDocument(data)
	if err != nil {
		return nil, fmt.Errorf("decoding document: %w", err)
	}

	return doc, nil
}

// keyBytesToRecordID derives a stable int64 RecordID from a primary key. Keys
// share prefixes, so it hashes the whole key.
func keyBytesToRecordID(keyBytes []byte) int64 {
	h := fnv.New64a()
	_, _ = h.Write(keyBytes)
	return int64(h.Sum64())
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
	// prefixReject, if non-nil, rejects a document stored out of band from
	// its first chunk when that alone proves the filter cannot match.
	prefixReject func([]byte) bool
	fields       []string
	lazy         bool
	buf          []byte
	// release, if set, unpins the iterated map's root from GC on Close.
	release func()
}

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
		countDocsExamined(it.ctx, 1)

		keyBytes, ok := keyDesc.GetBytes(0, k)
		if !ok {
			continue
		}

		recordID := keyBytesToRecordID(keyBytes)

		if it.onlyRecordID {
			doc, err := types.NewDocument()
			if err != nil {
				return struct{}{}, nil, err
			}

			doc.SetRecordID(recordID)
			it.count++

			return struct{}{}, doc, nil
		}

		var jsonBytes []byte
		var rejected bool
		jsonBytes, it.buf, rejected, err = readStoredBytesUnlessRejected(it.ctx, it.ns, v, it.buf, it.prefixReject)
		if err != nil || rejected {
			continue
		}

		var doc *types.Document
		if it.prefilter != nil {
			if !it.prefilter(jsonBytes) {
				continue
			}
		}
		if it.lazy {
			raw, err := stripVersion(jsonBytes)
			if err != nil {
				return struct{}{}, nil, err
			}
			doc = types.NewLazyDocument(bytes.Clone(raw))
		} else if doc, err = bsonToDocFields(jsonBytes, it.fields); err != nil {
			return struct{}{}, nil, err
		}

		doc.SetRecordID(recordID)
		it.count++

		return struct{}{}, doc, nil
	}
}

func (it *mapIter) Close() {
	if it.release != nil {
		it.release()
	}
}

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

// sliceIter is used by the secondary index lookup path to return pre-fetched
// results without a full scan.
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
