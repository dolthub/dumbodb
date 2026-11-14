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

package stages

import (
	"context"
	"errors"
	"fmt"
	"math"
	stdsort "sort"
	"strconv"

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// graphLookup represents the $graphLookup aggregation stage.
//
// It performs a recursive graph traversal, starting from the values produced by
// startWith and following the connectFromField -> connectToField edges in the
// "from" collection until no new documents are discovered or maxDepth is reached.
//
// Syntax:
//
//	{ $graphLookup: {
//	    from:                    <collection>,
//	    startWith:               <expression>,
//	    connectFromField:        <string>,
//	    connectToField:          <string>,
//	    as:                      <string>,
//	    maxDepth:                <number>,           // optional; 0-based
//	    depthField:              <string>,           // optional
//	    restrictSearchWithMatch: <document>,         // optional
//	} }
type graphLookup struct {
	cmp                     *collation.Comparator
	from                    string
	startWith               any // field-path expression ("$field") or literal
	connectFromField        string
	connectToField          string
	as                      string
	maxDepth                int64           // -1 means unlimited
	depthField              string          // empty means not set
	restrictSearchWithMatch *types.Document // nil means no filter
	fetcher                 CollectionFetcher
}

func NewGraphLookupStage(stage *types.Document, fetcher CollectionFetcher) (aggregations.Stage, error) {
	spec, err := stage.Get("$graphLookup")
	if err != nil {
		return nil, err
	}

	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$graphLookup (stage)")
	}

	specDoc, ok := spec.(*types.Document)
	if !ok {
		return nil, fail(handlererrors.ErrFailedToParse,
			"the $graphLookup stage specification must be an object, but found "+handlerparams.AliasFromType(spec))
	}

	// mongod reads 'from' first, then the other fields in order.
	fromVal, err := specDoc.Get("from")
	if err != nil {
		return nil, fail(handlererrors.ErrFailedToParse,
			"missing 'from' option to $graphLookup stage specification: "+types.FormatAnyValue(specDoc))
	}
	from, ok := fromVal.(string)
	if !ok {
		if fromDoc, isDoc := fromVal.(*types.Document); isDoc {
			db, _ := fromDoc.Get("db")
			coll, _ := fromDoc.Get("coll")
			return nil, fail(handlererrors.ErrFailedToParse, fmt.Sprintf(
				"$graphLookup with syntax {from: {db:<>, coll:<>},..} is not supported for db: %v and coll: %v", db, coll))
		}
		return nil, fail(handlererrors.ErrFailedToParse,
			"$graphLookup 'from' field must be a string, but found "+handlerparams.AliasFromType(fromVal))
	}

	gl := &graphLookup{
		cmp:      operators.CollationOf(stage),
		from:     from,
		maxDepth: -1,
		fetcher:  fetcher,
	}

	var hasStartWith bool
	for _, key := range specDoc.Keys() {
		v := must.NotFail(specDoc.Get(key))
		switch key {
		case "from":
		case "startWith":
			if err = checkStartWith(v); err != nil {
				return nil, err
			}
			gl.startWith, hasStartWith = v, true
		case "maxDepth":
			if gl.maxDepth, err = graphLookupMaxDepth(v); err != nil {
				return nil, err
			}
		case "restrictSearchWithMatch":
			match, isDoc := v.(*types.Document)
			if !isDoc {
				return nil, fail(handlererrors.ErrGraphLookupMatchNotObject,
					"restrictSearchWithMatch must be an object, found "+handlerparams.AliasFromType(v))
			}
			common.CanonicalizeFilter(match)
			if filterHasText(match) {
				err = handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
					"$text is not allowed in this context", "$graphLookup (stage)")
			} else if err = validateMatch(match); err == nil {
				err = common.ValidateFilter(match, gl.cmp)
			}
			if err != nil {
				var cerr *handlererrors.CommandError
				if errors.As(err, &cerr) {
					return nil, cerr.WithMessage("Failed to parse 'restrictSearchWithMatch' option to $graphLookup :: caused by :: " + cerr.Err().Error())
				}
				return nil, err
			}
			gl.restrictSearchWithMatch = match
		case "as", "connectFromField", "connectToField", "depthField":
			str, isString := v.(string)
			if !isString {
				return nil, fail(handlererrors.ErrGraphLookupNotString, fmt.Sprintf(
					"expected string as argument for %s, found: %s", key, handlerparams.AliasFromType(v)))
			}
			if err = checkFieldPathArgument(str, "$graphLookup"); err != nil {
				return nil, err
			}
			switch key {
			case "as":
				gl.as = str
			case "connectFromField":
				gl.connectFromField = str
			case "connectToField":
				gl.connectToField = str
			case "depthField":
				gl.depthField = str
			}
		default:
			return nil, fail(handlererrors.ErrGraphLookupUnknownArgument, "Unknown argument to $graphLookup: "+key)
		}
	}

	if !hasStartWith || gl.as == "" || gl.connectFromField == "" || gl.connectToField == "" {
		return nil, fail(handlererrors.ErrGraphLookupMissingArgument,
			"$graphLookup requires 'from', 'as', 'startWith', 'connectFromField', and 'connectToField' to be specified.")
	}

	return gl, nil
}

