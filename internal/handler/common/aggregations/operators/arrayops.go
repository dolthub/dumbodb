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

package operators

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// collectArray evaluates the input argument of $map or $reduce to an array.
// It reports a null or missing input as null.
func collectArray(arg any, doc *types.Document, opName string) (*types.Array, bool, error) {
	v, err := evalArgValue(arg, doc)
	if err != nil {
		return nil, false, err
	}

	if isNullish(v) {
		return nil, true, nil
	}

	arr, ok := v.(*types.Array)
	if !ok {
		if opName == "$reduce" {
			return nil, false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrReduceInputNotArray,
				"$reduce requires that 'input' be an array, found: "+types.FormatAnyValue(v), opName)
		}
		return nil, false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrMapInputNotArray,
			"input to $map must be an array not "+handlerparams.AliasFromType(v), opName)
	}

	return arr, false, nil
}

// arrayToSlice drains a *types.Array into a []any.
func arrayToSlice(arr *types.Array) ([]any, error) {
	iter := arr.Iterator()
	defer iter.Close()

	var result []any

	for {
		_, v, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, err
		}

		result = append(result, v)
	}

	return result, nil
}

// sliceToArray creates a *types.Array from a []any.
func sliceToArray(items []any) *types.Array {
	arr := types.MakeArray(len(items))
	for _, item := range items {
		arr.Append(item)
	}

	return arr
}

type sizeOp struct{ arg any }

func newSize(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$size",
			fmt.Sprintf("Expression $size takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &sizeOp{arg: args[0]}, nil
}

func (op *sizeOp) Process(doc *types.Document) (any, error) {
	v, missing, err := evalArgValueOrMissing(op.arg, doc)
	if err != nil {
		return nil, err
	}

	arr, ok := v.(*types.Array)
	if !ok || missing {
		typeName := "missing"
		if !missing {
			typeName = handlerparams.AliasFromType(v)
		}
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSizeNotArray,
			"The argument to $size must be an array, but was of type: "+typeName, "$size")
	}

	return int32(arr.Len()), nil
}

var _ Operator = (*sizeOp)(nil)

type isArrayOp struct{ arg any }

func newIsArray(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$isArray",
			fmt.Sprintf("Expression $isArray takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &isArrayOp{arg: args[0]}, nil
}

func (op *isArrayOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	_, ok := v.(*types.Array)

	return ok, nil
}

var _ Operator = (*isArrayOp)(nil)

// arrayElemAtOp represents { $arrayElemAt: [ <array>, <idx> ] }.
type arrayElemAtOp struct {
	arrayArg any
	idxArg   any
}

func newArrayElemAt(args ...any) (Operator, error) {
	if len(args) != 2 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$arrayElemAt",
			fmt.Sprintf("Expression $arrayElemAt takes exactly 2 arguments. %d were passed in.", len(args)))
	}

	return &arrayElemAtOp{arrayArg: args[0], idxArg: args[1]}, nil
}

func (op *arrayElemAtOp) Process(doc *types.Document) (any, error) {
	arrV, err := evalArgValue(op.arrayArg, doc)
	if err != nil {
		return nil, err
	}
	idxV, err := evalArgValue(op.idxArg, doc)
	if err != nil {
		return nil, err
	}
	if isNullish(arrV) || isNullish(idxV) {
		return types.Null, nil
	}

	arr, ok := arrV.(*types.Array)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrArrayElemAtNotArray,
			"$arrayElemAt's first argument must be an array, but is "+handlerparams.AliasFromType(arrV), "$arrayElemAt")
	}
	if !isNumber(idxV) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrArrayElemAtNotNumeric,
			"$arrayElemAt's second argument must be a numeric value, but is "+handlerparams.AliasFromType(idxV), "$arrayElemAt")
	}
	idx, ok := integralInt(idxV)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrArrayElemAtNotInt,
			"$arrayElemAt's second argument must be representable as a 32-bit integer: "+doubleString(idxV), "$arrayElemAt")
	}

	n := arr.Len()
	if idx < 0 {
		idx += n
	}
	// MongoDB's $arrayElemAt is missing, not null, for an index out of range.
	if idx < 0 || idx >= n {
		return nil, aggregations.ErrMissingValue
	}
	return must.NotFail(arr.Get(idx)), nil
}

var _ Operator = (*arrayElemAtOp)(nil)

type concatArraysOp struct{ args []any }

func newConcatArrays(args ...any) (Operator, error) {
	return &concatArraysOp{args: args}, nil
}

