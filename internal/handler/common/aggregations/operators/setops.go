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

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

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

// toSet converts an array to a deduplicated slice.
func toSet(arr *types.Array) ([]any, error) {
	items, err := arrayToSlice(arr)
	if err != nil {
		return nil, err
	}

	var result []any

	for _, item := range items {
		found := false

		for _, existing := range result {
			if types.CompareForAggregation(item, existing) == types.Equal {
				found = true
				break
			}
		}

		if !found {
			result = append(result, item)
		}
	}

	return result, nil
}

type setUnionOp struct{ args []any }

func newSetUnion(args ...any) (Operator, error) {
	return &setUnionOp{args: args}, nil
}

func (op *setUnionOp) Process(doc *types.Document) (any, error) {
	var result []any
	sawNull := false
	notArray := ""

	for _, arg := range op.args {
		arr, nullish, typeName, err := evalSetOperand(arg, doc)
		if err != nil {
			return nil, err
		}

		if nullish {
			sawNull = true
			continue
		}

		if arr == nil {
			if notArray == "" {
				notArray = typeName
			}
			continue
		}

		items, err := arrayToSlice(arr)
		if err != nil {
			return nil, err
		}

		for _, item := range items {
			found := false

			for _, existing := range result {
				if types.CompareForAggregation(item, existing) == types.Equal {
					found = true
					break
				}
			}

			if !found {
				result = append(result, item)
			}
		}
	}

	switch {
	case sawNull:
		return types.Null, nil
	case notArray != "":
		return nil, setOperandError(handlererrors.ErrSetUnionNotArray, "$setUnion",
			"All operands of $setUnion must be arrays. One argument is of type: "+notArray)
	}

	return sliceToArray(result), nil
}

var _ Operator = (*setUnionOp)(nil)

type setIntersectionOp struct{ args []any }

func newSetIntersection(args ...any) (Operator, error) {
	return &setIntersectionOp{args: args}, nil
}

func (op *setIntersectionOp) Process(doc *types.Document) (any, error) {
	var candidates []any
	sawNull := false
	notArray := ""

	for i, arg := range op.args {
		arr, nullish, typeName, err := evalSetOperand(arg, doc)
		if err != nil {
			return nil, err
		}

		if nullish {
			sawNull = true
			continue
		}

		if arr == nil {
			if notArray == "" {
				notArray = typeName
			}
			continue
		}

		if sawNull || notArray != "" {
			continue
		}

		if i == 0 {
			if candidates, err = toSet(arr); err != nil {
				return nil, err
			}
			continue
		}

		items, err := arrayToSlice(arr)
		if err != nil {
			return nil, err
		}

		var next []any

		for _, c := range candidates {
			for _, item := range items {
				if types.CompareForAggregation(c, item) == types.Equal {
					next = append(next, c)
					break
				}
			}
		}

		candidates = next
	}

	switch {
	case sawNull:
		return types.Null, nil
	case notArray != "":
		return nil, setOperandError(handlererrors.ErrSetIntersectionNotArray, "$setIntersection",
			"All operands of $setIntersection must be arrays. One argument is of type: "+notArray)
	}

	return sliceToArray(candidates), nil
}

var _ Operator = (*setIntersectionOp)(nil)

// setDifferenceOp represents { $setDifference: [ <set1>, <set2> ] }.
type setDifferenceOp struct {
	set1Arg any
	set2Arg any
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

	set1, err := toSet(arr1)
	if err != nil {
		return nil, err
	}

	set2items, err := arrayToSlice(arr2)
	if err != nil {
		return nil, err
	}

	var result []any

	for _, item := range set1 {
		inSet2 := false

		for _, s2 := range set2items {
			if types.CompareForAggregation(item, s2) == types.Equal {
				inSet2 = true
				break
			}
		}

		if !inSet2 {
			result = append(result, item)
		}
	}

	return sliceToArray(result), nil
}

var _ Operator = (*setDifferenceOp)(nil)

type setEqualsOp struct{ args []any }

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

		if nullish {
			return types.Null, nil
		}

		arrs[i], typeNames[i] = arr, typeName
	}

	var sets [][]any

	for i, arr := range arrs {
		if arr == nil {
			return nil, setOperandError(handlererrors.ErrSetEqualsNotArray, "$setEquals",
				fmt.Sprintf("All operands of $setEquals must be arrays. %d-th argument is of type: %s", i+1, typeNames[i]))
		}

		set, err := toSet(arr)
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
			found := false

			for _, o := range other {
				if types.CompareForAggregation(item, o) == types.Equal {
					found = true
					break
				}
			}

			if !found {
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

	switch {
	case null1 || null2:
		return types.Null, nil
	case arr1 == nil:
		return nil, setOperandError(handlererrors.ErrSetIsSubsetFirstNotArray, "$setIsSubset",
			"both operands of $setIsSubset must be arrays. First argument is of type: "+type1)
	case arr2 == nil:
		return nil, setOperandError(handlererrors.ErrSetIsSubsetSecondNotArray, "$setIsSubset",
			"both operands of $setIsSubset must be arrays. Second argument is of type: "+type2)
	}

	set1, err := toSet(arr1)
	if err != nil {
		return nil, err
	}

	set2items, err := arrayToSlice(arr2)
	if err != nil {
		return nil, err
	}

	for _, item := range set1 {
		found := false

		for _, s2 := range set2items {
			if types.CompareForAggregation(item, s2) == types.Equal {
				found = true
				break
			}
		}

		if !found {
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
