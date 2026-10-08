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
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

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
	localField   string         // simple form
	foreignField string         // simple form
	letVars      *types.Document // pipeline form: variable bindings
	pipeline     *types.Array   // pipeline form (may be nil)
	as           string
	fetcher      CollectionFetcher
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

	// "from" is required.
	fromVal, err := specDoc.Get("from")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"$lookup requires a 'from' option",
			"$lookup (stage)",
		)
	}

	from, ok := fromVal.(string)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			fmt.Sprintf("$lookup 'from' must be a string, got %s", types.FormatAnyValue(fromVal)),
			"$lookup (stage)",
		)
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
		keys, ok := equalityKeys(getFieldValue(d, foreignField))
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

// candidates returns, in their original order, the foreign documents that can
// equal localVal: all of them when localVal has no key.
func (idx *equalityIndex) candidates(localVal any) []*types.Document {
	keys, ok := equalityKeys(localVal)
	if !ok {
		return idx.docs
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
	localVal := getFieldValue(doc, l.localField)

	matched := make([]*types.Document, 0)
	for _, fromDoc := range idx.candidates(localVal) {
		if lookupValuesMatch(localVal, getFieldValue(fromDoc, l.foreignField)) {
			matched = append(matched, fromDoc)
		}
	}
	return matched
}

func (l *lookup) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := common.ConsumeDocuments(iter, "$lookup")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	fetcher := withFetchCache(l.fetcher)

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
	if perDocField == "" {
		if fromDocs, err = fetcher.Fetch(ctx, l.from, fetchFilter); err != nil {
			return nil, lazyerrors.Error(err)
		}
	}

	var eqIdx *equalityIndex
	switch {
	case l.localField == "":
	case fetchFilter == nil:
		// fromDocs is the cached whole collection; share its index with
		// other lookups in this stage, such as nested ones.
		if eqIdx, err = fetcher.unfilteredEqualityIndex(ctx, l.from, l.foreignField); err != nil {
			return nil, lazyerrors.Error(err)
		}
	default:
		eqIdx = newEqualityIndex(fromDocs, l.foreignField)
	}

	out := make([]*types.Document, 0, len(docs))

	if l.pipeline != nil {
		// Pipeline form: run the pipeline against the from collection for each input doc.
		for _, doc := range docs {
			candidates := fromDocs
			if perDocField != "" {
				if candidates, err = l.perDocCandidates(ctx, fetcher, doc, perDocField); err != nil {
					return nil, lazyerrors.Error(err)
				}
			}
			if eqIdx != nil {
				candidates = l.equalityMatches(doc, eqIdx)
			}

			matched, pErr := l.runPipeline(ctx, fetcher, candidates, doc)
			if pErr != nil {
				return nil, pErr
			}

			newDoc := doc.DeepCopy()
			arr := types.MakeArray(len(matched))

			for _, m := range matched {
				arr.Append(m)
			}

			newDoc.Set(l.as, arr)
			out = append(out, newDoc)
		}
	} else {
		// Simple equality join form.
		// MongoDB equality semantics: a foreign doc matches if any element of
		// localField (treated as a singleton when scalar) equals any element of
		// foreignField (treated as a singleton when scalar).
		for _, doc := range docs {
			matched := l.equalityMatches(doc, eqIdx)
			for i, m := range matched {
				matched[i] = m.DeepCopy()
			}

			newDoc := doc.DeepCopy()
			arr := types.MakeArray(len(matched))

			for _, m := range matched {
				arr.Append(m)
			}

			newDoc.Set(l.as, arr)
			out = append(out, newDoc)
		}
	}

	result := iterator.Values(iterator.ForSlice(out))
	closer.Add(result)

	return result, nil
}

// runPipeline runs the $lookup pipeline form against a set of documents.
// It evaluates let variables from localDoc, substitutes them into the pipeline spec,
// and executes each stage in sequence. Nested $lookup stages are supported.
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
		vars[k] = evaluateLetExpr(v, localDoc)
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
	value, ok := leadingMatchEqualityValue(substituteVarsInDoc(first, vars), field)
	if !ok {
		return fetcher.Fetch(ctx, l.from, nil)
	}
	return fetcher.Fetch(ctx, l.from, must.NotFail(types.NewDocument(field, value)))
}

