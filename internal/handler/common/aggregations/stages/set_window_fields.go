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
	"slices"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators/accumulators"
	"github.com/dolthub/dumbodb/internal/handler/commonpath"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

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

type windowFrame struct {
	frameType windowFrameType
	lower     windowBound
	upper     windowBound
	// numericRange is set for a range without a unit, whose sortBy values
	// must be numbers.
	numericRange bool
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
		if err := operators.CheckFieldPaths(pb); err != nil {
			return nil, err
		}
		if err := operators.CheckConstantSubexpressions(pb); err != nil {
			return nil, err
		}
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

	if err = operators.CheckFieldPaths(outputDoc); err != nil {
		return nil, err
	}
	if _, _, conflict := pathCollision(outputDoc.Keys()); conflict {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrWindowOutputPathCollision,
			"$setWindowFields 'output' specification contains two conflicting paths", "$setWindowFields (stage)")
	}
	for _, field := range outputDoc.Keys() {
		switch {
		case field == "":
			err = handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrEmptyFieldPath,
				"FieldPath cannot be constructed with empty string", "$setWindowFields (stage)")
		case strings.HasPrefix(field, "$"):
			err = handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFieldPathInvalidName,
				"Consider using $getField or $setField for a field path with '.' or '$'. :: caused by :: "+
					"FieldPath field names may not start with '$', given '"+strings.SplitN(field, ".", 2)[0]+"'.",
				"$setWindowFields (stage)")
		default:
			err = operators.CheckFieldPaths("$" + field)
		}
		if err != nil {
			return nil, err
		}
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

		if wof.frame != nil && wof.frame.frameType == windowFrameRange && swf.sortBy != nil && swf.sortBy.Len() > 0 {
			if order, _ := toFloat64(must.NotFail(swf.sortBy.Get(swf.sortBy.Keys()[0]))); order < 0 {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrRangeWindowDescendingSort,
					"Range-based bounds require an ascending sortBy",
					"$setWindowFields (stage)",
				)
			}
		}

		if wof.operator == "$linearFill" && (swf.sortBy == nil || swf.sortBy.Len() != 1) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrLinearFillSortBy,
				"$linearFill must be specified with a top level sortBy expression with exactly one element",
				"$setWindowFields (stage)",
			)
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
		if err = operators.CheckConstantSubexpressions(v); err != nil {
			return wof, err
		}

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
		case "$firstN", "$lastN", "$minN", "$maxN", "$top", "$bottom", "$topN", "$bottomN", "$median", "$percentile":
			acc, accErr := accumulators.NewAccumulator("$setWindowFields", field, must.NotFail(types.NewDocument(k, v)))
			if accErr != nil {
				return wof, accErr
			}
			wof.opSpec = acc
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
		frame.numericRange = !doc.Has("unit")

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

func (s *setWindowFields) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	// Collect all documents.
	docs, err := common.ConsumeDocuments(iter, "$setWindowFields")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Partition documents.
	partitions, err := s.partitionDocuments(docs)
	if err != nil {
		return nil, err
	}

	res := &windowPartitionsIterator{s: s, partitions: partitions}
	closer.Add(res)

	return res, nil
}

// windowPartitionsIterator computes one partition's window outputs at a time,
// and array-valued outputs only as each document is emitted, as mongod does.
type windowPartitionsIterator struct {
	s          *setWindowFields
	partitions [][]*types.Document
	partition  []*types.Document
	processed  []*types.Document
	next       int
}