// Process follows ExpressionConcatArrays::evaluate: arguments are checked
// left to right, and the first nullish one makes the result null and the
// first non-array is an error.
func (op *concatArraysOp) Process(doc *types.Document) (any, error) {
	var result []any

	for _, arg := range op.args {
		v, err := evalArgValue(arg, doc)
		if errors.Is(err, aggregations.ErrMissingValue) {
			return types.Null, nil
		}
		if err != nil {
			return nil, err
		}
		if isNullish(v) {
			return types.Null, nil
		}
		arr, ok := v.(*types.Array)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrConcatArraysNotArray,
				fmt.Sprintf("$concatArrays only supports arrays, not %s", handlerparams.AliasFromType(v)), "$concatArrays")
		}

		items, err := arrayToSlice(arr)
		if err != nil {
			return nil, err
		}

		result = append(result, items...)
	}

	return sliceToArray(result), nil
}

var _ Operator = (*concatArraysOp)(nil)

type reverseArrayOp struct{ arg any }

func newReverseArray(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$reverseArray",
			fmt.Sprintf("Expression $reverseArray takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &reverseArrayOp{arg: args[0]}, nil
}

func (op *reverseArrayOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(v) {
		return types.Null, nil
	}

	arr, ok := v.(*types.Array)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrReverseArrayNotArray,
			"The argument to $reverseArray must be an array, but was of type: "+handlerparams.AliasFromType(v), "$reverseArray")
	}

	items, err := arrayToSlice(arr)
	if err != nil {
		return nil, err
	}

	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}

	return sliceToArray(items), nil
}

var _ Operator = (*reverseArrayOp)(nil)

// sliceOp represents { $slice: [ <array>, <n> ] } or { $slice: [ <array>, <position>, <n> ] }.
type sliceOp struct {
	arrayArg    any
	positionArg any // nil for 2-arg form
	nArg        any
}

func newSlice(args ...any) (Operator, error) {
	switch len(args) {
	case 2:
		return &sliceOp{arrayArg: args[0], nArg: args[1]}, nil
	case 3:
		return &sliceOp{arrayArg: args[0], positionArg: args[1], nArg: args[2]}, nil
	default:
		return nil, newOperatorError(ErrArgsInvalidLen, "$slice",
			fmt.Sprintf("Expression $slice takes 2 or 3 arguments. %d were passed in.", len(args)))
	}
}

func (op *sliceOp) Process(doc *types.Document) (any, error) {
	arrV, err := evalArgValue(op.arrayArg, doc)
	if err != nil {
		return nil, err
	}
	second := op.nArg
	if op.positionArg != nil {
		second = op.positionArg
	}
	secondV, err := evalArgValue(second, doc)
	if err != nil {
		return nil, err
	}
	if isNullish(arrV) || isNullish(secondV) {
		return types.Null, nil
	}

	arr, ok := arrV.(*types.Array)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSliceFirstArg,
			"First argument to $slice must be an array, but is of type: "+handlerparams.AliasFromType(arrV), "$slice")
	}
	if !isNumber(secondV) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSliceSecondNotNumeric,
			"Second argument to $slice must be a numeric value, but is of type: "+handlerparams.AliasFromType(secondV), "$slice")
	}
	secondInt, ok := integralInt(secondV)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSliceSecondNotInt,
			"Second argument to $slice can't be represented as a 32-bit integer: "+doubleString(secondV), "$slice")
	}

	items, err := arrayToSlice(arr)
	if err != nil {
		return nil, err
	}
	n := len(items)
	start, end := 0, n

	if op.positionArg == nil {
		if secondInt >= 0 {
			end = min(n, secondInt)
		} else {
			start = max(0, n+secondInt)
		}
		return sliceToArray(items[start:end]), nil
	}

	if secondInt < 0 {
		start = max(0, n+secondInt)
	} else {
		start = min(n, secondInt)
	}

	countV, err := evalArgValue(op.nArg, doc)
	if err != nil {
		return nil, err
	}
	if isNullish(countV) {
		return types.Null, nil
	}
	if !isNumber(countV) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSliceThirdNotNumeric,
			"Third argument to $slice must be numeric, but is of type: "+handlerparams.AliasFromType(countV), "$slice")
	}
	count, ok := integralInt(countV)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSliceThirdNotInt,
			"Third argument to $slice can't be represented as a 32-bit integer: "+doubleString(countV), "$slice")
	}
	if count <= 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSliceThirdArgNotPositive,
			fmt.Sprintf("Third argument to $slice must be positive: %d", count), "$slice")
	}
	end = min(start+count, n)
	return sliceToArray(items[start:end]), nil
}

