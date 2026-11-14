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
// alone or inside arrays and documents, under cmp when cmp is non-nil. Field
// names compare bytewise. It walks arrays and documents element by element,
// as types.Compare does, without copying them.
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
	if ad, ok := a.(*types.Document); ok {
		if bd, ok := b.(*types.Document); ok {
			return collCompareDocuments(ad, bd, cmp)
		}
	}
	return types.Compare(a, b)
}

// collCompareDocuments is types.Compare for two documents under cmp: field by
// field, each by value type order, then name, then value.
func collCompareDocuments(a, b *types.Document, cmp *collation.Comparator) types.CompareResult {
	aKeys, bKeys := a.Keys(), b.Keys()
	aValues, bValues := a.Values(), b.Values()
	for i := range aKeys {
		if i == len(bKeys) {
			return types.Greater
		}
		if result := types.CompareTypeOrder(aValues[i], bValues[i]); result != types.Equal {
			return result
		}
		if result := types.Compare(aKeys[i], bKeys[i]); result != types.Equal {
			return result
		}
		if result := collCompare(aValues[i], bValues[i], cmp); result != types.Equal {
			return result
		}
	}
	if len(aKeys) < len(bKeys) {
		return types.Less
	}
	return types.Equal
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
	if cmp == nil {
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

// rangeMatches reports whether a range operator's comparison of fieldValue
// with operand under order gives want or also. An array operand also
// matches an array field holding an array element that compares so.
func (fc *FilterContext) rangeMatches(fieldValue, operand any, order types.SortType, want, also types.CompareResult) bool {
	if r := collCompareOrderOp(fieldValue, operand, order, fc.cmp); r == want || r == also {
		return true
	}
	fieldArr, isArray := fieldValue.(*types.Array)
	if _, operandIsArray := operand.(*types.Array); !isArray || !operandIsArray {
		return false
	}
	for i := range fieldArr.Len() {
		if elem, ok := must.NotFail(fieldArr.Get(i)).(*types.Array); ok {
			if r := collCompare(elem, operand, fc.cmp); r == want || r == also {
				return true
			}
		}
	}
	return false
}