func (it *windowPartitionsIterator) Next() (struct{}, *types.Document, error) {
	var unused struct{}
	for it.next >= len(it.processed) {
		if len(it.partitions) == 0 {
			return unused, nil, iterator.ErrIteratorDone
		}
		partition := it.partitions[0]
		it.partitions[0] = nil
		it.partitions = it.partitions[1:]

		if it.s.sortBy != nil && it.s.sortBy.Len() > 0 {
			if err := common.SortDocuments(partition, it.s.sortBy); err != nil {
				return unused, nil, lazyerrors.Error(err)
			}
		}
		processed, err := it.s.applyWindowFunctions(partition)
		if err != nil {
			return unused, nil, err
		}
		it.partition, it.processed, it.next = partition, processed, 0
	}

	i := it.next
	doc := it.processed[i]
	it.processed[i] = nil
	it.next++
	for _, wof := range it.s.outputs {
		if !deferredWindowOutput(wof.operator) {
			continue
		}
		val, err := it.s.windowValueAt(it.partition, wof, i)
		if err != nil {
			return unused, nil, err
		}
		if doc, err = setWindowOutput(doc, wof.outputField, val); err != nil {
			return unused, nil, err
		}
	}
	return unused, doc, nil
}

func (it *windowPartitionsIterator) Close() {
	it.partitions, it.partition, it.processed = nil, nil, nil
}

// deferredWindowOutput reports whether operator's results are computed as each
// document is emitted, because they are arrays that may be large.
func deferredWindowOutput(operator string) bool {
	switch operator {
	case "$push", "$addToSet", "$firstN", "$lastN", "$minN", "$maxN", "$topN", "$bottomN", "$percentile":
		return true
	}
	return false
}

// accumulateWindow applies acc to a window. mongod computes a window whose
// lower edge is bounded with accumulators that keep every value.
func accumulateWindow(acc accumulators.Accumulator, frame *windowFrame, window []*types.Document) (any, error) {
	iter := iterator.Values(iterator.ForSlice(window))
	if frame != nil && !frame.lower.unbounded {
		return accumulators.AccumulateWindow(acc, iter)
	}
	return accumulators.Accumulate(acc, iter)
}

// windowValueAt computes a deferred window output for the document at index i
// of the partition.
func (s *setWindowFields) windowValueAt(partition []*types.Document, wof windowOutputField, i int) (any, error) {
	window := s.windowDocs(wof.frame, i, partition)
	if acc, ok := wof.opSpec.(accumulators.Accumulator); ok {
		return accumulateWindow(acc, wof.frame, window)
	}
	return applyAccumulator(wof.operator, wof.expression, window)
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
			return nil, err
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
	val, missing, err := operators.EvalOperand(s.partitionBy, doc)
	if err != nil {
		return nil, err
	}
	if missing {
		return types.Null, nil
	}
	if _, isArray := val.(*types.Array); isArray {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
			"An expression used to partition cannot evaluate to value of type array", "$setWindowFields (stage)")
	}
	return val, nil
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

	// mongod checks each document's $linearFill inputs before it interpolates.
	for _, wof := range s.outputs {
		if wof.operator == "$linearFill" {
			if _, _, err := linearFillValues(wof.expression, partition); err != nil {
				return nil, err
			}
		}
	}

	for _, wof := range s.outputs {
		if deferredWindowOutput(wof.operator) {
			for i := range result {
				var err error
				if result[i], err = setWindowOutput(result[i], wof.outputField, types.Null); err != nil {
					return nil, err
				}
			}
			continue
		}
		vals, err := s.computeWindowValues(partition, wof)
		if err != nil {
			return nil, err
		}

		for i, val := range vals {
			if result[i], err = setWindowOutput(result[i], wof.outputField, val); err != nil {
				return nil, err
			}
		}
	}

	return result, nil
}

// windowMissingValue is a window output of mongod's missing value, which
// leaves the output field out.
type windowMissingValue struct{}