var _ Operator = (*sliceOp)(nil)

// indexOfArrayOp represents { $indexOfArray: [ <array>, <search-expr>, <start>, <end> ] }.
type indexOfArrayOp struct {
	arrayArg  any
	searchArg any
	startArg  any
	endArg    any
	collated
}

func newIndexOfArray(args ...any) (Operator, error) {
	switch len(args) {
	case 2:
		return &indexOfArrayOp{arrayArg: args[0], searchArg: args[1]}, nil
	case 3:
		return &indexOfArrayOp{arrayArg: args[0], searchArg: args[1], startArg: args[2]}, nil
	case 4:
		return &indexOfArrayOp{arrayArg: args[0], searchArg: args[1], startArg: args[2], endArg: args[3]}, nil
	default:
		return nil, newOperatorError(ErrArgsInvalidLen, "$indexOfArray",
			fmt.Sprintf("Expression $indexOfArray takes 2, 3, or 4 arguments. %d were passed in.", len(args)))
	}
}

func (op *indexOfArrayOp) Process(doc *types.Document) (any, error) {
	arrV, err := evalArgValue(op.arrayArg, doc)
	if err != nil {
		return nil, err
	}
	if isNullish(arrV) {
		return types.Null, nil
	}
	arr, ok := arrV.(*types.Array)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexOfArrayNotArray,
			"$indexOfArray requires an array as a first argument, found: "+handlerparams.AliasFromType(arrV), "$indexOfArray")
	}

	search, err := evalArgValue(op.searchArg, doc)
	if err != nil {
		return nil, err
	}

	items, err := arrayToSlice(arr)
	if err != nil {
		return nil, err
	}
	start, end := 0, len(items)
	if op.startArg != nil {
		sv, err := evalArgValue(op.startArg, doc)
		if err != nil {
			return nil, err
		}
		if start, err = requireIndex(sv, "$indexOfArray", "starting index"); err != nil {
			return nil, err
		}
	}
	if op.endArg != nil {
		ev, err := evalArgValue(op.endArg, doc)
		if err != nil {
			return nil, err
		}
		if end, err = requireIndex(ev, "$indexOfArray", "ending index"); err != nil {
			return nil, err
		}
		end = min(end, len(items))
	}

	searchKey := op.cmp.KeyValue(search)
	for i := start; i < end; i++ {
		if types.CompareForAggregation(op.cmp.KeyValue(items[i]), searchKey) == types.Equal {
			return int32(i), nil
		}
	}
	return int32(-1), nil
}

var _ Operator = (*indexOfArrayOp)(nil)

// rangeOp represents { $range: [ <start>, <end>, <step> ] }.
type rangeOp struct {
	startArg any
	endArg   any
	stepArg  any
}

func newRange(args ...any) (Operator, error) {
	switch len(args) {
	case 2:
		return &rangeOp{startArg: args[0], endArg: args[1]}, nil
	case 3:
		return &rangeOp{startArg: args[0], endArg: args[1], stepArg: args[2]}, nil
	default:
		return nil, newOperatorError(ErrArgsInvalidLen, "$range",
			fmt.Sprintf("Expression $range takes 2 or 3 arguments. %d were passed in.", len(args)))
	}
}

func (op *rangeOp) Process(doc *types.Document) (any, error) {
	sv, err := evalArgValue(op.startArg, doc)
	if err != nil {
		return nil, err
	}

	ev, err := evalArgValue(op.endArg, doc)
	if err != nil {
		return nil, err
	}

	start, err := rangeStartArg.int32Value(sv)
	if err != nil {
		return nil, err
	}

	end, err := rangeEndArg.int32Value(ev)
	if err != nil {
		return nil, err
	}

	step := int64(1)

	if op.stepArg != nil {
		stv, err := evalArgValue(op.stepArg, doc)
		if err != nil {
			return nil, err
		}

		if step, err = rangeStepArg.int32Value(stv); err != nil {
			return nil, err
		}
	}

	if step == 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrRangeStepZero, "$range requires a non-zero step value", "$range",
		)
	}

	var count int64
	if span := end - start; span != 0 && (span > 0) == (step > 0) {
		count = (span + step - sign(step)) / step
	}

	if bytes := count * rangeElementBytes; bytes > maxRangeBytes {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrExceededMemoryLimit,
			fmt.Sprintf("$range would use too much memory (%d bytes) and cannot spill to disk. Memory limit: %d bytes",
				bytes, maxRangeBytes),
			"$range",
		)
	}

	items := make([]any, count)
	for i := range items {
		items[i] = int32(start + int64(i)*step)
	}

	return sliceToArray(items), nil
}

