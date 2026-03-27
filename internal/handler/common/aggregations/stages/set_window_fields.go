// Copyright 2021 FerretDB Inc.
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

	"github.com/dolthub/dongo/internal/handler/common"
	"github.com/dolthub/dongo/internal/handler/common/aggregations"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
	"github.com/dolthub/dongo/internal/util/must"
)

// windowBound represents a window boundary.
type windowBound struct {
	unbounded bool
	current   bool
	offset    int64 // used when not unbounded and not current
}

// windowFrameType is either "documents" or "range".
type windowFrameType int

const (
	windowFrameDocuments windowFrameType = iota
	windowFrameRange
)

// windowFrame represents the window frame specification.
type windowFrame struct {
	frameType windowFrameType
	lower     windowBound
	upper     windowBound
}

// windowOutputField describes one output field in $setWindowFields.
type windowOutputField struct {
	outputField string
	operator    string     // e.g. "$rank", "$sum"
	expression  any        // for operators that take an expression (sum, avg, min, max)
	frame       *windowFrame // nil for rank/denseRank/documentNumber
}

// setWindowFields represents $setWindowFields aggregation stage.
//
//	{ $setWindowFields: {
//	    partitionBy: <expression>,
//	    sortBy: { <field>: <sort order>, ... },
//	    output: {
//	      <field>: {
//	        <windowOperator>: <expression>,
//	        window: {
//	          documents: [ <lower>, <upper> ],  // or range
//	        }
//	      }
//	    }
//	  }
//	}
type setWindowFields struct {
	partitionBy any // expression or nil (single partition)
	sortBy      *types.Document
	outputs     []windowOutputField
}

// newSetWindowFields creates a new $setWindowFields stage.
func newSetWindowFields(stage *types.Document) (aggregations.Stage, error) {
	spec, err := common.GetRequiredParam[*types.Document](stage, "$setWindowFields")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"$setWindowFields specification stage must be an object",
			"$setWindowFields (stage)",
		)
	}

	swf := new(setWindowFields)

	// Parse partitionBy (optional).
	if spec.Has("partitionBy") {
		pb := must.NotFail(spec.Get("partitionBy"))
		swf.partitionBy = pb
	}

	// Parse sortBy (optional).
	if spec.Has("sortBy") {
		sb, err := common.GetRequiredParam[*types.Document](spec, "sortBy")
		if err != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"$setWindowFields sortBy must be an object",
				"$setWindowFields (stage)",
			)
		}
		swf.sortBy = sb
	}

	// Parse output (required).
	outputDoc, err := common.GetRequiredParam[*types.Document](spec, "output")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"$setWindowFields output must be an object",
			"$setWindowFields (stage)",
		)
	}

	iter := outputDoc.Iterator()
	defer iter.Close()

	for {
		field, val, iterErr := iter.Next()
		if errors.Is(iterErr, iterator.ErrIteratorDone) {
			break
		}

		if iterErr != nil {
			return nil, lazyerrors.Error(iterErr)
		}

		fieldSpec, ok := val.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf("$setWindowFields output field %q must be an object", field),
				"$setWindowFields (stage)",
			)
		}

		wof, parseErr := parseWindowOutputField(field, fieldSpec)
		if parseErr != nil {
			return nil, parseErr
		}

		swf.outputs = append(swf.outputs, wof)
	}

	return swf, nil
}

// parseWindowOutputField parses one output field specification.
func parseWindowOutputField(field string, spec *types.Document) (windowOutputField, error) {
	wof := windowOutputField{outputField: field}

	// Extract the window operator and expression (all keys except "window").
	specIter := spec.Iterator()
	defer specIter.Close()

	for {
		k, v, err := specIter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return wof, lazyerrors.Error(err)
		}

		if k == "window" {
			continue
		}

		// This is the window operator.
		wof.operator = k

		switch k {
		case "$rank", "$denseRank", "$documentNumber":
			// These operators take no argument (must be {} or no arg).
		case "$sum", "$avg", "$min", "$max":
			wof.expression = v
		default:
			return wof, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNotImplemented,
				fmt.Sprintf("$setWindowFields window operator %q is not implemented yet", k),
				"$setWindowFields (stage)",
			)
		}
	}

	if wof.operator == "" {
		return wof, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			fmt.Sprintf("$setWindowFields output field %q must specify a window operator", field),
			"$setWindowFields (stage)",
		)
	}

	// Parse optional window frame (not applicable for rank/denseRank/documentNumber).
	if spec.Has("window") {
		switch wof.operator {
		case "$rank", "$denseRank", "$documentNumber":
			// Ignore window spec for these operators.
		default:
			windowDoc, err := common.GetRequiredParam[*types.Document](spec, "window")
			if err != nil {
				return wof, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrTypeMismatch,
					"$setWindowFields window must be an object",
					"$setWindowFields (stage)",
				)
			}

			frame, err := parseWindowFrame(windowDoc)
			if err != nil {
				return wof, err
			}

			wof.frame = frame
		}
	}

	return wof, nil
}