// setWindowOutput sets the output field of doc to val, a dotted field as
// $addFields would set it.
func setWindowOutput(doc *types.Document, field string, val any) (*types.Document, error) {
	_, missing := val.(windowMissingValue)
	if !strings.Contains(field, ".") {
		if missing {
			doc.Remove(field)
		} else {
			doc.Set(field, val)
		}
		return doc, nil
	}
	var expr any = "$$REMOVE"
	if !missing {
		expr = must.NotFail(types.NewDocument("$literal", val))
	}
	spec := must.NotFail(types.NewDocument(field, expr))
	return operators.AddFieldsTree(doc, spec)
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

	if wof.frame != nil && wof.frame.numericRange {
		if err := s.checkNumericRangeSortBy(partition); err != nil {
			return nil, err
		}
	}

	switch wof.operator {
	case "$documentNumber":
		for i := range partition {
			vals[i] = windowCount(i + 1)
		}

	case "$rank":
		vals = computeRank(partition, s.sortBy, false)

	case "$denseRank":
		vals = computeRank(partition, s.sortBy, true)

	case "$sum", "$avg", "$min", "$max", "$first", "$last", "$push", "$addToSet",
		"$stdDevPop", "$stdDevSamp":
		if (wof.operator == "$sum" || wof.operator == "$avg") && wof.frame != nil && !wof.frame.lower.unbounded {
			return s.removableSumValues(partition, wof)
		}

		for i := range partition {
			window := s.windowDocs(wof.frame, i, partition)

			val, err := applyAccumulator(wof.operator, wof.expression, window)
			if err != nil {
				return nil, err
			}

			vals[i] = val
		}

	case "$count":
		for i := range partition {
			window := s.windowDocs(wof.frame, i, partition)
			vals[i] = windowCount(len(window))
		}

	case "$firstN", "$lastN", "$minN", "$maxN", "$top", "$bottom", "$topN", "$bottomN", "$median", "$percentile":
		acc := wof.opSpec.(accumulators.Accumulator)
		for i := range partition {
			window := s.windowDocs(wof.frame, i, partition)
			val, err := accumulateWindow(acc, wof.frame, window)
			if err != nil {
				return nil, err
			}
			vals[i] = val
		}

	case "$covariancePop", "$covarianceSamp":
		spec := wof.opSpec.(covPairSpec)
		for i := range partition {
			window := s.windowDocs(wof.frame, i, partition)
			val, err := computeCovariance(wof.operator, spec, window)
			if err != nil {
				return nil, err
			}
			vals[i] = val
		}

	case "$shift":
		spec := wof.opSpec.(shiftSpec)
		for i := range partition {
			idx := i + int(spec.by)
			if idx < 0 || idx >= n {
				vals[i] = spec.defaultVal
			} else {
				val, missing, err := operators.EvalOperand(spec.outputExpr, partition[idx])
				if err != nil {
					return nil, err
				}
				if missing {
					val = spec.defaultVal
				}
				vals[i] = val
			}
		}

	case "$expMovingAvg":
		spec := wof.opSpec.(emaSpec)
		var err error
		if vals, err = computeEMA(spec, partition); err != nil {
			return nil, err
		}

	case "$derivative":
		spec := wof.opSpec.(derivativeSpec)
		if spec.unit != "" {
			if err := s.validateSortByFieldIsDate(partition); err != nil {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrWindowDerivativeUnitRequiresDate,
					"PlanExecutor error during aggregation :: caused by :: $derivative with 'unit' expects the sortBy field to be a Date",
					"$setWindowFields (stage)",
				)
			}
		}
		for i := range partition {
			lo, hi := s.windowBounds(wof.frame, i, n)
			window := partition[lo:hi]
			val, err := computeDerivative(spec, window)
			if err != nil {
				return nil, err
			}
			vals[i] = val
		}

	case "$integral":
		spec := wof.opSpec.(derivativeSpec)
		if spec.unit != "" {
			if err := s.validateSortByFieldIsDate(partition); err != nil {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrWindowIntegralUnitRequiresDate,
					"PlanExecutor error during aggregation :: caused by :: $integral with 'unit' expects the sortBy field to be a Date",
					"$setWindowFields (stage)",
				)
			}
		}
		for i := range partition {
			lo, hi := s.windowBounds(wof.frame, i, n)
			window := partition[lo:hi]
			val, err := computeIntegral(spec, window)
			if err != nil {
				return nil, err
			}
			vals[i] = val
		}

	case "$linearFill":
		if err := s.validateSortByFieldIsNumericOrDate(partition); err != nil {
			return nil, err
		}
		var err error
		if vals, err = s.computeLinearFill(wof.expression, partition); err != nil {
			return nil, err
		}

	case "$locf":
		var err error
		if vals, err = computeLocf(wof.expression, partition); err != nil {
			return nil, err
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

// windowDocs returns the slice of documents in the window for document at position i.
// For range frames it uses value-based bounds; for document frames it uses index bounds.
func (s *setWindowFields) windowDocs(frame *windowFrame, i int, partition []*types.Document) []*types.Document {
	n := len(partition)

	if frame != nil && frame.frameType == windowFrameRange {
		return s.windowDocsRange(frame, i, partition)
	}

	lo, hi := s.windowBounds(frame, i, n)

	return partition[lo:hi]
}

// windowSpan returns the window of the document at position i as partition
// indexes [lo, hi); lo == hi for an empty window.
func (s *setWindowFields) windowSpan(frame *windowFrame, i int, partition []*types.Document) (int, int) {
	if frame.frameType != windowFrameRange {
		return s.windowBounds(frame, i, len(partition))
	}

	docs := s.windowDocsRange(frame, i, partition)
	if len(docs) == 0 {
		return 0, 0
	}

	lo := slices.Index(partition, docs[0])

	return lo, lo + len(docs)
}

// removableSumValues computes $sum or $avg over windows with a lower bound as
// mongod does: one running state per partition, each document added as it
// enters the window and removed as it leaves, adds first.
func (s *setWindowFields) removableSumValues(partition []*types.Document, wof windowOutputField) ([]any, error) {
	vals := make([]any, len(partition))
	inputs := make([]any, len(partition))
	present := make([]bool, len(partition))
	sum := aggregations.NewRemovableSum()

	var lo, hi int
	for i := range partition {
		wlo, whi := s.windowSpan(wof.frame, i, partition)
		if whi <= wlo {
			wlo, whi = hi, hi
		}

		for j := max(hi, wlo); j < whi; j++ {
			v, missing, err := windowInput(wof.expression, partition[j])
			if err != nil {
				return nil, err
			}
			if !missing {
				inputs[j], present[j] = v, true
				sum.Add(v)
			}
		}

		for j := lo; j < min(wlo, hi); j++ {
			if present[j] {
				sum.Remove(inputs[j])
				inputs[j] = nil
			}
		}

		lo, hi = wlo, whi

		if wof.operator == "$avg" {
			vals[i] = sum.Avg()
		} else {
			vals[i] = sum.Sum()
		}
	}

	return vals, nil
}

// windowInput evaluates a window function's input for one document; no
// expression counts the document as 1.
func windowInput(expression any, doc *types.Document) (any, bool, error) {
	if expression == nil {
		return int32(1), false, nil
	}
	return operators.EvalOperand(expression, doc)
}

// windowDocsRange computes the window slice for a range-based frame at position i.
// The frame bounds are value offsets from the current document's sort field value.
func (s *setWindowFields) windowDocsRange(frame *windowFrame, i int, partition []*types.Document) []*types.Document {
	if s.sortBy == nil || s.sortBy.Len() == 0 {
		return partition
	}

	sortKey := s.sortBy.Keys()[0]
	currentVal := sortByValue(partition[i], sortKey)

	currentFloat, ok := toFloat64(currentVal)
	if !ok {
		// Fall back to index-based bounds for non-numeric sort fields.
		lo, hi := s.windowBounds(frame, i, len(partition))
		return partition[lo:hi]
	}

	loUnbounded := frame.lower.unbounded
	var loBound float64

	if !loUnbounded {
		if frame.lower.current {
			loBound = currentFloat
		} else {
			loBound = currentFloat + float64(frame.lower.offset)
		}
	}

	hiUnbounded := frame.upper.unbounded
	var hiBound float64

	if !hiUnbounded {
		if frame.upper.current {
			hiBound = currentFloat
		} else {
			hiBound = currentFloat + float64(frame.upper.offset)
		}
	}

	var result []*types.Document

	for _, doc := range partition {
		val := sortByValue(doc, sortKey)

		f, ok := toFloat64(val)
		if !ok {
			continue
		}

		if !loUnbounded && f < loBound {
			continue
		}

		if !hiUnbounded && f > hiBound {
			continue
		}

		result = append(result, doc)
	}

	return result
}

// validateSortByFieldIsDate checks that the first sort-by field value in the partition is a Date.
// Returns a non-nil error if the partition is non-empty and the field is not a time.Time.
func (s *setWindowFields) validateSortByFieldIsDate(partition []*types.Document) error {
	if len(partition) == 0 || s.sortBy == nil || s.sortBy.Len() == 0 {
		return nil
	}

	sortKey := s.sortBy.Keys()[0]
	val := sortByValue(partition[0], sortKey)

	if _, ok := val.(time.Time); !ok {
		return fmt.Errorf("not a date")
	}

	return nil
}

// bsonTypeName returns the MongoDB type name for a value (used in error messages).
func bsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "missing"
	case string:
		return "string"
	case float64:
		return "double"
	case int32:
		return "int"
	case int64:
		return "long"
	case bool:
		return "bool"
	case time.Time:
		return "date"
	case *types.Document:
		return "object"
	case *types.Array:
		return "array"
	case types.NullType:
		return "null"
	default:
		return "unknown"
	}
}