// maxRangeBytes matches MongoDB's internalQueryMaxRangeBytes default.
const maxRangeBytes = 100 * 1024 * 1024

// rangeElementBytes is the approximate in-memory size of one $range element.
const rangeElementBytes = 16

// rangeArg describes one $range argument for MongoDB's validation errors.
type rangeArg struct {
	notNumeric handlererrors.ErrorCode
	notInt32   handlererrors.ErrorCode
	numericMsg string // "$range requires a numeric ... value, found value of type:" + sep
	int32What  string
}

var (
	rangeStartArg = rangeArg{
		handlererrors.ErrRangeStartNotNumeric, handlererrors.ErrRangeStartNotInt32,
		"$range requires a numeric starting value, found value of type: ", "a starting value",
	}
	rangeEndArg = rangeArg{
		handlererrors.ErrRangeEndNotNumeric, handlererrors.ErrRangeEndNotInt32,
		"$range requires a numeric ending value, found value of type: ", "an ending value",
	}
	// MongoDB's step message has no space after the colon.
	rangeStepArg = rangeArg{
		handlererrors.ErrRangeStepNotNumeric, handlererrors.ErrRangeStepNotInt32,
		"$range requires a numeric step value, found value of type:", "a step value",
	}
)

// int32Value returns v as an int64 holding an int32 value. As in MongoDB,
// v must be numeric (null included is not) and represent a 32-bit integer
// exactly; Decimal128 is accepted on the same terms.
func (a rangeArg) int32Value(v any) (int64, error) {
	switch v := v.(type) {
	case int32:
		return int64(v), nil
	case int64:
		if v < math.MinInt32 || v > math.MaxInt32 {
			return 0, a.notInt32Error(v)
		}
		return v, nil
	case float64:
		if v != math.Trunc(v) || v < math.MinInt32 || v > math.MaxInt32 {
			return 0, a.notInt32Error(v)
		}
		return int64(v), nil
	case types.Decimal128:
		n, ok := aggregations.Decimal128ToInt64(v)
		if !ok || n < math.MinInt32 || n > math.MaxInt32 {
			return 0, a.notInt32Error(types.FormatAnyValue(v))
		}
		return n, nil
	default:
		return 0, handlererrors.NewCommandErrorMsgWithArgument(
			a.notNumeric, a.numericMsg+handlerparams.AliasFromType(v), "$range",
		)
	}
}

func (a rangeArg) notInt32Error(v any) error {
	return handlererrors.NewCommandErrorMsgWithArgument(
		a.notInt32,
		fmt.Sprintf("$range requires %s that can be represented as a 32-bit integer, found value: %v", a.int32What, v),
		"$range",
	)
}

func sign(n int64) int64 {
	if n < 0 {
		return -1
	}
	return 1
}

var _ Operator = (*rangeOp)(nil)

// inArrayOp represents { $in: [ <expr>, <array-expr> ] }.
type inArrayOp struct {
	valueArg any
	arrayArg any
	collated
}

func newInArray(args ...any) (Operator, error) {
	if len(args) != 2 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$in",
			fmt.Sprintf("Expression $in takes exactly 2 arguments. %d were passed in.", len(args)))
	}

	return &inArrayOp{valueArg: args[0], arrayArg: args[1]}, nil
}

func (op *inArrayOp) Process(doc *types.Document) (any, error) {
	val, err := evalArgValue(op.valueArg, doc)
	if err != nil {
		return nil, err
	}

	v, missing, err := EvalOperand(op.arrayArg, doc)
	if err != nil {
		return nil, err
	}
	arr, ok := v.(*types.Array)
	if !ok {
		typeName := "missing"
		if !missing {
			typeName = handlerparams.AliasFromType(v)
		}
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInRequiresArray,
			"$in requires an array as a second argument, found: "+typeName, "$in")
	}

	items, err := arrayToSlice(arr)
	if err != nil {
		return nil, err
	}

	valKey := op.cmp.KeyValue(val)
	for _, item := range items {
		if types.CompareForAggregation(op.cmp.KeyValue(item), valKey) == types.Equal {
			return true, nil
		}
	}

	return false, nil
}