// filterHasText reports whether filter holds $text, at the top level or in a
// logical operator's clauses.
func filterHasText(filter *types.Document) bool {
	for _, k := range filter.Keys() {
		switch k {
		case "$text":
			return true
		case "$and", "$or", "$nor":
			clauses, ok := must.NotFail(filter.Get(k)).(*types.Array)
			if !ok {
				continue
			}
			for i := range clauses.Len() {
				if c, ok := must.NotFail(clauses.Get(i)).(*types.Document); ok && filterHasText(c) {
					return true
				}
			}
		}
	}
	return false
}

// graphLookupMaxDepth returns a $graphLookup maxDepth, which must be a
// non-negative integral number.
func graphLookupMaxDepth(v any) (int64, error) {
	fail := func(code handlererrors.ErrorCode, msg string) (int64, error) {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$graphLookup (stage)")
	}
	var f float64
	switch n := v.(type) {
	case int32:
		f = float64(n)
	case int64:
		if n < 0 {
			return fail(handlererrors.ErrGraphLookupMaxDepthNegative, fmt.Sprintf("maxDepth requires a nonnegative argument, found: %d", n))
		}
		return n, nil
	case float64:
		f = n
	case types.Decimal128:
		d, err := strconv.ParseFloat(primitive.NewDecimal128(n.H, n.L).String(), 64)
		if err != nil {
			return fail(handlererrors.ErrGraphLookupMaxDepthNotIntegral, "maxDepth could not be represented as a long long: "+types.FormatAnyValue(v))
		}
		f = d
	default:
		return fail(handlererrors.ErrGraphLookupMaxDepthNotNumeric, "maxDepth must be numeric, found type: "+handlerparams.AliasFromType(v))
	}
	// mongod's messages show the value cast to a long long.
	asLong := int64(math.MaxInt64)
	if f < math.MaxInt64 && f > math.MinInt64 {
		asLong = int64(f)
	}
	if f < 0 || math.IsNaN(f) {
		return fail(handlererrors.ErrGraphLookupMaxDepthNegative, fmt.Sprintf("maxDepth requires a nonnegative argument, found: %d", asLong))
	}
	if f != math.Trunc(f) || f >= math.MaxInt64 {
		return fail(handlererrors.ErrGraphLookupMaxDepthNotIntegral, fmt.Sprintf("maxDepth could not be represented as a long long: %d", asLong))
	}
	return int64(f), nil
}

func (gl *graphLookup) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := common.ConsumeDocuments(iter, "$graphLookup")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	fetcher := withFetchCache(gl.fetcher)
	if err := checkViewCollation(ctx, fetcher, gl.from, gl.cmp); err != nil {
		return nil, err
	}
	ctx = WithJoinStages(ctx, JoinStages{N: 1})
	var src graphSource
	if gl.cmp == nil && fetcher.IndexedField(ctx, gl.from, gl.connectToField) {
		src = &indexedGraphSource{fetcher: fetcher, from: gl.from, connectToField: gl.connectToField}
	} else {
		fromDocs, fErr := fetcher.Fetch(ctx, gl.from, nil)
		if fErr != nil {
			return nil, lazyerrors.Error(fErr)
		}
		src = newGraphIndex(fromDocs, gl.connectToField, gl.cmp)
	}

	out := make([]*types.Document, 0, len(docs))

	for _, doc := range docs {
		visited, gErr := gl.traverse(ctx, doc, src)
		if gErr != nil {
			return nil, gErr
		}

		newDoc := doc.DeepCopy()
		arr := types.MakeArray(len(visited))

		for _, v := range visited {
			arr.Append(v)
		}

		newDoc.Set(gl.as, arr)
		out = append(out, newDoc)
	}

	result := iterator.Values(iterator.ForSlice(out))
	closer.Add(result)

	return result, nil
}