// checkNumericRangeSortBy returns mongod's error for a document whose sortBy
// value is not a number, which a range window without a unit requires.
func (s *setWindowFields) checkNumericRangeSortBy(partition []*types.Document) error {
	if s.sortBy == nil || s.sortBy.Len() == 0 {
		return nil
	}
	key := s.sortBy.Keys()[0]
	for _, doc := range partition {
		val := sortByValue(doc, key)
		typeName := "missing"
		switch val.(type) {
		case float64, int32, int64, types.Decimal128:
			continue
		case nil:
		default:
			typeName = handlerparams.AliasFromType(val)
		}
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRangeSortByNotNumeric,
			"Invalid range: Expected the sortBy field to be a number, but it was "+typeName, "$setWindowFields (stage)")
	}
	return nil
}

// validateSortByFieldIsNumericOrDate checks that the sort-by field values are numeric or Date.
// Returns an error matching MongoDB's TypeMismatch error if they are not.
func (s *setWindowFields) validateSortByFieldIsNumericOrDate(partition []*types.Document) error {
	if len(partition) == 0 || s.sortBy == nil || s.sortBy.Len() == 0 {
		return nil
	}

	sortKey := s.sortBy.Keys()[0]

	for _, doc := range partition {
		val := sortByValue(doc, sortKey)

		switch val.(type) {
		case float64, int32, int64, types.Decimal128, time.Time:
			// valid
		default:
			typeName := bsonTypeName(val)
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"PlanExecutor error during aggregation :: caused by :: Value of the sortBy field must be numeric or a date, but found "+typeName,
				"$setWindowFields (stage)",
			)
		}
	}

	return nil
}

