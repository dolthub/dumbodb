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

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// collectArray evaluates arg to a *types.Array. Returns nil if null, error if not array.
func collectArray(arg any, doc *types.Document, opName string) (*types.Array, bool, error) {
	v, err := evalArgValue(arg, doc)
	if err != nil {
		return nil, false, err
	}

	if v == types.Null {
		return nil, true, nil
	}

	arr, ok := v.(*types.Array)
	if !ok {
		return nil, false, newOperatorError(ErrArgsInvalidLen, opName,
			fmt.Sprintf("%s argument must be an array, got %T", opName, v))
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

// ── $size ─────────────────────────────────────────────────────────────────────

type sizeOp struct{ arg any }

func newSize(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$size",
			fmt.Sprintf("Expression $size takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &sizeOp{arg: args[0]}, nil
}

func (op *sizeOp) Process(doc *types.Document) (any, error) {
	arr, isNull, err := collectArray(op.arg, doc, "$size")
	if err != nil {
		return nil, err
	}

	if isNull {
		return types.Null, nil
	}

	return int32(arr.Len()), nil
}

var _ Operator = (*sizeOp)(nil)

// ── $isArray ──────────────────────────────────────────────────────────────────

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

// ── $arrayElemAt ──────────────────────────────────────────────────────────────

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
	arr, isNull, err := collectArray(op.arrayArg, doc, "$arrayElemAt")
	if err != nil {
		return nil, err
	}

	if isNull {
		return types.Null, nil
	}

	idxV, err := evalArgValue(op.idxArg, doc)
	if err != nil {
		return nil, err
	}

	if idxV == types.Null {
		return types.Null, nil
	}

	idx := int(toFloat64(idxV))
	n := arr.Len()

	if idx < 0 {
		idx = n + idx
	}

	if idx < 0 || idx >= n {
		return types.Null, nil
	}

	v, err := arr.Get(idx)
	if err != nil {
		return types.Null, nil
	}

	return v, nil
}

var _ Operator = (*arrayElemAtOp)(nil)

// ── $concatArrays ─────────────────────────────────────────────────────────────

type concatArraysOp struct{ args []any }

func newConcatArrays(args ...any) (Operator, error) {
	return &concatArraysOp{args: args}, nil
}

func (op *concatArraysOp) Process(doc *types.Document) (any, error) {
	var result []any

	for _, arg := range op.args {
		arr, isNull, err := collectArray(arg, doc, "$concatArrays")
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

		result = append(result, items...)
	}

	return sliceToArray(result), nil
}

var _ Operator = (*concatArraysOp)(nil)

// ── $reverseArray ─────────────────────────────────────────────────────────────

type reverseArrayOp struct{ arg any }

func newReverseArray(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$reverseArray",
			fmt.Sprintf("Expression $reverseArray takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &reverseArrayOp{arg: args[0]}, nil
}

func (op *reverseArrayOp) Process(doc *types.Document) (any, error) {
	arr, isNull, err := collectArray(op.arg, doc, "$reverseArray")
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

	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}

	return sliceToArray(items), nil
}

var _ Operator = (*reverseArrayOp)(nil)

// ── $slice ────────────────────────────────────────────────────────────────────

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
	arr, isNull, err := collectArray(op.arrayArg, doc, "$slice")
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

	n := len(items)

	nV, err := evalArgValue(op.nArg, doc)
	if err != nil {
		return nil, err
	}

	count := int(toFloat64(nV))

	start := 0
	if op.positionArg != nil {
		pV, err := evalArgValue(op.positionArg, doc)
		if err != nil {
			return nil, err
		}

		start = int(toFloat64(pV))
		if start < 0 {
			start = n + start
			if start < 0 {
				start = 0
			}
		}
	} else if count < 0 {
		// negative n without position means from end
		start = n + count
		if start < 0 {
			start = 0
		}

		count = n - start
	}

	if start > n {
		return sliceToArray(nil), nil
	}

	end := start + count
	if end > n {
		end = n
	}

	if start < 0 {
		start = 0
	}

	return sliceToArray(items[start:end]), nil
}

var _ Operator = (*sliceOp)(nil)

// ── $indexOfArray ─────────────────────────────────────────────────────────────

// indexOfArrayOp represents { $indexOfArray: [ <array>, <search-expr>, <start>, <end> ] }.
type indexOfArrayOp struct {
	arrayArg  any
	searchArg any
	startArg  any
	endArg    any
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
	arr, isNull, err := collectArray(op.arrayArg, doc, "$indexOfArray")
	if err != nil {
		return nil, err
	}

	if isNull {
		return types.Null, nil
	}

	search, err := evalArgValue(op.searchArg, doc)
	if err != nil {
		return nil, err
	}

	items, err := arrayToSlice(arr)
	if err != nil {
		return nil, err
	}

	n := len(items)
	start := 0
	end := n

	if op.startArg != nil {
		sv, err := evalArgValue(op.startArg, doc)
		if err != nil {
			return nil, err
		}

		if sv != types.Null {
			start = int(toFloat64(sv))
		}
	}

	if op.endArg != nil {
		ev, err := evalArgValue(op.endArg, doc)
		if err != nil {
			return nil, err
		}

		if ev != types.Null {
			end = int(toFloat64(ev))
		}
	}

	if start < 0 {
		start = 0
	}

	if end > n {
		end = n
	}

	for i := start; i < end; i++ {
		if types.Compare(items[i], search) == 0 {
			return int32(i), nil
		}
	}

	return int32(-1), nil
}

var _ Operator = (*indexOfArrayOp)(nil)

// ── $range ────────────────────────────────────────────────────────────────────

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

	start := int(toFloat64(sv))
	end := int(toFloat64(ev))
	step := 1

	if op.stepArg != nil {
		stv, err := evalArgValue(op.stepArg, doc)
		if err != nil {
			return nil, err
		}

		if stv != types.Null {
			step = int(toFloat64(stv))
		}
	}

	if step == 0 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$range", "$range requires a non-zero step value")
	}

	var items []any

	if step > 0 {
		for i := start; i < end; i += step {
			items = append(items, int32(i))
		}
	} else {
		for i := start; i > end; i += step {
			items = append(items, int32(i))
		}
	}

	return sliceToArray(items), nil
}

