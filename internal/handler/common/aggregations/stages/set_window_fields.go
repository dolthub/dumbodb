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
	operator    string       // e.g. "$rank", "$sum"
	expression  any          // for operators that take an expression (sum, avg, min, max)
	frame       *windowFrame // nil for rank/denseRank/documentNumber
	opSpec      any          // operator-specific config (for complex operators)
}

// shiftSpec holds $shift operator configuration.
type shiftSpec struct {
	outputExpr any
	by         int64
	defaultVal any
}

// emaSpec holds $expMovingAvg operator configuration.
type emaSpec struct {
	inputExpr any
	alpha     float64
}

// derivativeSpec holds $derivative/$integral operator configuration.
type derivativeSpec struct {
	inputExpr any
	unit      string
}

// covPairSpec holds two expressions for $covariancePop/$covarianceSamp.
type covPairSpec struct {
	expr1 any
	expr2 any
}

// topSpec holds $top/$bottom operator configuration.
type topSpec struct {
	sortBy     *types.Document
	outputExpr *types.Document
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
		case "$rank", "$denseRank", "$documentNumber", "$count", "$locf", "$linearFill":
			// These operators take no argument or a field path stored in expression.
			switch k {
			case "$locf", "$linearFill":
				wof.expression = v
			}
		case "$sum", "$avg", "$min", "$max", "$first", "$last", "$push", "$addToSet",
			"$stdDevPop", "$stdDevSamp":
			wof.expression = v
		case "$covariancePop", "$covarianceSamp":
			// Expects an array [expr1, expr2].
			arr, ok := v.(*types.Array)
			if !ok || arr.Len() != 2 {
				return wof, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					fmt.Sprintf("%s requires an array of exactly 2 expressions", k),
					"$setWindowFields (stage)",
				)
			}
			wof.opSpec = covPairSpec{
				expr1: must.NotFail(arr.Get(0)),
				expr2: must.NotFail(arr.Get(1)),
			}
		case "$shift":
			shiftDoc, ok := v.(*types.Document)
			if !ok {
				return wof, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrTypeMismatch,
					"$shift requires an object argument",
					"$setWindowFields (stage)",
				)
			}
			spec, parseErr := parseShiftSpec(shiftDoc)
			if parseErr != nil {
				return wof, parseErr
			}
			wof.opSpec = spec
		case "$expMovingAvg":
			emaDoc, ok := v.(*types.Document)
			if !ok {
				return wof, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrTypeMismatch,
					"$expMovingAvg requires an object argument",
					"$setWindowFields (stage)",
				)
			}
			spec, parseErr := parseEMASpec(emaDoc)
			if parseErr != nil {
				return wof, parseErr
			}
			wof.opSpec = spec
		case "$derivative", "$integral":
			derivDoc, ok := v.(*types.Document)
			if !ok {
				return wof, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrTypeMismatch,
					fmt.Sprintf("%s requires an object argument", k),
					"$setWindowFields (stage)",
				)
			}
			spec, parseErr := parseDerivativeSpec(derivDoc)
			if parseErr != nil {
				return wof, parseErr
			}
			wof.opSpec = spec
		case "$top", "$bottom":
			topDoc, ok := v.(*types.Document)
			if !ok {
				return wof, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrTypeMismatch,
					fmt.Sprintf("%s requires an object argument", k),
					"$setWindowFields (stage)",
				)
			}
			spec, parseErr := parseTopSpec(topDoc)
			if parseErr != nil {
				return wof, parseErr
			}
			wof.opSpec = spec
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

	// Parse optional window frame (not applicable for rank/denseRank/documentNumber/shift/expMovingAvg/locf/linearFill).
	if spec.Has("window") {
		switch wof.operator {
		case "$rank", "$denseRank", "$documentNumber",
			"$shift", "$expMovingAvg", "$locf", "$linearFill":
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

	case "$sum", "$avg", "$min", "$max", "$first", "$last", "$push", "$addToSet",
		"$stdDevPop", "$stdDevSamp":
		for i := range partition {
			lo, hi := s.windowBounds(wof.frame, i, n)

			window := partition[lo:hi]

			val, err := applyAccumulator(wof.operator, wof.expression, window)
			if err != nil {
				return nil, err
			}

			vals[i] = val
		}

	case "$count":
		for i := range partition {
			lo, hi := s.windowBounds(wof.frame, i, n)
			vals[i] = int64(hi - lo)
		}

	case "$covariancePop", "$covarianceSamp":
		spec := wof.opSpec.(covPairSpec)
		for i := range partition {
			lo, hi := s.windowBounds(wof.frame, i, n)
			window := partition[lo:hi]
			val := computeCovariance(wof.operator, spec, window)
			vals[i] = val
		}

	case "$shift":
		spec := wof.opSpec.(shiftSpec)
		for i := range partition {
			idx := i + int(spec.by)
			if idx < 0 || idx >= n {
				vals[i] = spec.defaultVal
			} else {
				vals[i] = evaluateExpr(spec.outputExpr, partition[idx])
			}
		}

	case "$expMovingAvg":
		spec := wof.opSpec.(emaSpec)
		vals = computeEMA(spec, partition)

	case "$derivative":
		spec := wof.opSpec.(derivativeSpec)
		for i := range partition {
			lo, hi := s.windowBounds(wof.frame, i, n)
			window := partition[lo:hi]
			vals[i] = computeDerivative(spec, window)
		}

	case "$integral":
		spec := wof.opSpec.(derivativeSpec)
		for i := range partition {
			lo, hi := s.windowBounds(wof.frame, i, n)
			window := partition[lo:hi]
			vals[i] = computeIntegral(spec, window)
		}

	case "$linearFill":
		vals = computeLinearFill(wof.expression, partition)

	case "$locf":
		vals = computeLocf(wof.expression, partition)

	case "$top", "$bottom":
		spec := wof.opSpec.(topSpec)
		for i := range partition {
			lo, hi := s.windowBounds(wof.frame, i, n)
			window := partition[lo:hi]
			val, err := computeTopBottom(wof.operator, spec, window)
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
		if isLower {
			return i // lower bound is inclusive: include current document
		}
		return i + 1 // upper bound is exclusive in slice notation: i+1 includes doc at i
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
	case "$first":
		if len(values) == 0 {
			return types.Null, nil
		}
		return values[0], nil
	case "$last":
		if len(values) == 0 {
			return types.Null, nil
		}
		return values[len(values)-1], nil
	case "$push":
		arr := types.MakeArray(len(values))
		for _, v := range values {
			arr.Append(v)
		}
		return arr, nil
	case "$addToSet":
		arr := types.MakeArray(0)
		for _, v := range values {
			found := false
			for i := range arr.Len() {
				existing := must.NotFail(arr.Get(i))
				if types.CompareForAggregation(v, existing) == types.Equal {
					found = true
					break
				}
			}
			if !found {
				arr.Append(v)
			}
		}
		return arr, nil
	case "$stdDevPop":
		return stdDevWindowValues(values, false), nil
	case "$stdDevSamp":
		return stdDevWindowValues(values, true), nil
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

// stdDevWindowValues computes population (sample=false) or sample (sample=true) standard deviation.
func stdDevWindowValues(values []any, sample bool) any {
	var nums []float64

	for _, v := range values {
		switch n := v.(type) {
		case float64:
			nums = append(nums, n)
		case int32:
			nums = append(nums, float64(n))
		case int64:
			nums = append(nums, float64(n))
		}
	}

	count := len(nums)
	if count == 0 {
		return types.Null
	}

	if sample && count < 2 {
		return types.Null
	}

	var sum float64
	for _, n := range nums {
		sum += n
	}

	mean := sum / float64(count)

	var variance float64
	for _, n := range nums {
		d := n - mean
		variance += d * d
	}

	divisor := float64(count)
	if sample {
		divisor = float64(count - 1)
	}

	return math.Sqrt(variance / divisor)
}

// parseShiftSpec parses the $shift operator configuration.
func parseShiftSpec(doc *types.Document) (shiftSpec, error) {
	spec := shiftSpec{}

	outputVal, err := doc.Get("output")
	if err != nil {
		return spec, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$shift requires 'output' field",
			"$setWindowFields (stage)",
		)
	}

	spec.outputExpr = outputVal

	byVal, err := doc.Get("by")
	if err != nil {
		return spec, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$shift requires 'by' field",
			"$setWindowFields (stage)",
		)
	}

	switch b := byVal.(type) {
	case int32:
		spec.by = int64(b)
	case int64:
		spec.by = b
	case float64:
		spec.by = int64(b)
	default:
		return spec, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$shift 'by' must be a number",
			"$setWindowFields (stage)",
		)
	}

	if doc.Has("default") {
		defVal := must.NotFail(doc.Get("default"))
		spec.defaultVal = defVal
	} else {
		spec.defaultVal = types.Null
	}

	return spec, nil
}