// windowCount returns a rank, document number or count the way mongod
// emits it: int32, widening to int64 only past the int32 range.
func windowCount(n int) any {
	if n <= math.MaxInt32 {
		return int32(n)
	}
	return int64(n)
}

// computeRank computes $rank or $denseRank values for all documents in a partition.
func computeRank(partition []*types.Document, sortBy *types.Document, dense bool) []any {
	n := len(partition)
	vals := make([]any, n)

	if n == 0 {
		return vals
	}

	denseRankVal := 1
	vals[0] = windowCount(1)

	for i := 1; i < n; i++ {
		tied := isTied(partition[i-1], partition[i], sortBy)

		if tied {
			if dense {
				vals[i] = windowCount(denseRankVal)
			} else {
				vals[i] = vals[i-1]
			}
		} else {
			denseRankVal++

			if dense {
				vals[i] = windowCount(denseRankVal)
			} else {
				// i+1 because rank is 1-based and i is 0-based current position.
				vals[i] = windowCount(i + 1)
			}
		}
	}

	return vals
}

// sortByValue returns the value at the sortBy path key, or nil when missing.
func sortByValue(doc *types.Document, key string) any {
	path, err := types.NewPathFromString(key)
	if err != nil {
		return nil
	}
	v, _ := doc.GetByPath(path)
	return v
}

