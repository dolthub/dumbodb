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
	"strings"
	"time"

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
)

type joinStagesKey struct{}

// JoinStages describes, for fetches of a join's foreign view, the stages
// mongod runs the view as besides the view's own, and refuses as it executes
// when that is over the limit.
type JoinStages struct {
	// N is the join's own stages; 0 means no check.
	N int
	// ViewLimit reports that mongod refuses with ViewPipelineTooLong, as for a
	// $lookup with only a pipeline, rather than PipelineTooLong.
	ViewLimit bool
}

// WithJoinStages records js for fetches under ctx.
func WithJoinStages(ctx context.Context, js JoinStages) context.Context {
	return context.WithValue(ctx, joinStagesKey{}, js)
}

// JoinStagesOf returns the JoinStages WithJoinStages recorded.
func JoinStagesOf(ctx context.Context) JoinStages {
	js, _ := ctx.Value(joinStagesKey{}).(JoinStages)
	return js
}

// CollectionFetcher reads the foreign side of $lookup and $graphLookup.
type CollectionFetcher interface {
	// Fetch returns the documents of collectionName that match filter (all of
	// them when filter is nil). It may return extra documents; callers apply
	// their own matching, and must not modify the returned documents.
	Fetch(ctx context.Context, collectionName string, filter *types.Document) ([]*types.Document, error)

	// IndexedField reports whether an equality filter on field of
	// collectionName can be served by an index.
	IndexedField(ctx context.Context, collectionName, field string) bool
}

// StatsFetcher is implemented by fetchers that can run a sub-pipeline that
// starts with $collStats on collectionName.
type StatsFetcher interface {
	CollStats(ctx context.Context, collectionName string, pipeline []aggregations.Stage, closer *iterator.MultiCloser) (types.DocumentsIterator, error) //nolint:lll // for readability
}

// FetcherFunc adapts a function to a CollectionFetcher that has no indexes.
type FetcherFunc func(ctx context.Context, collectionName string, filter *types.Document) ([]*types.Document, error)

func (f FetcherFunc) Fetch(ctx context.Context, collectionName string, filter *types.Document) ([]*types.Document, error) {
	return f(ctx, collectionName, filter)
}

func (FetcherFunc) IndexedField(context.Context, string, string) bool { return false }

// cachingFetcher remembers unfiltered fetches and index answers for the
// duration of one stage, so a $lookup nested in a sub-pipeline does not reload
// the same collection for every outer document.
type cachingFetcher struct {
	CollectionFetcher
	all       map[string][]*types.Document
	indexed   map[string]bool
	eqIndexes map[string]*equalityIndex
}

// ViewResolver is implemented by fetchers that can report the full pipeline
// of a view.
type ViewResolver interface {
	ViewPipeline(ctx context.Context, name string) (*types.Array, bool)
}

// ExistenceChecker is implemented by fetchers that can report whether a
// collection, or the collection a view reads, exists.
type ExistenceChecker interface {
	SourceExists(ctx context.Context, name string) bool
}

// SourceExists implements ExistenceChecker, reporting true when the wrapped
// fetcher cannot tell.
func (c *cachingFetcher) SourceExists(ctx context.Context, name string) bool {
	if ec, ok := c.CollectionFetcher.(ExistenceChecker); ok {
		return ec.SourceExists(ctx, name)
	}
	return true
}

// ViewPipeline implements ViewResolver when the wrapped fetcher does.
func (c *cachingFetcher) ViewPipeline(ctx context.Context, name string) (*types.Array, bool) {
	if vr, ok := c.CollectionFetcher.(ViewResolver); ok {
		return vr.ViewPipeline(ctx, name)
	}
	return nil, false
}

// CollStats implements StatsFetcher when the wrapped fetcher does.
func (c *cachingFetcher) CollStats(ctx context.Context, collectionName string, pipeline []aggregations.Stage, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	sf, ok := c.CollectionFetcher.(StatsFetcher)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrNotImplemented,
			"$collStats is not supported here", "$collStats (stage)")
	}
	return sf.CollStats(ctx, collectionName, pipeline, closer)
}

func withFetchCache(f CollectionFetcher) *cachingFetcher {
	if c, ok := f.(*cachingFetcher); ok {
		return c
	}
	return &cachingFetcher{
		CollectionFetcher: f,
		all:               map[string][]*types.Document{},
		indexed:           map[string]bool{},
		eqIndexes:         map[string]*equalityIndex{},
	}
}

// unfilteredEqualityIndex returns the equality index on field over the cached
// unfiltered documents of collectionName, building it once.
func (c *cachingFetcher) unfilteredEqualityIndex(ctx context.Context, collectionName, field string, cmp *collation.Comparator) (*equalityIndex, error) {
	key := collectionName + "\x00" + field
	if cmp != nil {
		key += "\x00collated"
	}
	if idx, ok := c.eqIndexes[key]; ok {
		return idx, nil
	}
	docs, err := c.Fetch(ctx, collectionName, nil)
	if err != nil {
		return nil, err
	}
	idx := newEqualityIndex(docs, field, cmp)
	c.eqIndexes[key] = idx
	return idx, nil
}

func (c *cachingFetcher) Fetch(ctx context.Context, collectionName string, filter *types.Document) ([]*types.Document, error) {
	if filter != nil {
		return c.CollectionFetcher.Fetch(ctx, collectionName, filter)
	}
	if docs, ok := c.all[collectionName]; ok {
		return docs, nil
	}
	docs, err := c.CollectionFetcher.Fetch(ctx, collectionName, nil)
	if err != nil {
		return nil, err
	}
	c.all[collectionName] = docs
	return docs, nil
}

func (c *cachingFetcher) IndexedField(ctx context.Context, collectionName, field string) bool {
	key := collectionName + "\x00" + field
	indexed, ok := c.indexed[key]
	if !ok {
		indexed = c.CollectionFetcher.IndexedField(ctx, collectionName, field)
		c.indexed[key] = indexed
	}
	return indexed
}

// lookup represents $lookup stage.
//
// Simple equality join form:
//
//	{ $lookup: { from: <coll>, localField: <field>, foreignField: <field>, as: <array field> } }
//
// Pipeline form (with optional let variables):
//
//	{ $lookup: { from: <coll>, let: { <var>: <expr>, ... }, pipeline: [...], as: <array field> } }
type lookup struct {
	from         string
	localField   string          // simple form
	foreignField string          // simple form
	letVars      *types.Document // pipeline form: variable bindings
	pipeline     *types.Array    // pipeline form (may be nil)
	as           string
	fetcher      CollectionFetcher
	// unwind is the following $unwind of as, which this stage performs by
	// emitting one document per match (see MarkLookupsUnwound).
	unwind *unwind
	// sbePrefix is how many leading sub-pipeline stages report errors as
	// mongod's SBE engine does; see SBEPrefixLen.
	sbePrefix int
	// sbe is set when mongod runs this equality $lookup in SBE, which limits
	// the size of one document's matches differently; see MarkSBELookups.
	sbe bool
	// sbeSubPipeline is set for a $unionWith sub-pipeline, whose leading
	// stages mongod runs in SBE.
	sbeSubPipeline bool
	// inClassic is set for a sub-pipeline nested in a $lookup sub-pipeline,
	// which mongod runs entirely in its classic engine.
	inClassic bool
	// viewIsView reports which collections are views, for MarkSBELookups.
	viewIsView func(string) bool
	// before are the stages between this stage and its absorbed $unwind,
	// which run on the input, as mongod moves them ahead of the $lookup.
	before []aggregations.Stage
	// cmp is the command's collation, under which join values compare.
	cmp *collation.Comparator
}