var _ Operator = (*inArrayOp)(nil)

// filterOp represents
// { $filter: { input: <array>, as: <var>, cond: <cond-expr>, limit: <expr> } }.
type filterOp struct {
	inputArg any
	asName   string
	condArg  any
	limitArg any
}

func newFilter(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$filter",
			"$filter requires a document with 'input', 'cond' fields")
	}

	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$filter",
			"$filter requires a document argument")
	}

	for _, k := range doc.Keys() {
		switch k {
		case "input", "as", "cond", "limit":
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFilterUnknownArgument,
				"Unrecognized parameter to $filter: "+k, "$filter")
		}
	}

	inputArg, err := doc.Get("input")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFilterMissingInput,
			"Missing 'input' parameter to $filter", "$filter")
	}

	condArg, err := doc.Get("cond")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFilterMissingCond,
			"Missing 'cond' parameter to $filter", "$filter")
	}

	asName, err := iterationVariableName(doc)
	if err != nil {
		return nil, err
	}

	limitArg, _ := doc.Get("limit")

	return &filterOp{inputArg: inputArg, asName: asName, condArg: condArg, limitArg: limitArg}, nil
}

// iterationVariableName returns the variable named by the 'as' field of a
// $map or $filter, where mongod reads any non-string value as an empty name.
func iterationVariableName(spec *types.Document) (string, error) {
	asV, err := spec.Get("as")
	if err != nil {
		return "this", nil
	}
	name, _ := asV.(string)
	if err := CheckUserVariableName(name); err != nil {
		return "", err
	}
	return name, nil
}

func (op *filterOp) Process(doc *types.Document) (any, error) {
	input, err := evalArgValue(op.inputArg, doc)
	if err != nil {
		return nil, err
	}
	if isNullish(input) {
		return types.Null, nil
	}
	arr, ok := input.(*types.Array)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFilterInputNotArray,
			"input to $filter must be an array not "+handlerparams.AliasFromType(input), "$filter")
	}
	if arr.Len() == 0 {
		return arr, nil
	}

	limit, err := op.limit(doc)
	if err != nil {
		return nil, err
	}

	result := types.MakeArray(0)
	for i := 0; i < arr.Len() && (limit == 0 || result.Len() < limit); i++ {
		item := must.NotFail(arr.Get(i))

		iterDoc := copyForVariables(doc)
		bindVariable(iterDoc, op.asName, item, false)

		condVal, err := evalArgValue(op.condArg, iterDoc)
		if err != nil {
			return nil, err
		}

		if !isFalsy(condVal) {
			result.Append(item)
		}
	}

	return result, nil
}

// limit evaluates the limit argument; 0 means none.
func (op *filterOp) limit(doc *types.Document) (int, error) {
	if op.limitArg == nil {
		return 0, nil
	}
	v, missing, err := EvalOperand(op.limitArg, doc)
	if err != nil {
		return 0, err
	}
	if missing || isNullish(v) {
		return 0, nil
	}

	n, ok := exactInt32(v)
	if !ok {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFilterLimitNotInt32,
			"$filter: limit must be represented as a 32-bit integral value: "+types.FormatAnyValue(v), "$filter")
	}
	if n <= 0 {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFilterLimitNotPositive,
			fmt.Sprintf("$filter: limit must be greater than 0: %d", n), "$filter")
	}
	return int(n), nil
}

// exactInt32 returns a number that represents a 32-bit integer exactly.
func exactInt32(v any) (int32, bool) {
	var n int64
	switch v := v.(type) {
	case int32:
		return v, true
	case int64:
		n = v
	case float64:
		if v != math.Trunc(v) || v < math.MinInt32 || v > math.MaxInt32 {
			return 0, false
		}
		n = int64(v)
	case types.Decimal128:
		var ok bool
		if n, ok = aggregations.Decimal128ToInt64(v); !ok {
			return 0, false
		}
	default:
		return 0, false
	}
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, false
	}
	return int32(n), true
}

var _ Operator = (*filterOp)(nil)

// mapOp represents { $map: { input: <array>, as: <var>, in: <expr> } }.
type mapOp struct {
	inputArg any
	asName   string
	inArg    any
}

