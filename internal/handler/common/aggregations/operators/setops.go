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
	"fmt"

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// engine records whether mongod runs an operator in SBE.
type engine struct {
	sbe bool
}

func (e *engine) useSBE() { e.sbe = true }

// evalSetOperand evaluates an operand of a set operator: an array, a nullish
// value, or another type, named as mongod names it.
//
// A nullish operand makes a set operator null before any operand's type is
// checked, as in mongod's SBE engine. The classic engine checks types first,
// and fails on a nullish $setEquals or $setIsSubset operand.
func evalSetOperand(arg any, doc *types.Document) (arr *types.Array, nullish bool, typeName string, err error) {
	v, missing, err := evalArgValueOrMissing(arg, doc)
	if err != nil {
		return nil, false, "", err
	}
	if missing {
		return nil, true, "missing", nil
	}
	if arr, ok := v.(*types.Array); ok {
		return arr, false, "array", nil
	}
	return nil, isNullish(v), handlerparams.AliasFromType(v), nil
}

func setOperandError(code handlererrors.ErrorCode, opName, msg string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(code, msg, opName)
}

// keyedValue is a set element with its collation key, computed once so
// comparisons do not rebuild it.
type keyedValue struct {
	v   any
	key any
}

// keyedValues returns arr's elements with their collation keys.
func keyedValues(arr *types.Array, cmp *collation.Comparator) ([]keyedValue, error) {
	items, err := arrayToSlice(arr)
	if err != nil {
		return nil, err
	}
	out := make([]keyedValue, len(items))
	for i, item := range items {
		out[i] = keyedValue{v: item, key: cmp.KeyValue(item)}
	}
	return out, nil
}

// containsKey reports whether set holds an element whose key equals key.
func containsKey(set []keyedValue, key any) bool {
	for _, e := range set {
		if types.CompareForAggregation(key, e.key) == types.Equal {
			return true
		}
	}
	return false
}

// appendNew appends the elements of items not yet in set.
func appendNew(set, items []keyedValue) []keyedValue {
	for _, item := range items {
		if !containsKey(set, item.key) {
			set = append(set, item)
		}
	}
	return set
}

// toSet converts an array to a deduplicated slice.
func toSet(arr *types.Array, cmp *collation.Comparator) ([]keyedValue, error) {
	items, err := keyedValues(arr, cmp)
	if err != nil {
		return nil, err
	}
	return appendNew(nil, items), nil
}

func keyedToArray(set []keyedValue) *types.Array {
	arr := types.MakeArray(len(set))
	for _, e := range set {
		arr.Append(e.v)
	}
	return arr
}

type setUnionOp struct {
	args []any
	collated
	engine
}

func newSetUnion(args ...any) (Operator, error) {
	return &setUnionOp{args: args}, nil
}

func (op *setUnionOp) Process(doc *types.Document) (any, error) {
	var result []keyedValue
	sawNull := false
	notArray := ""

	for _, arg := range op.args {
		arr, nullish, typeName, err := evalSetOperand(arg, doc)
		if err != nil {
			return nil, err
		}

		if nullish {
			if !op.sbe {
				return types.Null, nil
			}
			sawNull = true
			continue
		}

		if arr == nil {
			if !op.sbe {
				return nil, op.notArrayError(typeName)
			}
			if notArray == "" {
				notArray = typeName
			}
			continue
		}

		items, err := keyedValues(arr, op.cmp)
		if err != nil {
			return nil, err
		}

		result = appendNew(result, items)
	}

	switch {
	case sawNull:
		return types.Null, nil
	case notArray != "":
		return nil, op.notArrayError(notArray)
	}

	return keyedToArray(result), nil
}

func (op *setUnionOp) notArrayError(typeName string) error {
	return setOperandError(handlererrors.ErrSetUnionNotArray, "$setUnion",
		"All operands of $setUnion must be arrays. One argument is of type: "+typeName)
}

var _ Operator = (*setUnionOp)(nil)

type setIntersectionOp struct {
	args []any
	collated
	engine
}

func newSetIntersection(args ...any) (Operator, error) {
	return &setIntersectionOp{args: args}, nil
}

func (op *setIntersectionOp) notArrayError(typeName string) error {
	return setOperandError(handlererrors.ErrSetIntersectionNotArray, "$setIntersection",
		"All operands of $setIntersection must be arrays. One argument is of type: "+typeName)
}