// parseEMASpec parses the $expMovingAvg operator configuration.
func parseEMASpec(doc *types.Document) (emaSpec, error) {
	spec := emaSpec{}

	inputVal, err := doc.Get("input")
	if err != nil {
		return spec, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$expMovingAvg requires 'input' field",
			"$setWindowFields (stage)",
		)
	}

	spec.inputExpr = inputVal

	if doc.Has("alpha") {
		alphaVal := must.NotFail(doc.Get("alpha"))
		switch a := alphaVal.(type) {
		case float64:
			spec.alpha = a
		case int32:
			spec.alpha = float64(a)
		case int64:
			spec.alpha = float64(a)
		default:
			return spec, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$expMovingAvg 'alpha' must be a number",
				"$setWindowFields (stage)",
			)
		}
	} else if doc.Has("N") {
		nVal := must.NotFail(doc.Get("N"))
		var n float64
		switch nv := nVal.(type) {
		case float64:
			n = nv
		case int32:
			n = float64(nv)
		case int64:
			n = float64(nv)
		default:
			return spec, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$expMovingAvg 'N' must be a number",
				"$setWindowFields (stage)",
			)
		}
		spec.alpha = 2.0 / (n + 1.0)
	} else {
		return spec, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$expMovingAvg requires 'N' or 'alpha' field",
			"$setWindowFields (stage)",
		)
	}

	return spec, nil
}