func newMap(args ...any) (Operator, error) {
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$map")
	}
	var doc *types.Document
	if len(args) == 1 {
		doc, _ = args[0].(*types.Document)
	}
	if doc == nil {
		return nil, fail(handlererrors.ErrMapNotObject, "$map only supports an object as its argument")
	}
	for _, k := range doc.Keys() {
		switch k {
		case "input", "as", "in":
		default:
			return nil, fail(handlererrors.ErrMapUnknownParameter, "Unrecognized parameter to $map: "+k)
		}
	}

	inputArg, err := doc.Get("input")
	if err != nil {
		return nil, fail(handlererrors.ErrMapMissingInput, "Missing 'input' parameter to $map")
	}

	inArg, err := doc.Get("in")
	if err != nil {
		return nil, fail(handlererrors.ErrMapMissingIn, "Missing 'in' parameter to $map")
	}

	asName, err := iterationVariableName(doc)
	if err != nil {
		return nil, err
	}

	return &mapOp{inputArg: inputArg, asName: asName, inArg: inArg}, nil
}

func (op *mapOp) Process(doc *types.Document) (any, error) {
	arr, isNull, err := collectArray(op.inputArg, doc, "$map")
	if err != nil {
		return nil, err
	}

	if isNull {
		return types.Null, nil
	}

	items, err := arrayToSlice(arr)
	if err != nil {
		return nil, err
	}

	result := make([]any, 0, len(items))

	for _, item := range items {
		iterDoc := copyForVariables(doc)
		if err != nil {
			return nil, err
		}

		bindVariable(iterDoc, op.asName, item, false)

		val, err := evalArgValue(op.inArg, iterDoc)
		if err != nil {
			return nil, err
		}

		result = append(result, val)
	}

	return sliceToArray(result), nil
}

var _ Operator = (*mapOp)(nil)

// reduceOp represents { $reduce: { input: <array>, initialValue: <val>, in: <expr> } }.
type reduceOp struct {
	inputArg        any
	initialValueArg any
	inArg           any
}

func newReduce(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$reduce",
			"$reduce requires a document with 'input', 'initialValue', 'in' fields")
	}

	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$reduce", "$reduce requires a document argument")
	}

	inputArg, err := doc.Get("input")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$reduce", "Missing 'input' parameter to $reduce")
	}

	initialValueArg, err := doc.Get("initialValue")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$reduce", "Missing 'initialValue' parameter to $reduce")
	}

	inArg, err := doc.Get("in")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$reduce", "Missing 'in' parameter to $reduce")
	}

	return &reduceOp{inputArg: inputArg, initialValueArg: initialValueArg, inArg: inArg}, nil
}

func (op *reduceOp) Process(doc *types.Document) (any, error) {
	arr, isNull, err := collectArray(op.inputArg, doc, "$reduce")
	if err != nil {
		return nil, err
	}

	if isNull {
		return types.Null, nil
	}

	accumulator, missing, err := EvalOperand(op.initialValueArg, doc)
	if err != nil {
		return nil, err
	}

	items, err := arrayToSlice(arr)
	if err != nil {
		return nil, err
	}

	limiter := newReduceLimiter()
	for _, item := range items {
		iterDoc := copyForVariables(doc)
		bindVariable(iterDoc, "value", accumulator, missing)
		bindVariable(iterDoc, "this", item, false)

		if accumulator, missing, err = EvalOperand(op.inArg, iterDoc); err != nil {
			return nil, err
		}

		if err = limiter.check(accumulator); err != nil {
			return nil, err
		}
	}

	if missing {
		return nil, aggregations.ErrMissingValue
	}
	return accumulator, nil
}

var _ Operator = (*reduceOp)(nil)

// zipOp represents { $zip: { inputs: [ <arr1>, <arr2>, ... ], useLongestLength: bool, defaults: <arr> } }.
type zipOp struct {
	inputsArg        any
	useLongestLength bool
	defaultsArg      any
}