// NewLookupStage creates a new $lookup stage with a collection fetcher.
// The fetcher provides access to the "from" collection.
func NewLookupStage(stage *types.Document, fetcher CollectionFetcher) (aggregations.Stage, error) {
	spec, err := stage.Get("$lookup")
	if err != nil {
		return nil, err
	}

	specDoc, ok := spec.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			fmt.Sprintf("$lookup specification must be an object, got %s", types.FormatAnyValue(spec)),
			"$lookup (stage)",
		)
	}

	// "from" is required, unless the pipeline starts with $documents.
	fromVal, err := specDoc.Get("from")
	if err != nil && !startsWithDocuments(specDoc) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"$lookup requires a 'from' option",
			"$lookup (stage)",
		)
	}

	from, ok := fromVal.(string)
	if err == nil && !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			fmt.Sprintf("$lookup 'from' must be a string, got %s", types.FormatAnyValue(fromVal)),
			"$lookup (stage)",
		)
	}
	if err == nil {
		if err = checkSubPipelineSource(from, specDoc, "$lookup"); err != nil {
			return nil, err
		}
	}

	// "as" is required.
	asVal, err := specDoc.Get("as")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"$lookup requires an 'as' option",
			"$lookup (stage)",
		)
	}

	asField, ok := asVal.(string)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			fmt.Sprintf("$lookup 'as' must be a string, got %s", types.FormatAnyValue(asVal)),
			"$lookup (stage)",
		)
	}
	if strings.IndexByte(asField, 0) >= 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFieldPathNulByte,
			"FieldPath field names may not contain '\\0'.", "$lookup (stage)")
	}

	l := &lookup{
		cmp:     operators.CollationOf(stage),
		from:    from,
		as:      asField,
		fetcher: fetcher,
	}

	// Determine form: simple or pipeline.
	hasPipeline := specDoc.Has("pipeline")
	hasLocalField := specDoc.Has("localField")
	hasLet := specDoc.Has("let")

	if hasPipeline {
		// Pipeline form.
		pipelineVal, pErr := specDoc.Get("pipeline")
		if pErr != nil {
			return nil, lazyerrors.Error(pErr)
		}

		pipelineArr, ok := pipelineVal.(*types.Array)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("$lookup 'pipeline' must be an array, got %s", types.FormatAnyValue(pipelineVal)),
				"$lookup (stage)",
			)
		}
		if err := aggregations.CheckPipelineLength(pipelineArr.Len()); err != nil {
			return nil, err
		}

		if err := CheckParseFailingStages(pipelineArr); err != nil {
			return nil, err
		}
		if err := checkFirstOnlyStages(pipelineArr); err != nil {
			return nil, err
		}
		l.pipeline = pipelineArr

		if hasLet {
			letVal, lErr := specDoc.Get("let")
			if lErr != nil {
				return nil, lazyerrors.Error(lErr)
			}

			letDoc, ok := letVal.(*types.Document)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrFailedToParse,
					fmt.Sprintf("$lookup 'let' must be an object, got %s", types.FormatAnyValue(letVal)),
					"$lookup (stage)",
				)
			}

			if err := operators.CheckFieldPaths(letDoc); err != nil {
				return nil, err
			}
			for _, name := range letDoc.Keys() {
				if err := operators.CheckUserVariableName(name); err != nil {
					return nil, err
				}
			}
			l.letVars = letDoc
		}

		if err := l.checkSubPipeline(); err != nil {
			return nil, err
		}
	}

	// localField/foreignField select the foreign documents; with a pipeline
	// (the concise form) the pipeline then runs on those matches only.
	if hasLocalField {
		localFieldVal, lErr := specDoc.Get("localField")
		if lErr != nil {
			return nil, lazyerrors.Error(lErr)
		}

		localField, ok := localFieldVal.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("$lookup 'localField' must be a string, got %s", types.FormatAnyValue(localFieldVal)),
				"$lookup (stage)",
			)
		}

		foreignFieldVal, fErr := specDoc.Get("foreignField")
		if fErr != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				"$lookup 'foreignField' is required when 'localField' is specified",
				"$lookup (stage)",
			)
		}

		foreignField, ok := foreignFieldVal.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("$lookup 'foreignField' must be a string, got %s", types.FormatAnyValue(foreignFieldVal)),
				"$lookup (stage)",
			)
		}

		l.localField = localField
		l.foreignField = foreignField
	}

	if !hasPipeline && !hasLocalField {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"$lookup requires either 'localField'/'foreignField' or 'pipeline'",
			"$lookup (stage)",
		)
	}

	return l, nil
}

// equalityIndex groups foreign documents by the keys of their foreignField
// values, so an equality join considers only an input document's candidates
// instead of every foreign document. Any two values valuesEqual treats as equal
// share a key; documents with a value that has no key are always candidates.
type equalityIndex struct {
	docs    []*types.Document
	byKey   map[string][]int
	unkeyed []int
}

func newEqualityIndex(docs []*types.Document, foreignField string, cmp *collation.Comparator) *equalityIndex {
	idx := &equalityIndex{docs: docs, byKey: make(map[string][]int)}
	parts := splitLookupPath(foreignField)
	for pos, d := range docs {
		values, missing := lookupForeignPathValues(d, parts)
		var keys []string
		ok := !missing
		for _, v := range values {
			vk, vok := equalityKeys(collatedLookupValue(cmp, v))
			ok = ok && vok
			keys = append(keys, vk...)
		}
		if !ok {
			idx.unkeyed = append(idx.unkeyed, pos)
			continue
		}
		for _, key := range keys {
			idx.byKey[key] = append(idx.byKey[key], pos)
		}
	}
	return idx
}

// candidatesFor returns, in their original order, the foreign documents that
// can match one of the local candidates: all of them when one has no key.
func (idx *equalityIndex) candidatesFor(cands []any) []*types.Document {
	keys := make([]string, 0, len(cands))
	for _, c := range cands {
		if _, isArr := c.(*types.Array); isArr {
			return idx.docs
		}
		key, ok := equalityKey(c)
		if !ok {
			return idx.docs
		}
		keys = append(keys, key)
	}

	positions := append([]int(nil), idx.unkeyed...)
	for _, key := range keys {
		positions = append(positions, idx.byKey[key]...)
	}
	stdsort.Ints(positions)

	out := make([]*types.Document, 0, len(positions))
	for i, pos := range positions {
		if i > 0 && positions[i-1] == pos {
			continue
		}
		out = append(out, idx.docs[pos])
	}
	return out
}

// equalityKeys returns the keys of a join value, one per element of an array.
// It reports false when the value (or an element) has no key: anything but
// strings, booleans, and numbers, and integers too large to compare with a
// double exactly.
func equalityKeys(v any) ([]string, bool) {
	if _, ok := v.(indexedArray); ok {
		return nil, false
	}
	arr, isArr := v.(*types.Array)
	if !isArr {
		key, ok := equalityKey(v)
		return []string{key}, ok
	}

	keys := make([]string, 0, arr.Len())
	for i := 0; i < arr.Len(); i++ {
		key, ok := equalityKey(must.NotFail(arr.Get(i)))
		if !ok {
			return nil, false
		}
		keys = append(keys, key)
	}
	return keys, true
}

// maxExactJoinInt bounds the integers whose double conversion is exact, so an
// integer and a double that compare equal also share a key.
const maxExactJoinInt = 1 << 53