var _ Operator = (*rangeOp)(nil)

// ── $in (array membership check) ──────────────────────────────────────────────

// inArrayOp represents { $in: [ <expr>, <array-expr> ] }.
type inArrayOp struct {
	valueArg any
	arrayArg any
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

	arr, isNull, err := collectArray(op.arrayArg, doc, "$in")
	if err != nil {
		return nil, err
	}

	if isNull {
		return false, nil
	}

	items, err := arrayToSlice(arr)
	if err != nil {
		return nil, err
	}

	for _, item := range items {
		if types.Compare(item, val) == 0 {
			return true, nil
		}
	}

	return false, nil
}

var _ Operator = (*inArrayOp)(nil)

// ── $filter ───────────────────────────────────────────────────────────────────

// filterOp represents { $filter: { input: <array>, as: <var>, cond: <cond-expr> } }.
type filterOp struct {
	inputArg any
	asName   string
	condArg  any
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

	inputArg, err := doc.Get("input")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$filter", "Missing 'input' parameter to $filter")
	}

	condArg, err := doc.Get("cond")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$filter", "Missing 'cond' parameter to $filter")
	}

	asName := "this"
	if asV, err := doc.Get("as"); err == nil {
		if s, ok := asV.(string); ok {
			asName = s
		}
	}

	return &filterOp{inputArg: inputArg, asName: asName, condArg: condArg}, nil
}

func (op *filterOp) Process(doc *types.Document) (any, error) {
	arr, isNull, err := collectArray(op.inputArg, doc, "$filter")
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

	var result []any

	for _, item := range items {
		// Create a document with the variable bound.
		iterDoc := doc.DeepCopy()
		if err != nil {
			return nil, err
		}

		iterDoc.Set("$$"+op.asName, item)

		condVal, err := evalArgValue(op.condArg, iterDoc)
		if err != nil {
			return nil, err
		}

		if !isFalsy(condVal) {
			result = append(result, item)
		}
	}

	return sliceToArray(result), nil
}

var _ Operator = (*filterOp)(nil)

// ── $map ──────────────────────────────────────────────────────────────────────

// mapOp represents { $map: { input: <array>, as: <var>, in: <expr> } }.
type mapOp struct {
	inputArg any
	asName   string
	inArg    any
}

func newMap(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$map",
			"$map requires a document with 'input', 'in' fields")
	}

	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$map", "$map requires a document argument")
	}

	inputArg, err := doc.Get("input")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$map", "Missing 'input' parameter to $map")
	}

	inArg, err := doc.Get("in")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$map", "Missing 'in' parameter to $map")
	}

	asName := "this"
	if asV, err := doc.Get("as"); err == nil {
		if s, ok := asV.(string); ok {
			asName = s
		}
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
		iterDoc := doc.DeepCopy()
		if err != nil {
			return nil, err
		}

		iterDoc.Set("$$"+op.asName, item)

		val, err := evalArgValue(op.inArg, iterDoc)
		if err != nil {
			return nil, err
		}

		result = append(result, val)
	}

	return sliceToArray(result), nil
}