func newZip(args ...any) (Operator, error) {
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$zip")
	}
	if len(args) != 1 {
		return nil, fail(handlererrors.ErrZipNotObject, "$zip only supports an object as an argument, found array")
	}
	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, fail(handlererrors.ErrZipNotObject,
			"$zip only supports an object as an argument, found "+handlerparams.AliasFromType(args[0]))
	}

	op := &zipOp{}
	var defaults *types.Array
	for _, key := range doc.Keys() {
		v := must.NotFail(doc.Get(key))
		switch key {
		case "inputs":
			if _, isArray := v.(*types.Array); !isArray {
				return nil, fail(handlererrors.ErrZipInputsNotArray,
					"inputs must be an array of expressions, found "+handlerparams.AliasFromType(v))
			}
			op.inputsArg = v
		case "defaults":
			arr, isArray := v.(*types.Array)
			if !isArray {
				return nil, fail(handlererrors.ErrZipDefaultsNotArray,
					"defaults must be an array of expressions, found "+handlerparams.AliasFromType(v))
			}
			defaults = arr
			op.defaultsArg = v
		case "useLongestLength":
			b, isBool := v.(bool)
			if !isBool {
				return nil, fail(handlererrors.ErrZipUseLongestLengthType,
					"useLongestLength must be a bool, found "+handlerparams.AliasFromType(v))
			}
			op.useLongestLength = b
		default:
			return nil, fail(handlererrors.ErrZipUnknownArgument, "$zip found an unknown argument: "+key)
		}
	}

	inputs, _ := op.inputsArg.(*types.Array)
	switch {
	case inputs == nil || inputs.Len() == 0:
		return nil, fail(handlererrors.ErrZipNoInputs, "$zip requires at least one input array")
	case defaults != nil && !op.useLongestLength:
		return nil, fail(handlererrors.ErrZipDefaultsWithoutLongest, "cannot specify defaults unless useLongestLength is true")
	case defaults != nil && defaults.Len() != inputs.Len():
		return nil, fail(handlererrors.ErrZipDefaultsLength, "defaults and inputs must have the same length")
	}
	return op, nil
}