func equalityKey(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return "s" + v, true
	case bool:
		return "b" + strconv.FormatBool(v), true
	case int32:
		return "n" + strconv.FormatInt(int64(v), 10), true
	case int64:
		if v <= -maxExactJoinInt || v >= maxExactJoinInt {
			return "", false
		}
		return "n" + strconv.FormatInt(v, 10), true
	case float64:
		if math.IsNaN(v) {
			return "", false
		}
		if v == math.Trunc(v) && v > -maxExactJoinInt && v < maxExactJoinInt {
			return "n" + strconv.FormatInt(int64(v), 10), true
		}
		return "f" + strconv.FormatFloat(v, 'g', -1, 64), true
	}
	return "", false
}

// equalityMatches returns the foreign documents whose foreignField matches
// doc's localField under MongoDB's equality join semantics.
func (l *lookup) equalityMatches(doc *types.Document, idx *equalityIndex) []*types.Document {
	candidates := lookupLocalCandidates(doc, l.localField)
	for i, c := range candidates {
		candidates[i] = collatedLookupValue(l.cmp, c)
	}

	matched := make([]*types.Document, 0)
	for _, fromDoc := range idx.candidatesFor(candidates) {
		if foreignMatches(fromDoc, l.foreignField, candidates, l.cmp) {
			matched = append(matched, fromDoc)
		}
	}
	return matched
}

// lookupLocalCandidates returns the values mongod compares with foreignField
// for doc's localField: every value along the path, with arrays on the way
// and at its end expanded one level. With none, the candidate is null.
func lookupLocalCandidates(doc *types.Document, field string) []any {
	var out []any
	var walk func(v any, parts []string)
	walk = func(v any, parts []string) {
		if len(parts) == 0 {
			if arr, ok := v.(*types.Array); ok {
				for i := range arr.Len() {
					out = append(out, must.NotFail(arr.Get(i)))
				}
				return
			}
			out = append(out, v)
			return
		}
		switch v := v.(type) {
		case *types.Document:
			if next, err := v.Get(parts[0]); err == nil {
				walk(next, parts[1:])
			}
		case *types.Array:
			if elem, ok := arrayIndexElement(v, parts[0]); ok {
				walk(elem, parts[1:])
				return
			}
			for i := range v.Len() {
				if elem, ok := must.NotFail(v.Get(i)).(*types.Document); ok {
					walk(elem, parts)
				}
			}
		}
	}
	walk(doc, splitLookupPath(field))
	if len(out) == 0 {
		return []any{nil}
	}
	return out
}

// splitLookupPath splits a dotted path, without allocating for a dotless one
// beyond its one-element slice.
func splitLookupPath(field string) []string {
	if !strings.Contains(field, ".") {
		return []string{field}
	}
	return strings.Split(field, ".")
}

// arrayIndexElement returns arr's element at the index a path component
// names, when the component is all digits without a leading zero and the
// index is in range.
func arrayIndexElement(arr *types.Array, part string) (any, bool) {
	if part == "" || len(part) > 9 || (len(part) > 1 && part[0] == '0') {
		return nil, false
	}
	n := 0
	for i := range len(part) {
		c := part[i]
		if c < '0' || c > '9' {
			return nil, false
		}
		n = n*10 + int(c-'0')
	}
	if n >= arr.Len() {
		return nil, false
	}
	return must.NotFail(arr.Get(n)), true
}

// lookupForeignValues returns the values a query on field reaches in doc,
// traversing arrays along the path, and whether some branch lacks the field.
func lookupForeignValues(doc *types.Document, field string) ([]any, bool) {
	return lookupForeignPathValues(doc, splitLookupPath(field))
}

func lookupForeignPathValues(doc *types.Document, parts []string) ([]any, bool) {
	if len(parts) == 1 {
		v, err := doc.Get(parts[0])
		if err != nil {
			return nil, true
		}
		return []any{v}, false
	}
	var out []any
	missing := false
	var walk func(v any, parts []string)
	walk = func(v any, parts []string) {
		if len(parts) == 0 {
			out = append(out, v)
			return
		}
		switch v := v.(type) {
		case *types.Document:
			next, err := v.Get(parts[0])
			if err != nil {
				missing = true
				return
			}
			walk(next, parts[1:])
		case *types.Array:
			if elem, ok := arrayIndexElement(v, parts[0]); ok {
				switch elem := elem.(type) {
				case *types.Array:
					if len(parts) == 1 {
						out = append(out, indexedArray{elem})
					} else {
						walk(elem, parts[1:])
					}
				case *types.Document:
					walk(elem, parts[1:])
				default:
					// A scalar at an index ends the path without making it missing.
					if len(parts) == 1 {
						out = append(out, elem)
					}
				}
			}
			for i := range v.Len() {
				if elem, ok := must.NotFail(v.Get(i)).(*types.Document); ok {
					walk(elem, parts)
				}
			}
		default:
			missing = true
		}
	}
	walk(doc, parts)
	return out, missing
}

// collatedLookupValue returns v with its strings as sort keys under cmp, so
// values equal under the collation are equal as BSON.
func collatedLookupValue(cmp *collation.Comparator, v any) any {
	if cmp == nil {
		return v
	}
	if indexed, ok := v.(indexedArray); ok {
		return indexedArray{arr: cmp.KeyValue(indexed.arr).(*types.Array)}
	}
	return cmp.KeyValue(v)
}

// foreignMatches reports whether doc matches {field: {$in: candidates}}.
func foreignMatches(doc *types.Document, field string, candidates []any, cmp *collation.Comparator) bool {
	values, missing := lookupForeignValues(doc, field)
	for i, v := range values {
		values[i] = collatedLookupValue(cmp, v)
	}
	for _, c := range candidates {
		if isNullOrMissing(c) && missing {
			return true
		}
		for _, v := range values {
			if foreignMatchesCandidate(v, c) {
				return true
			}
		}
	}
	return false
}

func (l *lookup) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	for _, s := range l.before {
		var err error
		if iter, err = s.Process(ctx, iter, closer); err != nil {
			return nil, err
		}
	}
	docs, err := common.ConsumeDocuments(iter, "$lookup")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	fetcher := withFetchCache(l.fetcher)
	pipelineLen := 0
	if l.pipeline != nil {
		pipelineLen = l.pipeline.Len()
	}
	ctx = WithJoinStages(ctx, JoinStages{N: pipelineLen + 1, ViewLimit: l.localField == ""})

	// With an index on the joined field, ask the backend for just the matching
	// documents; without one, load the collection once and join in memory.
	var fetchFilter *types.Document
	if l.localField != "" && l.cmp == nil && fetcher.IndexedField(ctx, l.from, l.foreignField) {
		fetchFilter = equalityFetchFilter(docs, l.localField, l.foreignField)
	}
	perDocField := ""
	if l.pipeline != nil && l.localField == "" && l.cmp == nil {
		if field := leadingMatchEqualityField(l.pipeline); field != "" && fetcher.IndexedField(ctx, l.from, field) {
			perDocField = field
		}
	}

	var fromDocs []*types.Document
	if perDocField == "" && l.from != "" {
		if fromDocs, err = fetcher.Fetch(ctx, l.from, fetchFilter); err != nil {
			return nil, lazyerrors.Error(err)
		}
	}

	var eqIdx *equalityIndex
	switch {
	case l.localField == "" || l.from == "":
	case fetchFilter == nil:
		// fromDocs is the cached whole collection; share its index with
		// other lookups in this stage, such as nested ones.
		if eqIdx, err = fetcher.unfilteredEqualityIndex(ctx, l.from, l.foreignField, l.cmp); err != nil {
			return nil, lazyerrors.Error(err)
		}
	default:
		eqIdx = newEqualityIndex(fromDocs, l.foreignField, l.cmp)
	}

	res := &lookupIterator{
		ctx:              ctx,
		l:                l,
		fetcher:          fetcher,
		docs:             docs,
		fromDocs:         fromDocs,
		eqIdx:            eqIdx,
		perDocField:      perDocField,
		validatedMatches: map[int]bool{},
	}
	closer.Add(res)

	return res, nil
}