var _ Operator = (*mapOp)(nil)

// ── $reduce ───────────────────────────────────────────────────────────────────

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

	accumulator, err := evalArgValue(op.initialValueArg, doc)
	if err != nil {
		return nil, err
	}

	items, err := arrayToSlice(arr)
	if err != nil {
		return nil, err
	}

	for _, item := range items {
		iterDoc := doc.DeepCopy()
		if err != nil {
			return nil, err
		}

		iterDoc.Set("$$value", accumulator)
		iterDoc.Set("$$this", item)

		accumulator, err = evalArgValue(op.inArg, iterDoc)
		if err != nil {
			return nil, err
		}
	}

	return accumulator, nil
}

var _ Operator = (*reduceOp)(nil)

// ── $zip ──────────────────────────────────────────────────────────────────────

// zipOp represents { $zip: { inputs: [ <arr1>, <arr2>, ... ], useLongestLength: bool, defaults: <arr> } }.
type zipOp struct {
	inputsArg          any
	useLongestLength   bool
	defaultsArg        any
}

func newZip(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$zip", "$zip requires a document argument")
	}

	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$zip", "$zip requires a document argument")
	}

	inputsArg, err := doc.Get("inputs")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$zip", "Missing 'inputs' parameter to $zip")
	}

	useLongest := false
	if ullV, err := doc.Get("useLongestLength"); err == nil {
		if b, ok := ullV.(bool); ok {
			useLongest = b
		}
	}

	var defaultsArg any
	if dV, err := doc.Get("defaults"); err == nil {
		defaultsArg = dV
	}

	return &zipOp{inputsArg: inputsArg, useLongestLength: useLongest, defaultsArg: defaultsArg}, nil
}

func (op *zipOp) Process(doc *types.Document) (any, error) {
	inputsV, err := evalArgValue(op.inputsArg, doc)
	if err != nil {
		return nil, err
	}

	if inputsV == types.Null {
		return types.Null, nil
	}

	inputsArr, ok := inputsV.(*types.Array)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$zip", "$zip 'inputs' must be an array")
	}

	inputs, err := arrayToSlice(inputsArr)
	if err != nil {
		return nil, err
	}

	// Evaluate each input array.
	var arrays [][]any

	for _, inp := range inputs {
		arr, isNull, err := collectArray(inp, doc, "$zip")
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

		arrays = append(arrays, items)
	}

	if len(arrays) == 0 {
		return sliceToArray(nil), nil
	}

	// Determine length.
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

	// Collect defaults.
	var defaults []any

	if op.defaultsArg != nil {
		dV, err := evalArgValue(op.defaultsArg, doc)
		if err != nil {
			return nil, err
		}

		if dV != types.Null {
			if dArr, ok := dV.(*types.Array); ok {
				defaults, err = arrayToSlice(dArr)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	// Build result.
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

// ── $objectToArray ────────────────────────────────────────────────────────────

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

	if v == types.Null {
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

// ── $arrayToObject ────────────────────────────────────────────────────────────

// arrayToObjectOp represents { $arrayToObject: <array-expr> }.
// Converts an array of {k, v} documents to a single document.
type arrayToObjectOp struct{ arg any }

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

	if v == types.Null {
		return types.Null, nil
	}

	arr, ok := v.(*types.Array)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$arrayToObject",
			fmt.Sprintf("$arrayToObject requires an array argument, got %T", v))
	}

	result := types.MakeDocument(arr.Len())

	iter := arr.Iterator()
	defer iter.Close()

	for {
		_, elem, iterErr := iter.Next()
		if errors.Is(iterErr, iterator.ErrIteratorDone) {
			break
		}

		if iterErr != nil {
			return nil, iterErr
		}

		pair, ok := elem.(*types.Document)
		if !ok {
			return nil, newOperatorError(ErrArgsInvalidLen, "$arrayToObject",
				"$arrayToObject requires an array of objects with 'k' and 'v' fields")
		}

		kv, err := pair.Get("k")
		if err != nil {
			return nil, newOperatorError(ErrArgsInvalidLen, "$arrayToObject",
				"$arrayToObject element must have a 'k' field")
		}

		key, ok := kv.(string)
		if !ok {
			return nil, newOperatorError(ErrArgsInvalidLen, "$arrayToObject",
				"$arrayToObject element 'k' field must be a string")
		}

		val, err := pair.Get("v")
		if err != nil {
			return nil, newOperatorError(ErrArgsInvalidLen, "$arrayToObject",
				"$arrayToObject element must have a 'v' field")
		}

		result.Set(key, val)
	}

	return result, nil
}

var _ Operator = (*arrayToObjectOp)(nil)