// parseWindowFrame parses the window frame specification.
func parseWindowFrame(doc *types.Document) (*windowFrame, error) {
	frame := new(windowFrame)

	if doc.Has("documents") {
		frame.frameType = windowFrameDocuments

		arr, err := common.GetRequiredParam[*types.Array](doc, "documents")
		if err != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"$setWindowFields window documents must be an array",
				"$setWindowFields (stage)",
			)
		}

		if arr.Len() != 2 {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$setWindowFields window documents must have exactly 2 elements",
				"$setWindowFields (stage)",
			)
		}

		lower, err := parseWindowBound(must.NotFail(arr.Get(0)))
		if err != nil {
			return nil, err
		}

		upper, err := parseWindowBound(must.NotFail(arr.Get(1)))
		if err != nil {
			return nil, err
		}

		frame.lower = lower
		frame.upper = upper

		return frame, nil
	}

	if doc.Has("range") {
		frame.frameType = windowFrameRange

		arr, err := common.GetRequiredParam[*types.Array](doc, "range")
		if err != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"$setWindowFields window range must be an array",
				"$setWindowFields (stage)",
			)
		}

		if arr.Len() != 2 {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$setWindowFields window range must have exactly 2 elements",
				"$setWindowFields (stage)",
			)
		}

		lower, err := parseWindowBound(must.NotFail(arr.Get(0)))
		if err != nil {
			return nil, err
		}

		upper, err := parseWindowBound(must.NotFail(arr.Get(1)))
		if err != nil {
			return nil, err
		}

		frame.lower = lower
		frame.upper = upper

		return frame, nil
	}

	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrBadValue,
		"$setWindowFields window must specify 'documents' or 'range'",
		"$setWindowFields (stage)",
	)
}

// parseWindowBound parses a single window bound value.
func parseWindowBound(v any) (windowBound, error) {
	switch val := v.(type) {
	case string:
		switch val {
		case "unbounded":
			return windowBound{unbounded: true}, nil
		case "current":
			return windowBound{current: true}, nil
		default:
			return windowBound{}, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("invalid window bound %q: must be 'unbounded', 'current', or a number", val),
				"$setWindowFields (stage)",
			)
		}
	case int32:
		return windowBound{offset: int64(val)}, nil
	case int64:
		return windowBound{offset: val}, nil
	case float64:
		return windowBound{offset: int64(val)}, nil
	default:
		return windowBound{}, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"window bound must be 'unbounded', 'current', or a number",
			"$setWindowFields (stage)",
		)
	}
}

// Process implements Stage interface.
func (s *setWindowFields) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	// Collect all documents.
	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Partition documents.
	partitions, err := s.partitionDocuments(docs)
	if err != nil {
		return nil, err
	}

	var result []*types.Document

	for _, partition := range partitions {
		// Sort within partition.
		if s.sortBy != nil && s.sortBy.Len() > 0 {
			if err = common.SortDocuments(partition, s.sortBy); err != nil {
				return nil, lazyerrors.Error(err)
			}
		}

		// For each document in the partition, compute all output fields.
		processed, err := s.applyWindowFunctions(partition)
		if err != nil {
			return nil, err
		}

		result = append(result, processed...)
	}

	res := iterator.Values(iterator.ForSlice(result))
	closer.Add(res)

	return res, nil
}