// lookupMaxMatchBytes is mongod's internalLookupStageIntermediateDocumentMaxSizeBytes:
// the most one input document's matches may total.
const lookupMaxMatchBytes = 100 * 1024 * 1024

// lookupArrayElementOverhead is what mongod's array accumulator adds to each
// matched document's BSON size when it reports an equality $lookup's size.
const lookupArrayElementOverhead = 9

// lookupIterator joins one input document per Next, so the stage holds only
// its inputs and the document being joined.
type lookupIterator struct {
	ctx              context.Context
	l                *lookup
	fetcher          *cachingFetcher
	docs             []*types.Document
	fromDocs         []*types.Document
	eqIdx            *equalityIndex
	perDocField      string
	validatedMatches map[int]bool
	next             int
	pending          *unwoundMatches
}

func (it *lookupIterator) Next() (struct{}, *types.Document, error) {
	var unused struct{}
	for {
		if it.pending != nil {
			doc, err := it.nextUnwound()
			if doc != nil || err != nil {
				return unused, doc, err
			}
			continue
		}

		if it.next >= len(it.docs) {
			return unused, nil, iterator.ErrIteratorDone
		}
		doc := it.docs[it.next]
		it.docs[it.next] = nil
		it.next++

		if it.l.unwind != nil {
			if err := it.startUnwound(doc); err != nil {
				return unused, nil, err
			}
			continue
		}

		matched, err := it.matches(doc)
		if err != nil {
			return unused, nil, err
		}

		newDoc := doc.DeepCopy()
		arr := types.MakeArray(len(matched))
		for _, m := range matched {
			arr.Append(m)
		}
		setNestedField(newDoc, it.l.as, arr)
		return unused, newDoc, nil
	}
}

// setNestedField sets the dotted path field of doc to v, replacing any
// non-document value on the way, as mongod's $lookup does for its as field.
func setNestedField(doc *types.Document, field string, v any) {
	parts := strings.Split(field, ".")
	for _, part := range parts[:len(parts)-1] {
		cur, err := doc.Get(part)
		next, ok := cur.(*types.Document)
		if err != nil || !ok {
			next = types.MakeDocument(1)
			doc.Set(part, next)
		}
		doc = next
	}
	doc.Set(parts[len(parts)-1], v)
}

// startUnwound begins emitting doc's matches one at a time, as mongod does
// for a $lookup that absorbs an $unwind of its as field.
func (it *lookupIterator) startUnwound(doc *types.Document) error {
	l := it.l
	if l.pipeline == nil {
		matched := l.equalityMatches(doc, it.eqIdx)
		it.pending = &unwoundMatches{doc: doc, iter: iterator.Values(iterator.ForSlice(matched)), copyMatches: true}
		return nil
	}
	candidates, err := it.candidates(doc)
	if err != nil {
		return err
	}
	matches, closer, err := l.pipelineIter(it.ctx, it.fetcher, candidates, doc, it.validatedMatches)
	if err != nil {
		return err
	}
	it.pending = &unwoundMatches{doc: doc, iter: matches, closer: closer}
	return nil
}

// nextUnwound returns the next document for the pending input document, or
// nil when it has no more.
func (it *lookupIterator) nextUnwound() (*types.Document, error) {
	p, u := it.pending, it.l.unwind
	_, m, err := p.iter.Next()
	if errors.Is(err, iterator.ErrIteratorDone) {
		p.close()
		it.pending = nil
		if p.n > 0 {
			return nil, nil
		}
		// No matches: the $unwind sees an empty array.
		if !u.preserveNullAndEmptyArrays {
			return nil, nil
		}
		out := p.doc.DeepCopy()
		setNestedField(out, it.l.as, types.MakeArray(0))
		return u.preserved(out, true), nil
	}
	if err != nil {
		p.close()
		it.pending = nil
		return nil, err
	}
	if p.copyMatches {
		m = m.DeepCopy()
	}
	out := p.doc.DeepCopy()
	setNestedField(out, it.l.as, m)
	u.setIndex(out, int64(p.n))
	p.n++
	return out, nil
}

// unwoundMatches is an input document whose matches are being emitted.
type unwoundMatches struct {
	doc         *types.Document
	iter        types.DocumentsIterator
	closer      *iterator.MultiCloser
	copyMatches bool
	n           int
}

func (p *unwoundMatches) close() {
	p.iter.Close()
	if p.closer != nil {
		p.closer.Close()
	}
}

func (it *lookupIterator) Close() {
	if it.pending != nil {
		it.pending.close()
		it.pending = nil
	}
	it.docs = nil
}

// matches returns the foreign documents joined to doc.
func (it *lookupIterator) matches(doc *types.Document) ([]*types.Document, error) {
	l := it.l
	if l.pipeline == nil {
		matched := l.equalityMatches(doc, it.eqIdx)
		total := 0
		for i, m := range matched {
			size := m.BSONSize()
			switch {
			case l.sbe && total+size+lookupArrayElementOverhead > lookupMaxMatchBytes:
				// SBE refuses a match that would take the matches past the limit.
				size += lookupArrayElementOverhead
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrExceededMemoryLimit,
					fmt.Sprintf("Used too much memory for a single array. Memory limit: %d bytes. "+
						"The array contains %d elements and is of size %d bytes. The element being added has size %d bytes.",
						lookupMaxMatchBytes, i, total, size),
					"$lookup",
				)
			case !l.sbe && total > lookupMaxMatchBytes:
				// The classic engine refuses one once the matches so far pass it.
				return nil, l.matchesTooLarge()
			}
			if l.sbe {
				size += lookupArrayElementOverhead
			}
			total += size
			matched[i] = m.DeepCopy()
		}
		return matched, nil
	}

	candidates, err := it.candidates(doc)
	if err != nil {
		return nil, err
	}
	return l.runPipeline(it.ctx, it.fetcher, candidates, doc, it.validatedMatches)
}

// candidates returns the foreign documents a pipeline-form $lookup runs its
// sub-pipeline on for doc.
func (it *lookupIterator) candidates(doc *types.Document) ([]*types.Document, error) {
	l := it.l
	if it.eqIdx != nil {
		return l.equalityMatches(doc, it.eqIdx), nil
	}
	if it.perDocField != "" {
		candidates, err := l.perDocCandidates(it.ctx, it.fetcher, doc, it.perDocField)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
		return candidates, nil
	}
	return it.fromDocs, nil
}

// MarkLookupsUnwound has each $lookup in pipeline perform a following $unwind
// of its as field, as mongod absorbs that $unwind and streams the matches. A
// $match in between that does not read as does not prevent it, as mongod moves
// such a $match ahead of the $lookup.
func MarkLookupsUnwound(pipeline []aggregations.Stage, specs []*types.Document) {
	for i := 0; i < len(pipeline) && i < len(specs); i++ {
		l, ok := pipeline[i].(*lookup)
		if !ok || l.unwind != nil {
			continue
		}
		var crossed []int
		for j := i + 1; j < len(pipeline) && j < len(specs); j++ {
			if movesBeforeLookup(specs[j], l.as) {
				crossed = append(crossed, j)
				continue
			}
			if u, ok := pipeline[j].(*unwind); ok && unwindPath(specs[j]) == "$"+l.as {
				l.unwind = u
				u.absorbed = true
				for _, k := range crossed {
					l.before = append(l.before, pipeline[k])
					pipeline[k] = passThroughStage{}
				}
			}
			break
		}
	}
}