func (l *lookup) runPipeline(ctx context.Context, fetcher CollectionFetcher, fromDocs []*types.Document, localDoc *types.Document) ([]*types.Document, error) {
	vars, err := l.letBindings(localDoc)
	if err != nil {
		return nil, err
	}

	// fromDocs may be shared: run leading $match stages on them directly (they
	// do not modify documents) and copy only the survivors.
	current := fromDocs
	copied := false
	copyCurrent := func() {
		copies := make([]*types.Document, len(current))
		for i, d := range current {
			copies[i] = d.DeepCopy()
		}
		current = copies
		copied = true
	}

	// Execute each pipeline stage.
	pipeIter := l.pipeline.Iterator()
	defer pipeIter.Close()

	for {
		_, stageVal, err := pipeIter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		stageDoc, ok := stageVal.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"Each element of the '$lookup' pipeline must be an object",
				"$lookup (stage)",
			)
		}

		// Substitute let variables into the stage spec.
		substituted := substituteVarsInDoc(stageDoc, vars)

		// Create the stage. $lookup is handled specially to pass the fetcher.
		var stage aggregations.Stage

		if substituted.Command() == "$lookup" {
			stage, err = NewLookupStage(substituted, fetcher)
		} else {
			stage, err = NewStage(substituted)
		}

		if err != nil {
			return nil, err
		}

		if !copied && substituted.Command() != "$match" {
			copyCurrent()
		}

		// Run the stage against the current set of documents.
		stageInput := iterator.Values(iterator.ForSlice(current))
		stageCloser := iterator.NewMultiCloser()

		stageOutput, sErr := stage.Process(ctx, stageInput, stageCloser)
		if sErr != nil {
			stageCloser.Close()
			return nil, sErr
		}

		current, err = common.ConsumeDocuments(stageOutput, "$lookup")
		stageCloser.Close()

		if err != nil {
			return nil, lazyerrors.Error(err)
		}
	}

	if !copied {
		copyCurrent()
	}
	return current, nil
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
	switch v := value.(type) {
	case string:
		if strings.HasPrefix(v, "$") {
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

// evaluateLetExpr evaluates a let variable expression against a local document.
// Expressions of the form "$fieldName" are resolved to the field value from localDoc.
// Non-expression values are returned as-is.
func evaluateLetExpr(expr any, localDoc *types.Document) any {
	s, ok := expr.(string)
	if !ok {
		return expr
	}

	if !strings.HasPrefix(s, "$") {
		return s
	}

	// $$ prefix would be a variable reference  -- not valid in let values.
	if strings.HasPrefix(s, "$$") {
		return types.Null
	}

	fieldName := strings.TrimPrefix(s, "$")
	if fieldName == "" {
		return types.Null
	}

	val := getFieldValue(localDoc, fieldName)
	if val == nil {
		return types.Null
	}

	return val
}

// substituteVarsInDoc recursively replaces $$varName references in a document
// with the corresponding values from vars.
func substituteVarsInDoc(doc *types.Document, vars map[string]any) *types.Document {
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

		result.Set(k, substituteVarsInValue(v, vars))
	}

	return result
}

// substituteVarsInValue recursively substitutes $$varName references in a value.
func substituteVarsInValue(val any, vars map[string]any) any {
	switch v := val.(type) {
	case string:
		if strings.HasPrefix(v, "$$") {
			varName := strings.TrimPrefix(v, "$$")
			if resolved, ok := vars[varName]; ok {
				return resolved
			}
		}

		return v

	case *types.Document:
		return substituteVarsInDoc(v, vars)

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

			result.Append(substituteVarsInValue(elem, vars))
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

// lookupValuesMatch reports whether localVal and foreignVal match under MongoDB
// $lookup equality semantics: arrays on either side are treated as a set of
// candidate values, and a match exists if any candidate from one side equals
// any candidate from the other.
func lookupValuesMatch(localVal, foreignVal any) bool {
	localArr, localIsArr := localVal.(*types.Array)
	foreignArr, foreignIsArr := foreignVal.(*types.Array)

	switch {
	case localIsArr && foreignIsArr:
		arrIter := localArr.Iterator()
		defer arrIter.Close()

		for {
			_, elem, err := arrIter.Next()
			if errors.Is(err, iterator.ErrIteratorDone) {
				return false
			}

			if err != nil {
				return false
			}

			if arrayContainsValue(foreignArr, elem) {
				return true
			}
		}
	case localIsArr:
		return arrayContainsValue(localArr, foreignVal)
	case foreignIsArr:
		return arrayContainsValue(foreignArr, localVal)
	default:
		return valuesEqual(localVal, foreignVal)
	}
}

// arrayContainsValue reports whether the array contains a value equal to target.
func arrayContainsValue(arr *types.Array, target any) bool {
	arrIter := arr.Iterator()
	defer arrIter.Close()

	for {
		_, elem, err := arrIter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			break
		}

		if valuesEqual(elem, target) {
			return true
		}
	}

	return false
}

// valuesEqual compares two values for equality in the context of $lookup matching.
func valuesEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	// Use types.Compare if available, otherwise use reflect-based comparison.
	// For now, use a simple type-switch approach for common types.
	switch av := a.(type) {
	case int32:
		switch bv := b.(type) {
		case int32:
			return av == bv
		case int64:
			return int64(av) == bv
		case float64:
			return float64(av) == bv
		}
	case int64:
		switch bv := b.(type) {
		case int64:
			return av == bv
		case int32:
			return av == int64(bv)
		case float64:
			return float64(av) == bv
		}
	case float64:
		switch bv := b.(type) {
		case float64:
			return av == bv
		case int32:
			return av == float64(bv)
		case int64:
			return av == float64(bv)
		}
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	}

	return types.Compare(a, b) == types.Equal
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
		v := getFieldValue(doc, localField)
		arr, isArr := v.(*types.Array)
		if !isArr {
			if !add(v) {
				return nil
			}
			continue
		}
		if arr.Len() == 0 {
			return nil
		}
		for i := 0; i < arr.Len(); i++ {
			if !add(must.NotFail(arr.Get(i))) {
				return nil
			}
		}
	}

	return must.NotFail(types.NewDocument(foreignField, must.NotFail(types.NewDocument("$in", values))))
}

var (
	_ aggregations.Stage = (*lookup)(nil)
)