// traverse performs a breadth-first recursive graph traversal for one input document.
//
// It starts with the values produced by evaluating startWith against doc, then
// repeatedly looks up documents in fromDocs where connectToField matches a queued
// value, and queues those documents' connectFromField values for the next iteration.
//
// Cycle prevention: each connectToField search-value is only processed once, so
// cycles in the graph terminate naturally.
//
// Ordering: MongoDB returns depth-0 results first, then the remaining depth levels in
// reverse order (deepest first). Within each depth level, results appear in _id-ascending
// order (collection-scan proxy). Concretely, for a chain a(d0)->b(d1)->c(d2), MongoDB
// returns [a, c, b]  -- depth-0 first, then d2 before d1.
func (gl *graphLookup) traverse(ctx context.Context, doc *types.Document, src graphSource) ([]*types.Document, error) {
	// searchedKeys guards against re-processing the same connectToField search value.
	searchedKeys := make(map[string]bool)

	// resultIDs deduplicates result documents by their _id (or connectToField value).
	resultIDs := make(map[string]bool)

	// levelResults accumulates per-depth-level slices so we can reconstruct MongoDB's
	// ordering: depth-0 first, then depth-N, depth-(N-1), ..., depth-1.
	var levelResults [][]*types.Document

	frontier, err := evaluateStartWith(gl.startWith, doc)
	if err != nil {
		return nil, err
	}
	currentDepth := int64(0)

	for len(frontier) > 0 {
		if gl.maxDepth >= 0 && currentDepth > gl.maxDepth {
			break
		}

		// Build the set of frontier search-values for this depth level, skipping any
		// already processed (cycle prevention). Mark them searched immediately so that
		// even if a doc's connectFromField re-enqueues a value seen at a prior level,
		// it is silently dropped.
		frontierSet := make(map[string]bool, len(frontier))
		var frontierValues []any
		for _, searchVal := range frontier {
			key := graphKey(gl.cmp, searchVal)
			if !searchedKeys[key] {
				frontierSet[key] = true
				searchedKeys[key] = true
				frontierValues = append(frontierValues, searchVal)
			}
		}

		if len(frontierSet) == 0 {
			break
		}

		// nextFrontierSet deduplicates values enqueued for the next depth level.
		nextFrontierSet := make(map[string]bool)
		var nextFrontier []any

		var levelDocs []*types.Document

		// Visit this level's matches in _id order, regardless of which frontier
		// value they match, as MongoDB's per-level collection scan does.
		matches, mErr := src.levelMatches(ctx, frontierSet, frontierValues)
		if mErr != nil {
			return nil, lazyerrors.Error(mErr)
		}
		for _, fromDoc := range matches {

			if gl.restrictSearchWithMatch != nil {
				matched, mErr := common.FilterDocumentColl(fromDoc, gl.restrictSearchWithMatch, gl.cmp)
				if mErr != nil || !matched {
					continue
				}
			}

			docID := docIdentityKey(fromDoc)
			if resultIDs[docID] {
				continue
			}

			resultIDs[docID] = true

			result := fromDoc.DeepCopy()
			if gl.depthField != "" {
				result.Set(gl.depthField, int64(currentDepth))
			}

			levelDocs = append(levelDocs, result)

			// Enqueue the connectFromField value(s) for the next depth level.
			fromVal := getFieldValue(fromDoc, gl.connectFromField)
			if fromVal != nil {
				var vals []any
				if fromArr, ok := fromVal.(*types.Array); ok {
					vals = arrayElements(fromArr)
				} else {
					vals = []any{fromVal}
				}

				for _, v := range vals {
					k := graphKey(gl.cmp, v)
					if !searchedKeys[k] && !nextFrontierSet[k] {
						nextFrontierSet[k] = true
						nextFrontier = append(nextFrontier, v)
					}
				}
			}
		}

		if len(levelDocs) > 0 {
			levelResults = append(levelResults, levelDocs)
		}

		frontier = nextFrontier
		currentDepth++
	}

	// Reconstruct MongoDB's ordering: depth-0 first, then remaining levels deepest-first.
	// For levels [L0, L1, L2, ..., LN], output: [L0, LN, LN-1, ..., L1].
	var results []*types.Document
	if len(levelResults) > 0 {
		results = append(results, levelResults[0]...)
		for i := len(levelResults) - 1; i >= 1; i-- {
			results = append(results, levelResults[i]...)
		}
	}

	return results, nil
}

// graphSource returns, in _id order, the foreign documents whose
// connectToField key is in keys; values are the frontier values behind keys.
type graphSource interface {
	levelMatches(ctx context.Context, keys map[string]bool, values []any) ([]*types.Document, error)
}

// indexedGraphSource fetches each traversal level's matches through the index
// on connectToField. A level with a value that cannot be pushed down reads the
// whole collection, loaded once.
type indexedGraphSource struct {
	fetcher        CollectionFetcher
	from           string
	connectToField string
	all            *graphIndex
}