// movesBeforeLookup reports whether mongod moves the stage spec ahead of a
// $lookup into field: a $limit or $skip, and a $match or $sort that does
// not read field.
func movesBeforeLookup(spec *types.Document, field string) bool {
	switch spec.Command() {
	case "$limit", "$skip":
		return true
	case "$match":
		return !matchReadsField(spec, field)
	case "$sort":
		keys, ok := must.NotFail(spec.Get("$sort")).(*types.Document)
		if !ok {
			return false
		}
		for _, k := range keys.Keys() {
			if k == field || strings.HasPrefix(k, field+".") || strings.HasPrefix(field, k+".") {
				return false
			}
			if _, isMeta := must.NotFail(keys.Get(k)).(*types.Document); isMeta {
				return false
			}
		}
		return true
	}
	return false
}

// passThroughStage replaces a stage another stage has taken over.
type passThroughStage struct{}

func (passThroughStage) Process(_ context.Context, iter types.DocumentsIterator, _ *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	return iter, nil
}

// matchReadsField reports whether a $match stage may read field. It answers
// true for any filter it cannot analyze.
func matchReadsField(spec *types.Document, field string) bool {
	filter, ok := must.NotFail(spec.Get("$match")).(*types.Document)
	return !ok || filterReadsField(filter, field)
}

func filterReadsField(filter *types.Document, field string) bool {
	for _, key := range filter.Keys() {
		switch key {
		case "$and", "$or", "$nor":
			arr, ok := must.NotFail(filter.Get(key)).(*types.Array)
			if !ok {
				return true
			}
			for i := range arr.Len() {
				sub, ok := must.NotFail(arr.Get(i)).(*types.Document)
				if !ok || filterReadsField(sub, field) {
					return true
				}
			}
		case "$comment":
		case "$expr":
			if exprReadsField(must.NotFail(filter.Get(key)), field) {
				return true
			}
		default:
			if strings.HasPrefix(key, "$") || key == field ||
				strings.HasPrefix(key, field+".") || strings.HasPrefix(field, key+".") {
				return true
			}
		}
	}
	return false
}

// exprReadsField reports whether an expression may read field: through a
// path on it, or through $$ROOT or $$CURRENT.
func exprReadsField(expr any, field string) bool {
	switch v := expr.(type) {
	case string:
		switch {
		case strings.HasPrefix(v, "$$"):
			name, _, _ := strings.Cut(v[2:], ".")
			return name == "ROOT" || name == "CURRENT"
		case strings.HasPrefix(v, "$"):
			p := v[1:]
			return p == field || strings.HasPrefix(p, field+".") || strings.HasPrefix(field, p+".")
		}
	case *types.Array:
		for i := range v.Len() {
			if exprReadsField(must.NotFail(v.Get(i)), field) {
				return true
			}
		}
	case *types.Document:
		switch v.Command() {
		case "$literal":
			return false
		case "$getField":
			// Without input it reads $$CURRENT.
			spec, ok := must.NotFail(v.Get("$getField")).(*types.Document)
			if !ok || !spec.Has("input") {
				return true
			}
		case "$let":
			// Rebinding CURRENT changes what every path reads.
			if spec, ok := must.NotFail(v.Get("$let")).(*types.Document); ok {
				if vars, _ := spec.Get("vars"); vars != nil {
					if vars, ok := vars.(*types.Document); ok && vars.Has("CURRENT") {
						return true
					}
				}
			}
		}
		for _, k := range v.Keys() {
			if exprReadsField(must.NotFail(v.Get(k)), field) {
				return true
			}
		}
	}
	return false
}

// unwindPath returns the field path of an $unwind stage spec, or "".
func unwindPath(spec *types.Document) string {
	if spec == nil || spec.Command() != "$unwind" {
		return ""
	}
	v, _ := spec.Get("$unwind")
	switch v := v.(type) {
	case string:
		return v
	case *types.Document:
		p, _ := v.Get("path")
		s, _ := p.(string)
		return s
	}
	return ""
}

// letBindings evaluates the let variables against localDoc.
func (l *lookup) letBindings(localDoc *types.Document) (map[string]any, error) {
	vars := map[string]any{}
	if l.letVars == nil {
		return vars, nil
	}

	letIter := l.letVars.Iterator()
	defer letIter.Close()

	for {
		k, v, err := letIter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return vars, nil
		}
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
		if vars[k], err = evaluateLetExpr(v, localDoc); err != nil {
			return nil, err
		}
	}
}

// perDocCandidates fetches, for one input document, the foreign documents the
// sub-pipeline's leading equality on field can match, falling back to the whole
// collection when the compared value cannot be pushed down.
func (l *lookup) perDocCandidates(ctx context.Context, fetcher CollectionFetcher, doc *types.Document, field string) ([]*types.Document, error) {
	vars, err := l.letBindings(doc)
	if err != nil {
		return nil, err
	}
	first := must.NotFail(l.pipeline.Get(0)).(*types.Document)
	value, ok := leadingMatchEqualityValue(substituteVarsInStage(first, vars), field)
	if !ok {
		return fetcher.Fetch(ctx, l.from, nil)
	}
	return fetcher.Fetch(ctx, l.from, must.NotFail(types.NewDocument(field, value)))
}

// runPipeline runs the sub-pipeline for localDoc and returns its output.
func (l *lookup) runPipeline(ctx context.Context, fetcher CollectionFetcher, fromDocs []*types.Document, localDoc *types.Document, validatedMatches map[int]bool) ([]*types.Document, error) {
	iter, closer, err := l.pipelineIter(ctx, fetcher, fromDocs, localDoc, validatedMatches)
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	return l.collectMatches(iter)
}

