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

	"github.com/dolthub/dumbodb/internal/types"
)

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
			if types.Compare(item, existing) == 0 {
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

// ── $setUnion ─────────────────────────────────────────────────────────────────

type setUnionOp struct{ args []any }

func newSetUnion(args ...any) (Operator, error) {
	return &setUnionOp{args: args}, nil
}

func (op *setUnionOp) Process(doc *types.Document) (any, error) {
	var result []any

	for _, arg := range op.args {
		arr, isNull, err := collectArray(arg, doc, "$setUnion")
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

		for _, item := range items {
			found := false

			for _, existing := range result {
				if types.Compare(item, existing) == 0 {
					found = true
					break
				}
			}

			if !found {
				result = append(result, item)
			}
		}
	}

	return sliceToArray(result), nil
}

var _ Operator = (*setUnionOp)(nil)

// ── $setIntersection ──────────────────────────────────────────────────────────

type setIntersectionOp struct{ args []any }

func newSetIntersection(args ...any) (Operator, error) {
	return &setIntersectionOp{args: args}, nil
}

func (op *setIntersectionOp) Process(doc *types.Document) (any, error) {
	if len(op.args) == 0 {
		return sliceToArray(nil), nil
	}

	arr0, isNull, err := collectArray(op.args[0], doc, "$setIntersection")
	if err != nil {
		return nil, err
	}

	if isNull {
		return types.Null, nil
	}

	candidates, err := toSet(arr0)
	if err != nil {
		return nil, err
	}

	for _, arg := range op.args[1:] {
		arr, isNull, err := collectArray(arg, doc, "$setIntersection")
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

		var next []any

		for _, c := range candidates {
			for _, item := range items {
				if types.Compare(c, item) == 0 {
					next = append(next, c)
					break
				}
			}
		}

		candidates = next
	}

	return sliceToArray(candidates), nil
}

var _ Operator = (*setIntersectionOp)(nil)

// ── $setDifference ────────────────────────────────────────────────────────────

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
	arr1, isNull, err := collectArray(op.set1Arg, doc, "$setDifference")
	if err != nil {
		return nil, err
	}

	if isNull {
		return types.Null, nil
	}

	arr2, isNull, err := collectArray(op.set2Arg, doc, "$setDifference")
	if err != nil {
		return nil, err
	}

	if isNull {
		return types.Null, nil
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
			if types.Compare(item, s2) == 0 {
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

// ── $setEquals ────────────────────────────────────────────────────────────────

type setEqualsOp struct{ args []any }

func newSetEquals(args ...any) (Operator, error) {
	if len(args) < 2 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$setEquals",
			fmt.Sprintf("Expression $setEquals takes at least 2 arguments. %d were passed in.", len(args)))
	}

	return &setEqualsOp{args: args}, nil
}

func (op *setEqualsOp) Process(doc *types.Document) (any, error) {
	var sets [][]any

	for _, arg := range op.args {
		arr, isNull, err := collectArray(arg, doc, "$setEquals")
		if err != nil {
			return nil, err
		}

		if isNull {
			return types.Null, nil
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
				if types.Compare(item, o) == 0 {
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

// ── $setIsSubset ──────────────────────────────────────────────────────────────

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

	return &setIsSubsetOp{set1Arg: args[0], set2Arg: args[1]}, nil
}

func (op *setIsSubsetOp) Process(doc *types.Document) (any, error) {
	arr1, isNull, err := collectArray(op.set1Arg, doc, "$setIsSubset")
	if err != nil {
		return nil, err
	}

	if isNull {
		return types.Null, nil
	}

	arr2, isNull, err := collectArray(op.set2Arg, doc, "$setIsSubset")
	if err != nil {
		return nil, err
	}

	if isNull {
		return types.Null, nil
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
			if types.Compare(item, s2) == 0 {
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

// ── $anyElementTrue ───────────────────────────────────────────────────────────

type anyElementTrueOp struct{ arg any }

func newAnyElementTrue(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$anyElementTrue",
			fmt.Sprintf("Expression $anyElementTrue takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &anyElementTrueOp{arg: args[0]}, nil
}

func (op *anyElementTrueOp) Process(doc *types.Document) (any, error) {
	arr, isNull, err := collectArray(op.arg, doc, "$anyElementTrue")
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
		if !isFalsy(item) {
			return true, nil
		}
	}

	return false, nil
}

var _ Operator = (*anyElementTrueOp)(nil)

// ── $allElementsTrue ──────────────────────────────────────────────────────────

type allElementsTrueOp struct{ arg any }

func newAllElementsTrue(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$allElementsTrue",
			fmt.Sprintf("Expression $allElementsTrue takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &allElementsTrueOp{arg: args[0]}, nil
}

func (op *allElementsTrueOp) Process(doc *types.Document) (any, error) {
	arr, isNull, err := collectArray(op.arg, doc, "$allElementsTrue")
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
		if isFalsy(item) {
			return false, nil
		}
	}

	return true, nil
}

var _ Operator = (*allElementsTrueOp)(nil)