func (src *indexedGraphSource) levelMatches(ctx context.Context, keys map[string]bool, values []any) ([]*types.Document, error) {
	filter := equalityFetchFilter(valuesAsDocs(values), "v", src.connectToField)
	if filter == nil {
		if src.all == nil {
			docs, err := src.fetcher.Fetch(ctx, src.from, nil)
			if err != nil {
				return nil, err
			}
			src.all = newGraphIndex(docs, src.connectToField, nil)
		}
		return src.all.levelMatches(ctx, keys, values)
	}

	fetched, err := src.fetcher.Fetch(ctx, src.from, filter)
	if err != nil {
		return nil, err
	}
	matches := make([]*types.Document, 0, len(fetched))
	for _, d := range fetched {
		if keys[graphKey(nil, getFieldValue(d, src.connectToField))] {
			matches = append(matches, d)
		}
	}
	sortByID(matches)
	return matches, nil
}

// graphKey is the traversal's key for a connect value, with strings as sort
// keys under cmp when it is set.
func graphKey(cmp *collation.Comparator, v any) string {
	return fmt.Sprintf("%v", cmp.KeyValue(v))
}

// valuesAsDocs wraps values as {v: value} documents for equalityFetchFilter.
func valuesAsDocs(values []any) []*types.Document {
	docs := make([]*types.Document, 0, len(values))
	for _, v := range values {
		d := new(types.Document)
		if v != nil {
			d.Set("v", v)
		}
		docs = append(docs, d)
	}
	return docs
}

func sortByID(docs []*types.Document) {
	stdsort.SliceStable(docs, func(i, j int) bool {
		idI, errI := docs[i].Get("_id")
		idJ, errJ := docs[j].Get("_id")
		if errI != nil || errJ != nil {
			return false
		}
		return types.CompareOrder(idI, idJ, types.Ascending) == types.Less
	})
}

// graphIndex holds the foreign documents sorted by _id, with their positions
// grouped by connectToField key, so a traversal level reads only its matches.
type graphIndex struct {
	sorted []*types.Document
	byKey  map[string][]int
}

func newGraphIndex(fromDocs []*types.Document, connectToField string, cmp *collation.Comparator) *graphIndex {
	sorted := make([]*types.Document, len(fromDocs))
	copy(sorted, fromDocs)
	sortByID(sorted)

	byKey := make(map[string][]int)
	for pos, d := range sorted {
		key := graphKey(cmp, getFieldValue(d, connectToField))
		byKey[key] = append(byKey[key], pos)
	}
	return &graphIndex{sorted: sorted, byKey: byKey}
}

func (idx *graphIndex) levelMatches(_ context.Context, keys map[string]bool, _ []any) ([]*types.Document, error) {
	var positions []int
	for key := range keys {
		positions = append(positions, idx.byKey[key]...)
	}
	stdsort.Ints(positions)

	matches := make([]*types.Document, len(positions))
	for i, pos := range positions {
		matches[i] = idx.sorted[pos]
	}
	return matches, nil
}

// checkStartWith returns the error mongod raises while parsing the startWith
// expression. mongod does not fold its constants.
func checkStartWith(startWith any) error {
	if doc, ok := startWith.(*types.Document); ok && operators.IsOperator(doc) {
		if err := operators.CheckExpressionShape(doc); err != nil {
			return err
		}
	}
	return operators.CheckFieldPaths(startWith)
}

// evaluateStartWith evaluates the startWith expression against an input
// document, returning the initial set of values for the graph frontier: the
// elements of an array, nothing for a missing value, or the value itself.
func evaluateStartWith(startWith any, doc *types.Document) ([]any, error) {
	v, missing, err := operators.EvalOperand(startWith, doc)
	if err != nil || missing {
		return nil, err
	}
	if arr, ok := v.(*types.Array); ok {
		return arrayElements(arr), nil
	}
	return []any{v}, nil
}

func arrayElements(arr *types.Array) []any {
	result := make([]any, 0, arr.Len())
	arrIter := arr.Iterator()

	defer arrIter.Close()

	for {
		_, v, err := arrIter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			break
		}

		result = append(result, v)
	}

	return result
}

// docIdentityKey returns a string key that uniquely identifies a document.
// It prefers the _id field; falls back to a formatted representation.
func docIdentityKey(doc *types.Document) string {
	id, err := doc.Get("_id")
	if err == nil {
		return fmt.Sprintf("_id:%v", id)
	}

	return fmt.Sprintf("doc:%v", doc)
}

var (
	_ aggregations.Stage = (*graphLookup)(nil)
)