// pipelineIter starts the sub-pipeline for localDoc. Its stages are chained,
// so the sub-pipeline streams as it does in mongod. validatedMatches, when not
// nil, records the $match stages already validated for an earlier document;
// let values cannot change a stage's shape, so those are not validated again.
// The caller closes the returned closer.
func (l *lookup) pipelineIter(ctx context.Context, fetcher CollectionFetcher, fromDocs []*types.Document, localDoc *types.Document, validatedMatches map[int]bool) (types.DocumentsIterator, *iterator.MultiCloser, error) { //nolint:lll // for readability
	vars, err := l.letBindings(localDoc)
	if err != nil {
		return nil, nil, err
	}

	// fromDocs may be shared: run leading $match stages on them directly (they
	// do not modify documents) and copy only the survivors.
	var current types.DocumentsIterator = iterator.Values(iterator.ForSlice(fromDocs))
	copied := false

	closer := iterator.NewMultiCloser()
	closer.Add(current)
	fail := func(err error) (types.DocumentsIterator, *iterator.MultiCloser, error) {
		closer.Close()
		return nil, nil, err
	}

	subStages := must.NotFail(iterator.ConsumeValues(l.pipeline.Iterator()))
	built := make([]aggregations.Stage, len(subStages))
	specs := make([]*types.Document, len(subStages))
	for stageIndex, stageVal := range subStages {
		stageDoc, ok := stageVal.(*types.Document)
		if !ok {
			return fail(handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"Each element of the '$lookup' pipeline must be an object",
				"$lookup (stage)",
			))
		}

		substituted := substituteVarsInStage(stageDoc, vars)
		operators.BindCollation(substituted, l.cmp)
		specs[stageIndex] = substituted

		if stageIndex == 0 && l.from == "" {
			docs, err := EvalDocuments(substituted)
			if err != nil {
				return fail(err)
			}
			current = iterator.Values(iterator.ForSlice(docs))
			closer.Add(current)
			copied = true
			continue
		}

		storedInput := SeesStoredDocuments(subStages, stageIndex)
		if validatedMatches[stageIndex] {
			built[stageIndex] = newValidatedMatchStage(substituted, storedInput)
			continue
		}
		if built[stageIndex], err = newSubPipelineStage(substituted, fetcher, storedInput); err != nil {
			return fail(err)
		}
		if validatedMatches != nil && substituted.Command() == "$match" {
			validatedMatches[stageIndex] = true
		}
	}
	MarkLookupsUnwound(built, specs)
	if len(specs) > 0 && specs[0].Command() == "$collStats" && l.from != "" {
		sf, ok := fetcher.(StatsFetcher)
		if !ok {
			return fail(handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrNotImplemented,
				"$collStats is not supported here", "$collStats (stage)"))
		}
		stats, err := sf.CollStats(ctx, l.from, built, closer)
		if err != nil {
			return fail(err)
		}
		return stats, closer, nil
	}
	classic := l.inClassic || !l.sbeSubPipeline
	for _, st := range built {
		switch st := st.(type) {
		case *lookup:
			st.inClassic = st.inClassic || classic
		case *unionWith:
			st.sub.inClassic = st.sub.inClassic || classic
		}
	}
	if l.sbeSubPipeline && !l.inClassic {
		MarkSBELookups(built, specs, l.viewIsView)
	}

	for stageIndex, stage := range built {
		if stage == nil {
			continue
		}
		if stageIndex < l.sbePrefix {
			operators.MarkSBE(specs[stageIndex])
			stage = &sbeStage{inner: stage}
		}
		if !copied && specs[stageIndex].Command() != "$match" {
			current = newDeepCopyIterator(current)
			closer.Add(current)
			copied = true
		}
		if current, err = stage.Process(ctx, current, closer); err != nil {
			return fail(err)
		}
		if specs[stageIndex].Command() != "$match" {
			// The copies keep their stored documents' RecordIds, which the
			// sub-pipeline sees until a stage other than $match reshapes them.
			current = newMapIterator(current, closer, func(d *types.Document) (*types.Document, error) {
				d.SetRecordKey(nil)
				return d, nil
			})
		}
	}

	if !copied {
		current = newDeepCopyIterator(current)
		closer.Add(current)
	}
	return current, closer, nil
}

// collectMatches drains a sub-pipeline's output. mongod refuses a further
// match once the matches so far total more than lookupMaxMatchBytes.
func (l *lookup) collectMatches(iter types.DocumentsIterator) ([]*types.Document, error) {
	var matched []*types.Document
	total := 0
	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return matched, nil
		}
		if err != nil {
			return nil, err
		}
		if total > lookupMaxMatchBytes {
			return nil, l.matchesTooLarge()
		}
		total += doc.BSONSize()
		matched = append(matched, doc)
	}
}

func (l *lookup) matchesTooLarge() error {
	return handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrLookupMatchesTooLarge,
		fmt.Sprintf("Total size of documents in %s matching pipeline's $lookup stage exceeds %d bytes",
			l.from, lookupMaxMatchBytes),
		"$lookup",
	)
}

// deepCopyIterator yields a deep copy of each document of iter.
type deepCopyIterator struct {
	iter types.DocumentsIterator
}

func newDeepCopyIterator(iter types.DocumentsIterator) *deepCopyIterator {
	return &deepCopyIterator{iter: iter}
}

func (it *deepCopyIterator) Next() (struct{}, *types.Document, error) {
	_, doc, err := it.iter.Next()
	if err != nil {
		return struct{}{}, nil, err
	}
	copied := doc.DeepCopy()
	copied.SetRecordKey(doc.RecordKey())
	return struct{}{}, copied, nil
}

func (it *deepCopyIterator) Close() {
	it.iter.Close()
}

// LookupJoinField returns the field of a $lookup's foreign collection whose
// index the stage probes for each joined document: the foreignField, or the
// field of a leading $match equality in its pipeline. It is empty when the
// stage reads the whole foreign collection.
func LookupJoinField(spec *types.Document) string {
	if v, _ := spec.Get("localField"); v != nil {
		foreign, _ := spec.Get("foreignField")
		field, _ := foreign.(string)
		return field
	}
	v, _ := spec.Get("pipeline")
	pipeline, _ := v.(*types.Array)
	return leadingMatchEqualityField(pipeline)
}

// leadingMatchEqualityField returns the foreign field compared for equality by
// a sub-pipeline that starts with {$match: {$expr: {$eq: ["$field", x]}}} (in
// either operand order), or "" when the pipeline does not start that way.
func leadingMatchEqualityField(pipeline *types.Array) string {
	field, _, ok := leadingExprEquality(pipeline)
	if !ok {
		return ""
	}
	return field
}

// leadingMatchEqualityValue returns the value compared with field by the
// already-substituted leading $match stage, if it is a scalar that an
// equality filter on field matches the same way.
func leadingMatchEqualityValue(substitutedMatch *types.Document, field string) (any, bool) {
	gotField, value, ok := leadingExprEquality(must.NotFail(types.NewArray(substitutedMatch)))
	if !ok || gotField != field {
		return nil, false
	}
	literal, isLiteral := value.(*types.Document)
	if isLiteral = isLiteral && literal.Len() == 1 && literal.Command() == "$literal"; isLiteral {
		value = must.NotFail(literal.Get("$literal"))
	}
	switch v := value.(type) {
	case string:
		if strings.HasPrefix(v, "$") && !isLiteral {
			return nil, false
		}
	case int32, int64, bool, types.ObjectID, time.Time:
	case float64:
		if math.IsNaN(v) {
			return nil, false
		}
	default:
		return nil, false
	}
	return value, true
}

func leadingExprEquality(pipeline *types.Array) (field string, other any, ok bool) {
	if pipeline == nil || pipeline.Len() == 0 {
		return "", nil, false
	}
	stage, _ := must.NotFail(pipeline.Get(0)).(*types.Document)
	if stage == nil || stage.Len() != 1 || stage.Command() != "$match" {
		return "", nil, false
	}
	match, _ := must.NotFail(stage.Get("$match")).(*types.Document)
	if match == nil || match.Len() != 1 || match.Command() != "$expr" {
		return "", nil, false
	}
	expr, _ := must.NotFail(match.Get("$expr")).(*types.Document)
	if expr == nil || expr.Len() != 1 || expr.Command() != "$eq" {
		return "", nil, false
	}
	operands, _ := must.NotFail(expr.Get("$eq")).(*types.Array)
	if operands == nil || operands.Len() != 2 {
		return "", nil, false
	}

	isFieldPath := func(v any) (string, bool) {
		s, isStr := v.(string)
		if !isStr || !strings.HasPrefix(s, "$") || strings.HasPrefix(s, "$$") || len(s) < 2 {
			return "", false
		}
		return s[1:], true
	}
	a, b := must.NotFail(operands.Get(0)), must.NotFail(operands.Get(1))
	if f, isPath := isFieldPath(a); isPath {
		if _, bothPaths := isFieldPath(b); !bothPaths {
			return f, b, true
		}
	}
	if f, isPath := isFieldPath(b); isPath {
		if _, bothPaths := isFieldPath(a); !bothPaths {
			return f, a, true
		}
	}
	return "", nil, false
}

// evaluateLetExpr evaluates a $lookup let value, any expression, against
// localDoc; a missing result binds null.
func evaluateLetExpr(expr any, localDoc *types.Document) (any, error) {
	v, missing, err := operators.EvalOperand(expr, localDoc)
	if err != nil {
		return nil, err
	}
	if missing {
		return types.Null, nil
	}
	return v, nil
}

