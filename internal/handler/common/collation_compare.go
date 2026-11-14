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

package common

import (
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/commonpath"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// signToResult maps a collator sign (-1/0/+1) to a types.CompareResult.
func signToResult(n int) types.CompareResult {
	switch {
	case n < 0:
		return types.Less
	case n > 0:
		return types.Greater
	default:
		return types.Equal
	}
}

// collCompare compares two values like types.Compare, but compares strings,
// alone or inside arrays, under cmp when cmp is non-nil. Strings inside
// embedded documents compare bytewise, as collated indexes key them. It walks
// arrays element by element, as types.Compare does, without copying them.
func collCompare(a, b any, cmp *collation.Comparator) types.CompareResult {
	if cmp == nil {
		return types.Compare(a, b)
	}
	if as, ok := a.(string); ok {
		if bs, ok := b.(string); ok {
			return signToResult(cmp.CompareStrings(as, bs))
		}
	}
	if arr, ok := a.(*types.Array); ok {
		return collCompareArray(arr, b, cmp)
	}
	return types.Compare(a, b)
}

// collCompareArray is types.Compare for an array against b under cmp.
func collCompareArray(as *types.Array, b any, cmp *collation.Comparator) types.CompareResult {
	if bs, ok := b.(*types.Array); ok {
		return collCompareArrays(as, bs, cmp)
	}
	result, comparable := types.Less, false
	for i := 0; i < as.Len(); i++ {
		a := must.NotFail(as.Get(i))
		if types.CompareTypeOrder(a, b) != types.Equal {
			continue
		}
		if result = collCompare(a, b, cmp); result == types.Equal {
			return result
		}
		comparable = true
	}
	if !comparable {
		return types.Less
	}
	return result
}

// collCompareArrays is types.Compare for two arrays under cmp: element by
// element, the shorter array first when one is a prefix of the other.
func collCompareArrays(docArr, filterArr *types.Array, cmp *collation.Comparator) types.CompareResult {
	switch {
	case docArr.Len() == 0 && filterArr.Len() == 0:
		return types.Equal
	case docArr.Len() == 0:
		return types.Less
	case filterArr.Len() == 0:
		return types.Greater
	}
	for i := 0; i < docArr.Len(); i++ {
		if i >= filterArr.Len() {
			return types.Greater
		}
		if result := collCompareOrder(must.NotFail(docArr.Get(i)), must.NotFail(filterArr.Get(i)), cmp); result != types.Equal {
			return result
		}
	}
	if docArr.Len() < filterArr.Len() {
		return types.Less
	}
	return types.Equal
}

// collCompareOrder is types.CompareOrder under cmp: type order first, then
// value.
func collCompareOrder(a, b any, cmp *collation.Comparator) types.CompareResult {
	if result := types.CompareTypeOrder(a, b); result != types.Equal {
		return result
	}
	return collCompare(a, b, cmp)
}

// collCompareOrderOp is the collation-aware form of
// types.CompareOrderForOperator, used by the range operators: an array
// compares by its least (ascending) or greatest (descending) element of b's
// type, chosen under cmp.
func collCompareOrderOp(a, b any, order types.SortType, cmp *collation.Comparator) types.CompareResult {
	if cmp != nil {
		if as, ok := a.(string); ok {
			if bs, ok := b.(string); ok {
				return signToResult(cmp.CompareStrings(as, bs))
			}
		}
	}
	arrA, isAArray := a.(*types.Array)
	arrB, isBArray := b.(*types.Array)
	if cmp == nil || !isAArray && !isBArray {
		return types.CompareOrderForOperator(a, b, order)
	}
	if isAArray && !isBArray {
		arrA = arrA.FilterArrayByType(b)
	}
	if isAArray && arrA.Len() == 0 && isBArray && arrB.Len() == 0 {
		return types.Equal
	}
	if !isAArray && isBArray && arrB.Len() == 0 {
		if order == types.Ascending {
			return types.Greater
		}
		return types.Less
	}
	if isAArray && !isBArray {
		a = collArrayExtreme(arrA, order, cmp)
	}
	if types.CompareTypeOrder(a, b) != types.Equal {
		if order == types.Ascending {
			return types.Greater
		}
		return types.Less
	}
	return collCompare(a, b, cmp)
}

// collArrayExtreme is arr's least element for Ascending and greatest for
// Descending under cmp, or arr itself when it is empty.
func collArrayExtreme(arr *types.Array, order types.SortType, cmp *collation.Comparator) any {
	if arr.Len() == 0 {
		return arr
	}
	extreme := must.NotFail(arr.Get(0))
	for i := 1; i < arr.Len(); i++ {
		v := must.NotFail(arr.Get(i))
		c := collCompareOrder(extreme, v, cmp)
		if order == types.Ascending && c == types.Greater || order == types.Descending && c == types.Less {
			extreme = v
		}
	}
	return extreme
}

// arrayStringKeys returns v with every string, alone or inside arrays,
// replaced by its sort key under cmp, so that values ordered once per value
// compare bytewise in collation order.
func arrayStringKeys(v any, cmp *collation.Comparator) any {
	switch v := v.(type) {
	case string:
		return string(cmp.Key(v))
	case *types.Array:
		out := types.MakeArray(v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Append(arrayStringKeys(must.NotFail(v.Get(i)), cmp))
		}
		return out
	}
	return v
}

// lessFuncCollated returns a sort comparator that orders string fields under
// cmp and falls back to standard ordering for non-string values.
func lessFuncCollated(sortPath types.Path, sortType types.SortType, cmp *collation.Comparator) func(a, b *types.Document) bool {
	return func(a, b *types.Document) bool {
		aField := commonpath.SortKeyValue(a, sortPath)
		bField := commonpath.SortKeyValue(b, sortPath)

		aStr, aIsStr := aField.(string)
		bStr, bIsStr := bField.(string)

		if aIsStr && bIsStr {
			sign := cmp.CompareStrings(aStr, bStr)
			if sign == 0 {
				return false
			}
			if sortType == types.Ascending {
				return sign < 0
			}
			return sign > 0
		}

		return types.CompareOrderForSort(aField, bField, sortType) == types.Less
	}
}