// isTied returns true if two documents have the same sort key values.
func isTied(a, b *types.Document, sortBy *types.Document) bool {
	if sortBy == nil || sortBy.Len() == 0 {
		return true
	}

	// Documents tie when $sort would not order them: an array compares by
	// its least or greatest element, as sortBy directs.
	for _, key := range sortBy.Keys() {
		path, err := types.NewPathFromString(key)
		if err != nil {
			return false
		}
		order := types.Ascending
		if dir, ok := toFloat64(must.NotFail(sortBy.Get(key))); ok && dir < 0 {
			order = types.Descending
		}
		if common.CompareSortKeys(commonpath.SortKeyValue(a, path), commonpath.SortKeyValue(b, path), order) != types.Equal {
			return false
		}
	}

	return true
}

// applyAccumulator applies a window function without its own state, such as
// $sum, $push or $stdDevPop, over a window slice of documents.
func applyAccumulator(op string, expression any, window []*types.Document) (any, error) {
	switch op {
	case "$first", "$last":
		// The value of the first or last document, null when it is missing.
		if len(window) == 0 {
			return types.Null, nil
		}
		doc := window[0]
		if op == "$last" {
			doc = window[len(window)-1]
		}
		v, missing, err := operators.EvalOperand(expression, doc)
		if err != nil || missing {
			return types.Null, err
		}
		return v, nil
	}

	// Collect values from window documents by evaluating the expression.
	values, err := collectWindowValues(expression, window)
	if err != nil {
		return nil, err
	}

	switch op {
	case "$sum":
		sum := aggregations.NewNumberSum()
		for _, v := range values {
			sum.Add(v)
		}
		return sum.Result(), nil
	case "$avg":
		avg := aggregations.NewNumberAvg()
		for _, v := range values {
			avg.Add(v)
		}
		return avg.Result(), nil
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
		size := 0
		for _, v := range values {
			if size += types.BSONValueSize(v); size > windowAccumulatorMemoryLimit {
				return nil, windowMemoryError(op)
			}
			arr.Append(v)
		}
		return arr, nil
	case "$addToSet":
		arr := types.MakeArray(0)
		size := 0
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
				if size += types.BSONValueSize(v); size > windowAccumulatorMemoryLimit {
					return nil, windowMemoryError(op)
				}
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

// collectWindowValues evaluates an expression against each document in the
// window, leaving out missing values.
func collectWindowValues(expression any, window []*types.Document) ([]any, error) {
	values := make([]any, 0, len(window))
	for _, doc := range window {
		val, missing, err := windowInput(expression, doc)
		if err != nil {
			return nil, err
		}
		if !missing {
			values = append(values, val)
		}
	}
	return values, nil
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

	spec.defaultVal = types.Null
	if doc.Has("default") {
		defVal := must.NotFail(doc.Get("default"))
		if defVal != "$$REMOVE" && !operators.IsConstantExpression(defVal) {
			return spec, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				"'$shift:default' expression must yield a constant value.",
				"$setWindowFields (stage)",
			)
		}
		v, missing, err := operators.EvalOperand(defVal, types.MakeDocument(0))
		if err != nil {
			return spec, err
		}
		spec.defaultVal = v
		if missing {
			spec.defaultVal = windowMissingValue{}
		}
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

// evaluateExpr evaluates an expression against a document; a missing value
// is null.
func evaluateExpr(expr any, doc *types.Document) (any, error) {
	val, missing, err := operators.EvalOperand(expr, doc)
	if err != nil {
		return nil, err
	}
	if missing {
		return types.Null, nil
	}
	return val, nil
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
func computeCovariance(op string, spec covPairSpec, window []*types.Document) (any, error) {
	var xs, ys []float64

	for _, doc := range window {
		xv, err := evaluateExpr(spec.expr1, doc)
		if err != nil {
			return nil, err
		}
		yv, err := evaluateExpr(spec.expr2, doc)
		if err != nil {
			return nil, err
		}

		xf, xok := toFloat64(xv)
		yf, yok := toFloat64(yv)

		if xok && yok {
			xs = append(xs, xf)
			ys = append(ys, yf)
		}
	}

	n := len(xs)
	if n == 0 {
		return types.Null, nil
	}

	if op == "$covarianceSamp" && n < 2 {
		return types.Null, nil
	}

	var sumX, sumY float64
	for i := range n {
		sumX += xs[i]
		sumY += ys[i]
	}

	meanX := sumX / float64(n)
	meanY := sumY / float64(n)

	// Two-pass formula for numerical stability.
	var cov float64
	for i := range n {
		cov += (xs[i] - meanX) * (ys[i] - meanY)
	}

	cov /= float64(n)

	if op == "$covarianceSamp" {
		// Adjust for sample: multiply by n/(n-1).
		cov = cov * float64(n) / float64(n-1)
	}

	return cov, nil
}

// computeEMA computes exponential moving average over a partition.
func computeEMA(spec emaSpec, partition []*types.Document) ([]any, error) {
	n := len(partition)
	vals := make([]any, n)

	var prev float64
	hasPrev := false

	for i, doc := range partition {
		v, err := evaluateExpr(spec.inputExpr, doc)
		if err != nil {
			return nil, err
		}
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

	return vals, nil
}

// computeDerivative computes $derivative over a window.
// Returns null when less than 2 docs in window.
func computeDerivative(spec derivativeSpec, window []*types.Document) (any, error) {
	if len(window) < 2 {
		return types.Null, nil
	}

	first := window[0]
	last := window[len(window)-1]

	y1, err := evaluateExpr(spec.inputExpr, first)
	if err != nil {
		return nil, err
	}
	y2, err := evaluateExpr(spec.inputExpr, last)
	if err != nil {
		return nil, err
	}

	yf1, ok1 := toFloat64(y1)
	yf2, ok2 := toFloat64(y2)

	if !ok1 || !ok2 {
		return types.Null, nil
	}

	// For numeric sort fields, use x values from the sort field.
	// Without a time field we use position index difference.
	dx := float64(len(window) - 1)

	if dx == 0 {
		return types.Null, nil
	}

	return (yf2 - yf1) / dx, nil
}

// computeIntegral computes $integral (trapezoidal) over a window.
func computeIntegral(spec derivativeSpec, window []*types.Document) (any, error) {
	if len(window) < 2 {
		return float64(0), nil
	}

	var total float64

	for i := 1; i < len(window); i++ {
		y1, err := evaluateExpr(spec.inputExpr, window[i-1])
		if err != nil {
			return nil, err
		}
		y2, err := evaluateExpr(spec.inputExpr, window[i])
		if err != nil {
			return nil, err
		}

		yf1, ok1 := toFloat64(y1)
		yf2, ok2 := toFloat64(y2)

		if !ok1 || !ok2 {
			continue
		}

		// Trapezoidal rule with dt=1 (unit position steps).
		total += (yf1 + yf2) / 2.0
	}

	return total, nil
}

// computeLinearFill fills null and missing values by interpolating linearly,
// by the sortBy value, between the nearest values on either side; a value
// with none on one side stays null.
func (s *setWindowFields) computeLinearFill(expr any, partition []*types.Document) ([]any, error) {
	if err := s.validateSortByFieldIsNumericOrDate(partition); err != nil {
		return nil, err
	}
	sortKey := s.sortBy.Keys()[0]
	xs := make([]any, len(partition))
	for i, doc := range partition {
		xs[i] = sortByValue(doc, sortKey)
		if i > 0 && types.CompareForAggregation(xs[i], xs[i-1]) == types.Equal {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrLinearFillRepeatedSortValue,
				"There can be no repeated values in the sort field in the same partition",
				"$setWindowFields (stage)",
			)
		}
	}

	vals, known, err := linearFillValues(expr, partition)
	if err != nil {
		return nil, err
	}

	prev := -1
	for i := range vals {
		if known[i] {
			prev = i
			continue
		}
		next := i + 1
		for next < len(vals) && !known[next] {
			next++
		}
		if prev < 0 || next == len(vals) {
			continue
		}
		if vals[i], err = interpolate(xs[prev], vals[prev], xs[next], vals[next], xs[i]); err != nil {
			return nil, err
		}
	}
	return vals, nil
}

// interpolate returns y1 + (y2-y1)/(x2-x1) * (x-x1), computed with mongod's
// expression arithmetic so the result has the type mongod gives it.
func interpolate(x1, y1, x2, y2, x any) (any, error) {
	lit := func(v any) *types.Document { return must.NotFail(types.NewDocument("$literal", v)) }
	op := func(name string, args ...any) *types.Document {
		return must.NotFail(types.NewDocument(name, must.NotFail(types.NewArray(args...))))
	}
	slope := op("$divide", op("$subtract", lit(y2), lit(y1)), op("$subtract", lit(x2), lit(x1)))
	expr := op("$add", lit(y1), op("$multiply", slope, op("$subtract", lit(x), lit(x1))))
	v, _, err := operators.EvalOperand(expr, types.MakeDocument(0))
	return v, err
}

// linearFillValues evaluates a $linearFill input for each document,
// reporting which values are known numbers.
func linearFillValues(expr any, partition []*types.Document) ([]any, []bool, error) {
	vals := make([]any, len(partition))
	known := make([]bool, len(partition))
	for i, doc := range partition {
		v, err := evaluateExpr(expr, doc)
		if err != nil {
			return nil, nil, err
		}
		switch v.(type) {
		case types.NullType:
			vals[i] = types.Null
		case int32, int64, float64, types.Decimal128:
			vals[i], known[i] = v, true
		default:
			return nil, nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"Value to be filled must be numeric or nullish, but found "+handlerparams.AliasFromType(v),
				"$setWindowFields (stage)",
			)
		}
	}
	return vals, known, nil
}

// computeLocf fills null values using last observation carried forward.
func computeLocf(expr any, partition []*types.Document) ([]any, error) {
	n := len(partition)
	vals := make([]any, n)

	var lastObserved any

	for i, doc := range partition {
		v, err := evaluateExpr(expr, doc)
		if err != nil {
			return nil, err
		}
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

	return vals, nil
}

var (
	_ aggregations.Stage = (*setWindowFields)(nil)
)

// windowAccumulatorMemoryLimit is mongod's limit on the values a window
// function keeps.
const windowAccumulatorMemoryLimit = 100 * 1024 * 1024

func windowMemoryError(op string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrExceededMemoryLimit,
		fmt.Sprintf("%s used too much memory and cannot spill to disk. Memory limit: %d bytes", op, windowAccumulatorMemoryLimit),
		"$setWindowFields (stage)")
}