// substituteVarsInStage substitutes let variables into a sub-pipeline stage,
// leaving alone the names a nested $lookup's let defines again. A $match query
// only reads variables inside $expr.
func substituteVarsInStage(stage *types.Document, vars map[string]any) *types.Document {
	if len(vars) == 0 {
		return stage
	}
	if stage.Len() != 1 {
		return substituteVarsInDoc(stage, vars, false)
	}
	name := stage.Command()
	spec := must.NotFail(stage.Get(name))
	switch name {
	case "$match":
		if query, ok := spec.(*types.Document); ok {
			spec = substituteVarsInQuery(query, vars)
		}
	case "$lookup", "$unionWith":
		if d, ok := spec.(*types.Document); ok {
			spec = substituteVarsInSubPipelineSpec(d, vars)
		} else {
			spec = substituteVarsInValue(spec, vars, false)
		}
	case "$facet":
		if d, ok := spec.(*types.Document); ok {
			facets := types.MakeDocument(d.Len())
			for _, k := range d.Keys() {
				v := must.NotFail(d.Get(k))
				if p, ok := v.(*types.Array); ok {
					v = substituteVarsInPipeline(p, vars)
				}
				facets.Set(k, v)
			}
			spec = facets
		}
	default:
		spec = substituteVarsInValue(spec, vars, false)
	}
	return must.NotFail(types.NewDocument(name, spec))
}

// substituteVarsInSubPipelineSpec substitutes into a nested $lookup or
// $unionWith: its let values see the outer variables, its pipeline sees those
// its let does not define again.
func substituteVarsInSubPipelineSpec(spec *types.Document, vars map[string]any) *types.Document {
	inner := vars
	if let, ok := getOrNil(spec, "let").(*types.Document); ok {
		inner = withoutVars(vars, let.Keys()...)
	}
	out := types.MakeDocument(spec.Len())
	for _, k := range spec.Keys() {
		v := must.NotFail(spec.Get(k))
		switch k {
		case "let":
			v = substituteVarsInValue(v, vars, false)
		case "pipeline":
			if p, ok := v.(*types.Array); ok {
				v = substituteVarsInPipeline(p, inner)
			}
		}
		out.Set(k, v)
	}
	return out
}

func substituteVarsInPipeline(pipeline *types.Array, vars map[string]any) *types.Array {
	out := types.MakeArray(pipeline.Len())
	for i := range pipeline.Len() {
		v := must.NotFail(pipeline.Get(i))
		if stage, ok := v.(*types.Document); ok {
			v = substituteVarsInStage(stage, vars)
		}
		out.Append(v)
	}
	return out
}

func getOrNil(doc *types.Document, key string) any {
	v, _ := doc.Get(key)
	return v
}

// withoutVars returns vars less names, which an inner scope defines again.
func withoutVars(vars map[string]any, names ...string) map[string]any {
	shadowed := false
	for _, n := range names {
		if _, ok := vars[n]; ok {
			shadowed = true
		}
	}
	if !shadowed {
		return vars
	}
	out := make(map[string]any, len(vars))
	for k, v := range vars {
		out[k] = v
	}
	for _, n := range names {
		delete(out, n)
	}
	return out
}

func substituteVarsInQuery(query *types.Document, vars map[string]any) *types.Document {
	result := types.MakeDocument(query.Len())
	for _, k := range query.Keys() {
		v := must.NotFail(query.Get(k))
		switch k {
		case "$expr":
			v = substituteVarsInValue(v, vars, true)
		case "$and", "$or", "$nor":
			if arr, ok := v.(*types.Array); ok {
				clauses := types.MakeArray(arr.Len())
				for i := range arr.Len() {
					clause := must.NotFail(arr.Get(i))
					if d, isDoc := clause.(*types.Document); isDoc {
						clause = substituteVarsInQuery(d, vars)
					}
					clauses.Append(clause)
				}
				v = clauses
			}
		}
		result.Set(k, v)
	}
	return result
}

// substituteVarsInDoc recursively replaces $$varName references in a document
// with the corresponding values from vars, as literalOperand so that values
// such as "$b" are not evaluated again. $literal is left alone, and the
// operators that bind variables hide the names they define from their scope.
func substituteVarsInDoc(doc *types.Document, vars map[string]any, inExpr bool) *types.Document {
	if len(vars) == 0 {
		return doc
	}

	if doc.Len() == 1 {
		switch doc.Command() {
		case "$literal", "$const":
			return doc
		case "$let", "$map", "$filter", "$reduce":
			if spec, ok := must.NotFail(doc.Get(doc.Command())).(*types.Document); ok {
				return must.NotFail(types.NewDocument(doc.Command(), substituteVarsInBinding(doc.Command(), spec, vars)))
			}
		}
	}

	result := types.MakeDocument(doc.Len())
	for _, k := range doc.Keys() {
		result.Set(k, substituteVarsInValue(must.NotFail(doc.Get(k)), vars, inExpr))
	}

	return result
}

// substituteVarsInBinding substitutes into the specification of an operator
// that binds variables: its bound expression does not see the outer value of
// a name it defines.
func substituteVarsInBinding(op string, spec *types.Document, vars map[string]any) *types.Document {
	var bound []string
	scopedKey := "in"
	switch op {
	case "$let":
		if names, ok := getOrNil(spec, "vars").(*types.Document); ok {
			bound = names.Keys()
		}
	case "$map", "$filter":
		bound = []string{"this"}
		if as, ok := getOrNil(spec, "as").(string); ok {
			bound = []string{as}
		}
		if op == "$filter" {
			scopedKey = "cond"
		}
	case "$reduce":
		bound = []string{"this", "value"}
	}
	inner := withoutVars(vars, bound...)

	out := types.MakeDocument(spec.Len())
	for _, k := range spec.Keys() {
		v := must.NotFail(spec.Get(k))
		if k == scopedKey {
			v = substituteVarsInValue(v, inner, true)
		} else {
			v = substituteVarsInValue(v, vars, true)
		}
		out.Set(k, v)
	}
	return out
}

// unboundLetVariable stands in for a let variable while the sub-pipeline is
// parsed: references become $$CURRENT, a variable reference too, so a place
// that needs a constant rejects it as mongod rejects the variable.
type unboundLetVariable struct{}

// checkSubPipeline parses the sub-pipeline once, before any document is read,
// so its errors surface as in mongod.
func (l *lookup) checkSubPipeline() error {
	vars := map[string]any{}
	if l.letVars != nil {
		for _, name := range l.letVars.Keys() {
			vars[name] = unboundLetVariable{}
		}
	}
	for i := range l.pipeline.Len() {
		stageDoc, ok := must.NotFail(l.pipeline.Get(i)).(*types.Document)
		if !ok {
			return nil
		}
		substituted := substituteVarsInStage(stageDoc, vars)
		if i == 0 && l.from == "" {
			if substituted.Command() == "$documents" {
				if err := operators.CheckFieldPaths(must.NotFail(substituted.Get("$documents"))); err != nil {
					return err
				}
			}
			continue
		}
		switch substituted.Command() {
		case "$out", "$merge":
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrLookupSubPipelineWrites,
				substituted.Command()+" is not allowed within a $lookup's sub-pipeline", "$lookup (stage)")
		}
		if _, err := newSubPipelineStage(substituted, l.fetcher, false); err != nil {
			return err
		}
	}
	return nil
}