// partitionDocuments groups documents by the partitionBy expression.
// If partitionBy is nil, all documents are in a single partition.
func (s *setWindowFields) partitionDocuments(docs []*types.Document) ([][]*types.Document, error) {
	if s.partitionBy == nil {
		return [][]*types.Document{docs}, nil
	}

	type partEntry struct {
		key  any
		docs []*types.Document
	}

	var entries []partEntry

	for _, doc := range docs {
		key, err := s.evaluatePartitionKey(doc)
		if err != nil {
			key = types.Null
		}

		found := false

		for i, entry := range entries {
			if types.CompareForAggregation(key, entry.key) == types.Equal {
				entries[i].docs = append(entries[i].docs, doc)
				found = true

				break
			}
		}

		if !found {
			entries = append(entries, partEntry{key: key, docs: []*types.Document{doc}})
		}
	}

	result := make([][]*types.Document, len(entries))

	for i, entry := range entries {
		result[i] = entry.docs
	}

	return result, nil
}

// evaluatePartitionKey evaluates the partitionBy expression for a document.
func (s *setWindowFields) evaluatePartitionKey(doc *types.Document) (any, error) {
	switch pb := s.partitionBy.(type) {
	case string:
		expr, err := aggregations.NewExpression(pb, nil)
		if err != nil {
			return types.Null, nil
		}

		val, err := expr.Evaluate(doc)
		if err != nil {
			return types.Null, nil
		}

		return val, nil
	case *types.Document, *types.Array, float64, bool, int32, int64, types.NullType:
		return pb, nil
	default:
		return types.Null, nil
	}
}

// applyWindowFunctions applies all window output fields to each document in a partition.
func (s *setWindowFields) applyWindowFunctions(partition []*types.Document) ([]*types.Document, error) {
	n := len(partition)
	result := make([]*types.Document, n)

	for i, doc := range partition {
		// Clone the document to avoid mutating the original.
		clone, err := cloneDocument(doc)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		result[i] = clone
	}

	for _, wof := range s.outputs {
		vals, err := s.computeWindowValues(partition, wof)
		if err != nil {
			return nil, err
		}

		for i, val := range vals {
			result[i].Set(wof.outputField, val)
		}
	}

	return result, nil
}

// cloneDocument creates a shallow clone of a document.
func cloneDocument(doc *types.Document) (*types.Document, error) {
	clone := new(types.Document)
	docIter := doc.Iterator()
	defer docIter.Close()

	for {
		k, v, err := docIter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		clone.Set(k, v)
	}

	return clone, nil
}

// computeWindowValues computes the window function value for each document in the partition.
func (s *setWindowFields) computeWindowValues(partition []*types.Document, wof windowOutputField) ([]any, error) {
	n := len(partition)
	vals := make([]any, n)

	switch wof.operator {
	case "$documentNumber":
		for i := range partition {
			vals[i] = int64(i + 1)
		}

	case "$rank":
		vals = computeRank(partition, s.sortBy, false)

	case "$denseRank":
		vals = computeRank(partition, s.sortBy, true)

	case "$sum", "$avg", "$min", "$max":
		for i := range partition {
			lo, hi := s.windowBounds(wof.frame, i, n)

			window := partition[lo:hi]

			val, err := applyAccumulator(wof.operator, wof.expression, window)
			if err != nil {
				return nil, err
			}

			vals[i] = val
		}

	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNotImplemented,
			fmt.Sprintf("$setWindowFields window operator %q is not implemented yet", wof.operator),
			"$setWindowFields (stage)",
		)
	}

	return vals, nil
}

// windowBounds returns the [lo, hi) slice indices for the window frame at position i in partition of size n.
// Default frame (nil) is [unbounded, unbounded] for non-rank operators.
func (s *setWindowFields) windowBounds(frame *windowFrame, i, n int) (int, int) {
	if frame == nil {
		// Default: entire partition.
		return 0, n
	}

	lo := resolveBound(frame.lower, i, n, true)
	hi := resolveBound(frame.upper, i, n, false)

	// Clamp to valid range.
	if lo < 0 {
		lo = 0
	}

	if hi > n {
		hi = n
	}

	if lo > hi {
		lo = hi
	}

	return lo, hi
}

// resolveBound resolves a window bound to an integer slice index.
// isLower indicates whether this is the lower bound (for unbounded).
func resolveBound(b windowBound, i, n int, isLower bool) int {
	switch {
	case b.unbounded:
		if isLower {
			return 0
		}

		return n
	case b.current:
		return i + 1 // hi is exclusive for upper, inclusive-ish for lower; we handle this below
	default:
		// Offset relative to current document (i is 0-based).
		if isLower {
			return i + int(b.offset)
		}

		return i + int(b.offset) + 1
	}
}