// parseDerivativeSpec parses the $derivative/$integral operator configuration.
func parseDerivativeSpec(doc *types.Document) (derivativeSpec, error) {
	spec := derivativeSpec{}

	inputVal, err := doc.Get("input")
	if err != nil {
		return spec, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$derivative/$integral requires 'input' field",
			"$setWindowFields (stage)",
		)
	}

	spec.inputExpr = inputVal

	if doc.Has("unit") {
		unitVal := must.NotFail(doc.Get("unit"))
		if s, ok := unitVal.(string); ok {
			spec.unit = s
		}
	}

	return spec, nil
}

// parseTopSpec parses the $top/$bottom operator configuration.
func parseTopSpec(doc *types.Document) (topSpec, error) {
	spec := topSpec{}

	sortByVal, err := doc.Get("sortBy")
	if err != nil {
		return spec, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$top/$bottom requires 'sortBy' field",
			"$setWindowFields (stage)",
		)
	}

	sortByDoc, ok := sortByVal.(*types.Document)
	if !ok {
		return spec, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"$top/$bottom 'sortBy' must be an object",
			"$setWindowFields (stage)",
		)
	}

	spec.sortBy = sortByDoc

	outputVal, err := doc.Get("output")
	if err != nil {
		return spec, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$top/$bottom requires 'output' field",
			"$setWindowFields (stage)",
		)
	}

	outputDoc, ok := outputVal.(*types.Document)
	if !ok {
		return spec, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"$top/$bottom 'output' must be an object",
			"$setWindowFields (stage)",
		)
	}

	spec.outputExpr = outputDoc

	return spec, nil
}