// literalOperand returns an operand that evaluates to v. Inside $expr a bare
// scalar other than a "$" string means itself, and skipping the $literal
// saves an evaluation per document; elsewhere, such as in $project where 0
// and true are flags, every value is wrapped.
func literalOperand(v any, inExpr bool) any {
	if inExpr {
		switch v := v.(type) {
		case string:
			if !strings.HasPrefix(v, "$") {
				return v
			}
		case *types.Document, *types.Array:
		default:
			return v
		}
	}
	return must.NotFail(types.NewDocument("$literal", v))
}

// substituteVarsInValue recursively substitutes $$varName references in a value.
func substituteVarsInValue(val any, vars map[string]any, inExpr bool) any {
	switch v := val.(type) {
	case string:
		if strings.HasPrefix(v, "$$") {
			varName, _, dotted := strings.Cut(strings.TrimPrefix(v, "$$"), ".")
			if resolved, ok := vars[varName]; ok {
				if _, unbound := resolved.(unboundLetVariable); unbound {
					return "$$CURRENT" + strings.TrimPrefix(v, "$$"+varName)
				}
				if dotted {
					return must.NotFail(types.NewDocument("$let", must.NotFail(types.NewDocument(
						"vars", must.NotFail(types.NewDocument(varName, must.NotFail(types.NewDocument("$literal", resolved)))),
						"in", v,
					))))
				}
				return literalOperand(resolved, inExpr)
			}
		}

		return v

	case *types.Document:
		return substituteVarsInDoc(v, vars, inExpr)

	case *types.Array:
		result := types.MakeArray(v.Len())
		arrIter := v.Iterator()

		defer arrIter.Close()

		for {
			_, elem, err := arrIter.Next()
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}

			if err != nil {
				break
			}

			result.Append(substituteVarsInValue(elem, vars, inExpr))
		}

		return result

	default:
		return val
	}
}

// getFieldValue retrieves the value of a field from a document.
// Supports dotted paths (e.g. "items.sku") via GetByPath.
// Returns nil if the field does not exist.
func getFieldValue(doc *types.Document, field string) any {
	if !strings.Contains(field, ".") {
		v, err := doc.Get(field)
		if err != nil {
			return nil
		}

		return v
	}

	path, err := types.NewPathFromString(field)
	if err != nil {
		return nil
	}

	v, err := doc.GetByPath(path)
	if err != nil {
		return nil
	}

	return v
}

// indexedArray is an array a query path reaches by an index, such as k.0 of
// {k: [[5]]}; a query compares it whole, without matching its elements.
type indexedArray struct {
	arr *types.Array
}

// foreignMatchesCandidate reports whether foreignVal matches the query
// equality {foreignField: candidate}: it equals candidate, or it is an array
// with a top-level element that does. A null candidate matches a null or
// missing foreignVal.
func foreignMatchesCandidate(foreignVal, candidate any) bool {
	if indexed, ok := foreignVal.(indexedArray); ok {
		return valuesEqual(indexed.arr, candidate)
	}
	if valuesEqual(foreignVal, candidate) {
		return true
	}
	foreignArr, ok := foreignVal.(*types.Array)
	return ok && arrayContainsValue(foreignArr, candidate)
}

// arrayContainsValue reports whether the array contains a value equal to target.
func arrayContainsValue(arr *types.Array, target any) bool {
	for i := 0; i < arr.Len(); i++ {
		if valuesEqual(must.NotFail(arr.Get(i)), target) {
			return true
		}
	}
	return false
}

func isNullOrMissing(v any) bool {
	if v == nil {
		return true
	}
	_, ok := v.(types.NullType)
	return ok
}

// valuesEqual compares two values for equality in the context of $lookup matching.
// A missing value (nil) equals null. An array equals only an equal array.
func valuesEqual(a, b any) bool {
	if isNullOrMissing(a) || isNullOrMissing(b) {
		return isNullOrMissing(a) && isNullOrMissing(b)
	}

	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	}

	return types.CompareForAggregation(a, b) == types.Equal
}

// equalityFetchFilter returns {foreignField: {$in: [...]}} over the scalar
// localField values of docs, which selects every foreign document an equality
// join can match. It returns nil (fetch everything) when a value is missing,
// null, empty, or not a plain scalar, since $in would not match those the same
// way.
func equalityFetchFilter(docs []*types.Document, localField, foreignField string) *types.Document {
	values := types.MakeArray(len(docs))
	seen := make(map[string]struct{}, len(docs))
	add := func(v any) bool {
		switch v := v.(type) {
		case string, int32, int64, bool, types.ObjectID, time.Time:
		case float64:
			if math.IsNaN(v) {
				return false
			}
		default:
			return false
		}
		key := fmt.Sprintf("%T|%v", v, v)
		if _, dup := seen[key]; !dup {
			seen[key] = struct{}{}
			values.Append(v)
		}
		return true
	}

	for _, doc := range docs {
		for _, v := range lookupLocalCandidates(doc, localField) {
			if !add(v) {
				return nil
			}
		}
	}

	return must.NotFail(types.NewDocument(foreignField, must.NotFail(types.NewDocument("$in", values))))
}

var (
	_ aggregations.Stage = (*lookup)(nil)
)

// startsWithDocuments reports whether a $lookup spec's pipeline starts with
// $documents, which supplies the documents instead of a collection.
func startsWithDocuments(spec *types.Document) bool {
	pipeline, _ := spec.Get("pipeline")
	arr, ok := pipeline.(*types.Array)
	if !ok || arr.Len() == 0 {
		return false
	}
	first, ok := must.NotFail(arr.Get(0)).(*types.Document)
	return ok && first.Command() == "$documents"
}

// checkSubPipelineSource rejects an explicitly named collection that is
// empty or paired with a leading $documents, before any collection is read.
func checkSubPipelineSource(coll string, spec *types.Document, stage string) error {
	switch {
	case coll == "":
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace,
			"Invalid "+stage+" namespace: collection name cannot be empty", stage+" (stage)")
	case startsWithDocuments(spec):
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace,
			"'$documents' can only be run with {aggregate: 1}", "$documents (stage)")
	}
	return nil
}

// EvalDocuments evaluates a $documents stage as mongod's desugaring does:
// the value must be an array (5858203) of documents (40228).
func EvalDocuments(stage *types.Document) ([]*types.Document, error) {
	value := must.NotFail(stage.Get("$documents"))
	if err := operators.CheckFieldPaths(value); err != nil {
		return nil, err
	}
	v, _, err := operators.EvalOperand(value, types.MakeDocument(0))
	if err != nil {
		return nil, err
	}
	arr, ok := v.(*types.Array)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrUnwindNotArray,
			"an array is expected", "$documents (stage)")
	}
	docs := make([]*types.Document, arr.Len())
	for i := range docs {
		d, ok := must.NotFail(arr.Get(i)).(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrReplaceRootNotObject,
				fmt.Sprintf("elements within the array passed to $documents must evaluate to an object, but resulting value was: %s. "+
					"Type of resulting value: '%s'.", types.FormatAnyValue(must.NotFail(arr.Get(i))), handlerparams.AliasFromType(must.NotFail(arr.Get(i)))),
				"$documents (stage)")
		}
		docs[i] = d
	}
	return docs, nil
}

// checkFirstOnlyStages returns mongod's error for a $collStats that is not
// the first stage of a sub-pipeline.
func checkFirstOnlyStages(pipeline *types.Array) error {
	for i := 1; i < pipeline.Len(); i++ {
		if d, ok := must.NotFail(pipeline.Get(i)).(*types.Document); ok && d.Len() == 1 && d.Command() == "$collStats" {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCollStatsIsNotFirstStage,
				"$collStats is only valid as the first stage in a pipeline", "$collStats (stage)")
		}
	}
	return nil
}