func (op *setIntersectionOp) Process(doc *types.Document) (any, error) {
	var candidates []keyedValue
	sawNull := false
	notArray := ""

	for i, arg := range op.args {
		arr, nullish, typeName, err := evalSetOperand(arg, doc)
		if err != nil {
			return nil, err
		}

		if nullish {
			if !op.sbe {
				return types.Null, nil
			}
			sawNull = true
			continue
		}

		if arr == nil {
			if !op.sbe {
				return nil, op.notArrayError(typeName)
			}
			if notArray == "" {
				notArray = typeName
			}
			continue
		}

		if sawNull || notArray != "" {
			continue
		}

		if i == 0 {
			if candidates, err = toSet(arr, op.cmp); err != nil {
				return nil, err
			}
			continue
		}

		items, err := keyedValues(arr, op.cmp)
		if err != nil {
			return nil, err
		}

		var next []keyedValue

		for _, c := range candidates {
			if containsKey(items, c.key) {
				next = append(next, c)
			}
		}

		candidates = next
	}

	switch {
	case sawNull:
		return types.Null, nil
	case notArray != "":
		return nil, op.notArrayError(notArray)
	}

	return keyedToArray(candidates), nil
}

var _ Operator = (*setIntersectionOp)(nil)

// setDifferenceOp represents { $setDifference: [ <set1>, <set2> ] }.
type setDifferenceOp struct {
	set1Arg any
	set2Arg any
	collated
}

func newSetDifference(args ...any) (Operator, error) {
	if len(args) != 2 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$setDifference",
			fmt.Sprintf("Expression $setDifference takes exactly 2 arguments. %d were passed in.", len(args)))
	}

	return &setDifferenceOp{set1Arg: args[0], set2Arg: args[1]}, nil
}

func (op *setDifferenceOp) Process(doc *types.Document) (any, error) {
	arr1, null1, type1, err := evalSetOperand(op.set1Arg, doc)
	if err != nil {
		return nil, err
	}

	arr2, null2, type2, err := evalSetOperand(op.set2Arg, doc)
	if err != nil {
		return nil, err
	}

	switch {
	case null1 || null2:
		return types.Null, nil
	case arr1 == nil:
		return nil, setOperandError(handlererrors.ErrSetDifferenceFirstNotArray, "$setDifference",
			"both operands of $setDifference must be arrays. First argument is of type: "+type1)
	case arr2 == nil:
		return nil, setOperandError(handlererrors.ErrSetDifferenceSecondNotArray, "$setDifference",
			"both operands of $setDifference must be arrays. Second argument is of type: "+type2)
	}

	set1, err := toSet(arr1, op.cmp)
	if err != nil {
		return nil, err
	}

	set2items, err := keyedValues(arr2, op.cmp)
	if err != nil {
		return nil, err
	}

	var result []keyedValue

	for _, item := range set1 {
		if !containsKey(set2items, item.key) {
			result = append(result, item)
		}
	}

	return keyedToArray(result), nil
}

var _ Operator = (*setDifferenceOp)(nil)

type setEqualsOp struct {
	args []any
	collated
	engine
}

func newSetEquals(args ...any) (Operator, error) {
	if len(args) < 2 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSetEqualsTooFewArgs,
			fmt.Sprintf("$setEquals needs at least two arguments had: %d", len(args)), "$setEquals")
	}

	return &setEqualsOp{args: args}, nil
}

func (op *setEqualsOp) Process(doc *types.Document) (any, error) {
	arrs := make([]*types.Array, len(op.args))
	typeNames := make([]string, len(op.args))

	for i, arg := range op.args {
		arr, nullish, typeName, err := evalSetOperand(arg, doc)
		if err != nil {
			return nil, err
		}

		if nullish && op.sbe {
			return types.Null, nil
		}

		arrs[i], typeNames[i] = arr, typeName
	}

	var sets [][]keyedValue

	for i, arr := range arrs {
		if arr == nil {
			return nil, setOperandError(handlererrors.ErrSetEqualsNotArray, "$setEquals",
				fmt.Sprintf("All operands of $setEquals must be arrays. %d-th argument is of type: %s", i+1, typeNames[i]))
		}

		set, err := toSet(arr, op.cmp)
		if err != nil {
			return nil, err
		}

		sets = append(sets, set)
	}

	base := sets[0]

	for _, other := range sets[1:] {
		if len(base) != len(other) {
			return false, nil
		}

		for _, item := range base {
			if !containsKey(other, item.key) {
				return false, nil
			}
		}
	}

	return true, nil
}