// computeRank computes $rank or $denseRank values for all documents in a partition.
func computeRank(partition []*types.Document, sortBy *types.Document, dense bool) []any {
	n := len(partition)
	vals := make([]any, n)

	if n == 0 {
		return vals
	}

	rank := int64(1)
	denseRankVal := int64(1)
	vals[0] = int64(1)

	for i := 1; i < n; i++ {
		tied := isTied(partition[i-1], partition[i], sortBy)

		if tied {
			if dense {
				vals[i] = denseRankVal
			} else {
				vals[i] = vals[i-1]
			}
		} else {
			// i+1 because rank is 1-based and i is 0-based current position.
			rank = int64(i + 1)
			denseRankVal++

			if dense {
				vals[i] = denseRankVal
			} else {
				vals[i] = rank
			}
		}
	}

	return vals
}

// isTied returns true if two documents have the same sort key values.
func isTied(a, b *types.Document, sortBy *types.Document) bool {
	if sortBy == nil || sortBy.Len() == 0 {
		return true
	}

	for _, key := range sortBy.Keys() {
		aVal, _ := a.Get(key)
		bVal, _ := b.Get(key)

		if aVal == nil {
			aVal = types.Null
		}

		if bVal == nil {
			bVal = types.Null
		}

		if types.CompareForAggregation(aVal, bVal) != types.Equal {
			return false
		}
	}

	return true
}

// applyAccumulator applies $sum/$avg/$min/$max over a window slice of documents.
func applyAccumulator(op string, expression any, window []*types.Document) (any, error) {
	// Collect values from window documents by evaluating the expression.
	values, err := collectWindowValues(expression, window)
	if err != nil {
		return nil, err
	}

	switch op {
	case "$sum":
		return aggregations.SumNumbers(values...), nil
	case "$avg":
		return avgWindowValues(values), nil
	case "$min":
		return minWindowValue(values), nil
	case "$max":
		return maxWindowValue(values), nil
	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNotImplemented,
			fmt.Sprintf("window accumulator %q not implemented", op),
			"$setWindowFields (stage)",
		)
	}
}

// collectWindowValues evaluates an expression against each document in the window.
func collectWindowValues(expression any, window []*types.Document) ([]any, error) {
	var values []any

	switch expr := expression.(type) {
	case string:
		aggExpr, err := aggregations.NewExpression(expr, nil)
		if err != nil {
			// Not a field path; treat as a literal.
			for range window {
				values = append(values, expression)
			}

			return values, nil
		}

		for _, doc := range window {
			val, err := aggExpr.Evaluate(doc)
			if err == nil {
				values = append(values, val)
			}
		}
	case float64, int32, int64:
		for range window {
			values = append(values, expr)
		}
	case nil:
		// No expression; treat as 1 for $sum compatibility.
		for range window {
			values = append(values, int32(1))
		}
	default:
		for range window {
			values = append(values, expression)
		}
	}

	return values, nil
}

// avgWindowValues computes the average of numeric values; returns null for empty.
func avgWindowValues(values []any) any {
	var sum float64
	var count int

	for _, v := range values {
		switch n := v.(type) {
		case float64:
			sum += n
			count++
		case int32:
			sum += float64(n)
			count++
		case int64:
			sum += float64(n)
			count++
		}
	}

	if count == 0 {
		return types.Null
	}

	return sum / float64(count)
}

// minWindowValue returns the minimum value; returns null for empty.
func minWindowValue(values []any) any {
	var result any

	for _, v := range values {
		if _, isNull := v.(types.NullType); isNull {
			continue
		}

		if result == nil {
			result = v
			continue
		}

		if types.CompareOrder(v, result, types.Ascending) == types.Less {
			result = v
		}
	}

	if result == nil {
		return types.Null
	}

	return result
}

// maxWindowValue returns the maximum value; returns null for empty.
func maxWindowValue(values []any) any {
	var result any

	for _, v := range values {
		if _, isNull := v.(types.NullType); isNull {
			continue
		}

		if result == nil {
			result = v
			continue
		}

		if types.CompareOrder(v, result, types.Ascending) == types.Greater {
			result = v
		}
	}

	if result == nil {
		return types.Null
	}

	return result
}

// check interfaces
var (
	_ aggregations.Stage = (*setWindowFields)(nil)
)