func (op *zipOp) Process(doc *types.Document) (any, error) {
	inputs, err := arrayToSlice(op.inputsArg.(*types.Array))
	if err != nil {
		return nil, err
	}

	var arrays [][]any

	for _, inp := range inputs {
		v, err := evalArgValue(inp, doc)
		if err != nil {
			return nil, err
		}
		if isNullish(v) {
			return types.Null, nil
		}
		arr, isArray := v.(*types.Array)
		if !isArray {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrZipInputNotArray,
				"$zip found a non-array expression in input: "+types.FormatAnyValue(v), "$zip")
		}

		items, err := arrayToSlice(arr)
		if err != nil {
			return nil, err
		}

		arrays = append(arrays, items)
	}

	if len(arrays) == 0 {
		return sliceToArray(nil), nil
	}

	minLen := len(arrays[0])
	maxLen := len(arrays[0])

	for _, a := range arrays[1:] {
		if len(a) < minLen {
			minLen = len(a)
		}

		if len(a) > maxLen {
			maxLen = len(a)
		}
	}

	length := minLen
	if op.useLongestLength {
		length = maxLen
	}

	var defaults []any

	if op.defaultsArg != nil {
		dV, err := evalArgValue(op.defaultsArg, doc)
		if err != nil {
			return nil, err
		}

		if !isNullish(dV) {
			if dArr, ok := dV.(*types.Array); ok {
				defaults, err = arrayToSlice(dArr)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	result := make([]any, 0, length)

	for i := range length {
		tuple := make([]any, len(arrays))

		for j, a := range arrays {
			if i < len(a) {
				tuple[j] = a[i]
			} else if j < len(defaults) {
				tuple[j] = defaults[j]
			} else {
				tuple[j] = types.Null
			}
		}

		result = append(result, sliceToArray(tuple))
	}

	return sliceToArray(result), nil
}

var _ Operator = (*zipOp)(nil)

// objectToArrayOp represents { $objectToArray: <object-expr> }.
// Converts a document to an array of {k, v} documents.
type objectToArrayOp struct{ arg any }

func newObjectToArray(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$objectToArray",
			fmt.Sprintf("Expression $objectToArray takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &objectToArrayOp{arg: args[0]}, nil
}

func (op *objectToArrayOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(v) {
		return types.Null, nil
	}

	src, ok := v.(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$objectToArray",
			fmt.Sprintf("$objectToArray requires a document argument, got %T", v))
	}

	result := types.MakeArray(src.Len())

	for _, k := range src.Keys() {
		pair, err := types.NewDocument("k", k, "v", must.NotFail(src.Get(k)))
		if err != nil {
			return nil, err
		}

		result.Append(pair)
	}

	return result, nil
}

var _ Operator = (*objectToArrayOp)(nil)

// arrayToObjectOp represents { $arrayToObject: <array-expr> }.
// Converts an array of {k, v} documents to a single document.
type arrayToObjectOp struct {
	arg any
	engine
}

func newArrayToObject(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$arrayToObject",
			fmt.Sprintf("Expression $arrayToObject takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &arrayToObjectOp{arg: args[0]}, nil
}

func (op *arrayToObjectOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(v) {
		return types.Null, nil
	}

	arr, ok := v.(*types.Array)
	if !ok {
		return nil, arrayToObjectError(handlererrors.ErrArrayToObjectInput,
			"$arrayToObject requires an array input, found: "+handlerparams.AliasFromType(v))
	}

	result := types.MakeDocument(arr.Len())
	if arr.Len() == 0 {
		return result, nil
	}

	// The first element decides the format: [k, v] pairs or {k, v} objects.
	_, pairs := must.NotFail(arr.Get(0)).(*types.Array)
	for i := range arr.Len() {
		elem := must.NotFail(arr.Get(i))
		var key, val any
		switch e := elem.(type) {
		case *types.Array:
			if !pairs {
				return nil, arrayToObjectError(handlererrors.ErrArrayToObjectObjectsExpected,
					"$arrayToObject requires a consistent input format. Elements must all be arrays or all be objects. "+
						"Object was detected, now found: array")
			}
			if e.Len() != 2 {
				return nil, arrayToObjectError(handlererrors.ErrArrayToObjectPairSize,
					fmt.Sprintf("$arrayToObject requires an array of size 2 arrays,found array of size: %d", e.Len()))
			}
			key, val = must.NotFail(e.Get(0)), must.NotFail(e.Get(1))
			if _, ok := key.(string); !ok {
				return nil, arrayToObjectError(handlererrors.ErrArrayToObjectPairKeyType,
					"$arrayToObject requires an array of key-value pairs, where the key must be of type string. Found key type: "+
						handlerparams.AliasFromType(key))
			}
			if strings.IndexByte(key.(string), 0) >= 0 {
				code := handlererrors.ErrArrayToObjectPairKeyNul
				if op.sbe {
					code = handlererrors.ErrArrayToObjectPairKeyNulSBE
				}
				return nil, arrayToObjectError(code, "Key field cannot contain an embedded null byte")
			}
		case *types.Document:
			if pairs {
				return nil, arrayToObjectError(handlererrors.ErrArrayToObjectArraysExpected,
					"$arrayToObject requires a consistent input format. Elements must all be arrays or all be objects. "+
						"Array was detected, now found: object")
			}
			if e.Len() != 2 {
				return nil, arrayToObjectError(handlererrors.ErrArrayToObjectObjectSize,
					fmt.Sprintf("$arrayToObject requires an object keys of 'k' and 'v'. Found incorrect number of keys:%d", e.Len()))
			}
			var kErr, vErr error
			key, kErr = e.Get("k")
			val, vErr = e.Get("v")
			if kErr != nil || vErr != nil {
				return nil, arrayToObjectError(handlererrors.ErrArrayToObjectObjectKeys,
					"$arrayToObject requires an object with keys 'k' and 'v'. Missing either or both keys from: "+
						types.FormatAnyValue(e))
			}
			if _, ok := key.(string); !ok {
				return nil, arrayToObjectError(handlererrors.ErrArrayToObjectObjectKeyType,
					"$arrayToObject requires an object with keys 'k' and 'v', where the value of 'k' must be of type string. Found type: "+
						handlerparams.AliasFromType(key))
			}
			if strings.IndexByte(key.(string), 0) >= 0 {
				code := handlererrors.ErrArrayToObjectObjectKeyNul
				if op.sbe {
					code = handlererrors.ErrArrayToObjectObjectKeyNulSBE
				}
				return nil, arrayToObjectError(code, "Key field cannot contain an embedded null byte")
			}
		default:
			switch {
			case i == 0:
				return nil, arrayToObjectError(handlererrors.ErrArrayToObjectElementType,
					"Unrecognised input type format for $arrayToObject: "+handlerparams.AliasFromType(elem))
			case pairs:
				return nil, arrayToObjectError(handlererrors.ErrArrayToObjectArraysExpected,
					"$arrayToObject requires a consistent input format. Elements must all be arrays or all be objects. "+
						"Array was detected, now found: "+handlerparams.AliasFromType(elem))
			default:
				return nil, arrayToObjectError(handlererrors.ErrArrayToObjectObjectsExpected,
					"$arrayToObject requires a consistent input format. Elements must all be arrays or all be objects. "+
						"Object was detected, now found: "+handlerparams.AliasFromType(elem))
			}
		}
		result.Set(key.(string), val)
	}

	return result, nil
}

func arrayToObjectError(code handlererrors.ErrorCode, msg string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$arrayToObject")
}

var _ Operator = (*arrayToObjectOp)(nil)