// evaluateExpr evaluates an expression against a document.
func evaluateExpr(expr any, doc *types.Document) any {
	switch e := expr.(type) {
	case string:
		aggExpr, err := aggregations.NewExpression(e, nil)
		if err != nil {
			return expr
		}

		val, err := aggExpr.Evaluate(doc)
		if err != nil {
			return types.Null
		}

		return val
	default:
		return expr
	}
}

// toFloat64 converts a numeric value to float64, returns (val, true) or (0, false).
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// computeCovariance computes population or sample covariance.
func computeCovariance(op string, spec covPairSpec, window []*types.Document) any {
	var xs, ys []float64

	for _, doc := range window {
		xv := evaluateExpr(spec.expr1, doc)
		yv := evaluateExpr(spec.expr2, doc)

		xf, xok := toFloat64(xv)
		yf, yok := toFloat64(yv)

		if xok && yok {
			xs = append(xs, xf)
			ys = append(ys, yf)
		}
	}

	n := len(xs)
	if n == 0 {
		return types.Null
	}

	if op == "$covarianceSamp" && n < 2 {
		return types.Null
	}

	var sumX, sumY, sumXY float64
	for i := range n {
		sumX += xs[i]
		sumY += ys[i]
		sumXY += xs[i] * ys[i]
	}

	meanX := sumX / float64(n)
	meanY := sumY / float64(n)

	cov := sumXY/float64(n) - meanX*meanY

	if op == "$covarianceSamp" {
		// Adjust for sample: multiply by n/(n-1).
		cov = cov * float64(n) / float64(n-1)
	}

	return cov
}

// computeEMA computes exponential moving average over a partition.
func computeEMA(spec emaSpec, partition []*types.Document) []any {
	n := len(partition)
	vals := make([]any, n)

	var prev float64
	hasPrev := false

	for i, doc := range partition {
		v := evaluateExpr(spec.inputExpr, doc)
		f, ok := toFloat64(v)

		if !ok {
			// Non-numeric: carry forward previous or null.
			if hasPrev {
				vals[i] = prev
			} else {
				vals[i] = types.Null
			}

			continue
		}

		if !hasPrev {
			vals[i] = f
			prev = f
			hasPrev = true
		} else {
			ema := spec.alpha*f + (1-spec.alpha)*prev
			vals[i] = ema
			prev = ema
		}
	}

	return vals
}

// computeDerivative computes $derivative over a window.
// Returns null when less than 2 docs in window.
func computeDerivative(spec derivativeSpec, window []*types.Document) any {
	if len(window) < 2 {
		return types.Null
	}

	first := window[0]
	last := window[len(window)-1]

	y1 := evaluateExpr(spec.inputExpr, first)
	y2 := evaluateExpr(spec.inputExpr, last)

	yf1, ok1 := toFloat64(y1)
	yf2, ok2 := toFloat64(y2)

	if !ok1 || !ok2 {
		return types.Null
	}

	// For numeric sort fields, use x values from the sort field.
	// Without a time field we use position index difference.
	dx := float64(len(window) - 1)

	if dx == 0 {
		return types.Null
	}

	return (yf2 - yf1) / dx
}

// computeIntegral computes $integral (trapezoidal) over a window.
func computeIntegral(spec derivativeSpec, window []*types.Document) any {
	if len(window) < 2 {
		return float64(0)
	}

	var total float64

	for i := 1; i < len(window); i++ {
		y1 := evaluateExpr(spec.inputExpr, window[i-1])
		y2 := evaluateExpr(spec.inputExpr, window[i])

		yf1, ok1 := toFloat64(y1)
		yf2, ok2 := toFloat64(y2)

		if !ok1 || !ok2 {
			continue
		}

		// Trapezoidal rule with dt=1 (unit position steps).
		total += (yf1 + yf2) / 2.0
	}

	return total
}

