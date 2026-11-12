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

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
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
func (c *cachingFetcher) unfilteredEqualityIndex(ctx context.Context, collectionName, field string) (*equalityIndex, error) {
	key := collectionName + "\x00" + field
	if idx, ok := c.eqIndexes[key]; ok {
		return idx, nil
	}
	docs, err := c.Fetch(ctx, collectionName, nil)
	if err != nil {
		return nil, err
	}
	idx := newEqualityIndex(docs, field)
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

	l := &lookup{
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

			l.letVars = letDoc
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

func newEqualityIndex(docs []*types.Document, foreignField string) *equalityIndex {
	idx := &equalityIndex{docs: docs, byKey: make(map[string][]int)}
	for pos, d := range docs {
		values, missing := lookupForeignValues(d, foreignField)
		var keys []string
		ok := !missing
		for _, v := range values {
			vk, vok := equalityKeys(v)
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

	matched := make([]*types.Document, 0)
	for _, fromDoc := range idx.candidatesFor(candidates) {
		if foreignMatches(fromDoc, l.foreignField, candidates) {
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
			for i := range v.Len() {
				if elem, ok := must.NotFail(v.Get(i)).(*types.Document); ok {
					walk(elem, parts)
				}
			}
		}
	}
	walk(doc, strings.Split(field, "."))
	if len(out) == 0 {
		return []any{nil}
	}
	return out
}

// lookupForeignValues returns the values a query on field reaches in doc,
// traversing arrays along the path, and whether some branch lacks the field.
func lookupForeignValues(doc *types.Document, field string) ([]any, bool) {
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
			for i := range v.Len() {
				if elem, ok := must.NotFail(v.Get(i)).(*types.Document); ok {
					walk(elem, parts)
				}
			}
		default:
			missing = true
		}
	}
	walk(doc, strings.Split(field, "."))
	return out, missing || len(out) == 0
}

// foreignMatches reports whether doc matches {field: {$in: candidates}}.
func foreignMatches(doc *types.Document, field string, candidates []any) bool {
	values, missing := lookupForeignValues(doc, field)
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
	if l.localField != "" && fetcher.IndexedField(ctx, l.from, l.foreignField) {
		fetchFilter = equalityFetchFilter(docs, l.localField, l.foreignField)
	}
	perDocField := ""
	if l.pipeline != nil && l.localField == "" {
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
		if eqIdx, err = fetcher.unfilteredEqualityIndex(ctx, l.from, l.foreignField); err != nil {
			return nil, lazyerrors.Error(err)
		}
	default:
		eqIdx = newEqualityIndex(fromDocs, l.foreignField)
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
		for j := i + 1; j < len(pipeline) && j < len(specs); j++ {
			if specs[j].Command() == "$match" && !matchReadsField(specs[j], l.as) {
				continue
			}
			if u, ok := pipeline[j].(*unwind); ok && unwindPath(specs[j]) == "$"+l.as {
				l.unwind = u
				u.absorbed = true
			}
			break
		}
	}
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
		default:
			if strings.HasPrefix(key, "$") || key == field ||
				strings.HasPrefix(key, field+".") || strings.HasPrefix(field, key+".") {
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
		specs[stageIndex] = substituted

		if stageIndex == 0 && l.from == "" {
			docs, err := evalDocuments(substituted)
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
	if l.sbeSubPipeline {
		MarkSBELookups(built, specs)
	}

	for stageIndex, stage := range built {
		if stage == nil {
			continue
		}
		if stageIndex < l.sbePrefix {
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

// substituteVarsInStage substitutes let variables into a sub-pipeline stage.
// A $match query only reads variables inside $expr.
func substituteVarsInStage(stage *types.Document, vars map[string]any) *types.Document {
	if len(vars) == 0 || stage.Len() != 1 || stage.Command() != "$match" {
		return substituteVarsInDoc(stage, vars, false)
	}
	query, ok := must.NotFail(stage.Get("$match")).(*types.Document)
	if !ok {
		return stage
	}
	return must.NotFail(types.NewDocument("$match", substituteVarsInQuery(query, vars)))
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
// such as "$b" are not evaluated again.
func substituteVarsInDoc(doc *types.Document, vars map[string]any, inExpr bool) *types.Document {
	if len(vars) == 0 {
		return doc
	}

	result := new(types.Document)
	docIter := doc.Iterator()

	defer docIter.Close()

	for {
		k, v, err := docIter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			break
		}

		result.Set(k, substituteVarsInValue(v, vars, inExpr))
	}

	return result
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
			varName := strings.TrimPrefix(v, "$$")
			if resolved, ok := vars[varName]; ok {
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

// lookupValuesMatch reports whether foreignVal matches localVal the way mongod
// matches {foreignField: {$in: <candidates>}}, where the candidates are the
// elements of an array localVal, or localVal itself otherwise. A missing value
// or an empty array is a null candidate. An inner array is a single candidate.
func lookupValuesMatch(localVal, foreignVal any) bool {
	localArr, localIsArr := localVal.(*types.Array)
	if !localIsArr || localArr.Len() == 0 {
		if localIsArr {
			localVal = nil
		}
		return foreignMatchesCandidate(foreignVal, localVal)
	}

	for i := 0; i < localArr.Len(); i++ {
		if foreignMatchesCandidate(foreignVal, must.NotFail(localArr.Get(i))) {
			return true
		}
	}
	return false
}

// foreignMatchesCandidate reports whether foreignVal matches the query
// equality {foreignField: candidate}: it equals candidate, or it is an array
// with a top-level element that does. A null candidate matches a null or
// missing foreignVal.
func foreignMatchesCandidate(foreignVal, candidate any) bool {
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

// evalDocuments evaluates a $documents stage as mongod's desugaring does:
// the value must be an array (5858203) of documents (40228).
func evalDocuments(stage *types.Document) ([]*types.Document, error) {
	v, _, err := operators.EvalOperand(must.NotFail(stage.Get("$documents")), types.MakeDocument(0))
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
				"'replacement document' must evaluate to an object, but resulting value was: "+
					types.FormatAnyValue(must.NotFail(arr.Get(i))), "$documents (stage)")
		}
		docs[i] = d
	}
	return docs, nil
}