var _ Operator = (*setEqualsOp)(nil)

// setIsSubsetOp represents { $setIsSubset: [ <set1>, <set2> ] }.
type setIsSubsetOp struct {
	set1Arg any
	set2Arg any
	collated
	engine
}

func newSetIsSubset(args ...any) (Operator, error) {
	if len(args) != 2 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$setIsSubset",
			fmt.Sprintf("Expression $setIsSubset takes exactly 2 arguments. %d were passed in.", len(args)))
	}

	if isConstantExpression(args[1]) {
		v, _, err := foldConstant(args[1], nil)
		if err != nil {
			return nil, err
		}
		if _, ok := v.(*types.Array); !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSetIsSubsetConstantNotArray,
				"both operands of $setIsSubset must be arrays. Second argument is of type: "+handlerparams.AliasFromType(v),
				"$setIsSubset")
		}
	}

	return &setIsSubsetOp{set1Arg: args[0], set2Arg: args[1]}, nil
}

func (op *setIsSubsetOp) Process(doc *types.Document) (any, error) {
	arr1, null1, type1, err := evalSetOperand(op.set1Arg, doc)
	if err != nil {
		return nil, err
	}

	arr2, null2, type2, err := evalSetOperand(op.set2Arg, doc)
	if err != nil {
		return nil, err
	}

	firstNotArray := handlererrors.ErrSetIsSubsetFirstNotArray
	if !isConstantExpression(op.set2Arg) {
		firstNotArray = handlererrors.ErrSetIsSubsetFirstNotArrayVariable
	}
	switch {
	case op.sbe && (null1 || null2):
		return types.Null, nil
	case arr1 == nil:
		return nil, setOperandError(firstNotArray, "$setIsSubset",
			"both operands of $setIsSubset must be arrays. First argument is of type: "+type1)
	case arr2 == nil:
		return nil, setOperandError(handlererrors.ErrSetIsSubsetSecondNotArray, "$setIsSubset",
			"both operands of $setIsSubset must be arrays. Second argument is of type: "+type2)
	}

	set1, err := toSet(arr1, op.cmp)
	if err != nil {
		return nil, err
	}

	set2items, err := keyedValues(arr2, op.cmp)
	if err != nil {
		return nil, err
	}

	for _, item := range set1 {
		if !containsKey(set2items, item.key) {
			return false, nil
		}
	}

	return true, nil
}

var _ Operator = (*setIsSubsetOp)(nil)

type anyElementTrueOp struct{ arg any }

func newAnyElementTrue(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$anyElementTrue",
			fmt.Sprintf("Expression $anyElementTrue takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &anyElementTrueOp{arg: args[0]}, nil
}

func (op *anyElementTrueOp) Process(doc *types.Document) (any, error) {
	arr, _, typeName, err := evalSetOperand(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if arr == nil {
		return nil, setOperandError(handlererrors.ErrAnyElementTrueNotArray, "$anyElementTrue",
			"$anyElementTrue's argument must be an array, but is "+typeName)
	}

	items, err := arrayToSlice(arr)
	if err != nil {
		return nil, err
	}

	for _, item := range items {
		if !isFalsy(item) {
			return true, nil
		}
	}

	return false, nil
}

var _ Operator = (*anyElementTrueOp)(nil)

type allElementsTrueOp struct{ arg any }

func newAllElementsTrue(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$allElementsTrue",
			fmt.Sprintf("Expression $allElementsTrue takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &allElementsTrueOp{arg: args[0]}, nil
}

func (op *allElementsTrueOp) Process(doc *types.Document) (any, error) {
	arr, _, typeName, err := evalSetOperand(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if arr == nil {
		return nil, setOperandError(handlererrors.ErrAllElementsTrueNotArray, "$allElementsTrue",
			"$allElementsTrue's argument must be an array, but is "+typeName)
	}

	items, err := arrayToSlice(arr)
	if err != nil {
		return nil, err
	}

	for _, item := range items {
		if isFalsy(item) {
			return false, nil
		}
	}

	return true, nil
}

var _ Operator = (*allElementsTrueOp)(nil)