// computeLinearFill fills null values using linear interpolation.
func computeLinearFill(expr any, partition []*types.Document) []any {
	n := len(partition)
	vals := make([]any, n)

	// First pass: collect raw values.
	for i, doc := range partition {
		v := evaluateExpr(expr, doc)
		if _, isNull := v.(types.NullType); isNull {
			vals[i] = nil // sentinel for "needs fill"
		} else {
			vals[i] = v
		}
	}

	// Second pass: interpolate gaps.
	for i := range n {
		if vals[i] != nil {
			continue
		}

		// Find previous non-null.
		prevIdx := -1

		for j := i - 1; j >= 0; j-- {
			if vals[j] != nil {
				prevIdx = j
				break
			}
		}

		// Find next non-null.
		nextIdx := -1

		for j := i + 1; j < n; j++ {
			if vals[j] != nil {
				nextIdx = j
				break
			}
		}

		if prevIdx < 0 && nextIdx < 0 {
			vals[i] = types.Null
		} else if prevIdx < 0 {
			vals[i] = vals[nextIdx]
		} else if nextIdx < 0 {
			vals[i] = vals[prevIdx]
		} else {
			// Linear interpolation.
			pf, ok1 := toFloat64(vals[prevIdx])
			nf, ok2 := toFloat64(vals[nextIdx])

			if !ok1 || !ok2 {
				vals[i] = vals[prevIdx]
			} else {
				t := float64(i-prevIdx) / float64(nextIdx-prevIdx)
				vals[i] = pf + t*(nf-pf)
			}
		}
	}

	// Replace remaining nils with Null.
	for i := range n {
		if vals[i] == nil {
			vals[i] = types.Null
		}
	}

	return vals
}

// computeLocf fills null values using last observation carried forward.
func computeLocf(expr any, partition []*types.Document) []any {
	n := len(partition)
	vals := make([]any, n)

	var lastObserved any

	for i, doc := range partition {
		v := evaluateExpr(expr, doc)
		if _, isNull := v.(types.NullType); isNull {
			if lastObserved != nil {
				vals[i] = lastObserved
			} else {
				vals[i] = types.Null
			}
		} else {
			vals[i] = v
			lastObserved = v
		}
	}

	return vals
}

// computeTopBottom computes $top or $bottom — returns the output projection of the
// first document after sorting window by spec.sortBy.
// For $top: sort descending (highest first); but we respect whatever spec.sortBy says.
// For $bottom: sort ascending (lowest first); same — respect spec.sortBy.
// Both return the first element of the sorted window's output projection.
func computeTopBottom(op string, spec topSpec, window []*types.Document) (any, error) {
	if len(window) == 0 {
		return types.Null, nil
	}

	// Copy window so we can sort it.
	sorted := make([]*types.Document, len(window))
	copy(sorted, window)

	stdsort.SliceStable(sorted, func(i, j int) bool {
		a := sorted[i]
		b := sorted[j]

		if spec.sortBy == nil {
			return false
		}

		for _, key := range spec.sortBy.Keys() {
			order, _ := spec.sortBy.Get(key)

			aVal, _ := a.Get(key)
			bVal, _ := b.Get(key)

			if aVal == nil {
				aVal = types.Null
			}

			if bVal == nil {
				bVal = types.Null
			}

			cmp := types.CompareForAggregation(aVal, bVal)

			var ascending bool

			switch ov := order.(type) {
			case int32:
				ascending = ov >= 0
			case int64:
				ascending = ov >= 0
			case float64:
				ascending = ov >= 0
			default:
				ascending = true
			}

			if cmp == types.Equal {
				continue
			}

			if ascending {
				return cmp == types.Less
			}

			return cmp == types.Greater
		}

		return false
	})

	// Project output fields from the selected document.
	// Both $top and $bottom return sorted[0]: the caller uses sortBy to control which
	// end they want ($top with descending sortBy for the maximum, $bottom with ascending
	// sortBy for the minimum).
	selected := sorted[0]

	result := new(types.Document)

	if spec.outputExpr != nil {
		iter := spec.outputExpr.Iterator()
		defer iter.Close()

		for {
			k, v, err := iter.Next()
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}

			if err != nil {
				return nil, lazyerrors.Error(err)
			}

			projected := evaluateExpr(v, selected)
			result.Set(k, projected)
		}
	}

	return result, nil
}

// check interfaces
var (
	_ aggregations.Stage = (*setWindowFields)(nil)
)
