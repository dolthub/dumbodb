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
// embedded documents compare bytewise, as collated indexes key them.
func collCompare(a, b any, cmp *collation.Comparator) types.CompareResult {
	if cmp != nil {
		if as, ok := a.(string); ok {
			if bs, ok := b.(string); ok {
				return signToResult(cmp.CompareStrings(as, bs))
			}
		}
		a, b = collArrayKeys(a, b, cmp)
	}
	return types.Compare(a, b)
}

// collArrayKeys returns a and b with every string, alone or inside arrays,
// replaced by its sort key under cmp when either is an array, so array
// element comparisons follow the collation; otherwise a and b unchanged.
func collArrayKeys(a, b any, cmp *collation.Comparator) (any, any) {
	_, aIsArray := a.(*types.Array)
	_, bIsArray := b.(*types.Array)
	if !aIsArray && !bIsArray {
		return a, b
	}
	return arrayStringKeys(a, cmp), arrayStringKeys(b, cmp)
}

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

// collCompareOrderOp is the collation-aware form of
// types.CompareOrderForOperator, used by the range operators. Only
// string-vs-string comparison is redirected through cmp; the array-max/min and
// type-order handling for every other case is delegated unchanged.
func collCompareOrderOp(a, b any, order types.SortType, cmp *collation.Comparator) types.CompareResult {
	if cmp != nil {
		if as, ok := a.(string); ok {
			if bs, ok := b.(string); ok {
				return signToResult(cmp.CompareStrings(as, bs))
			}
		}
		a, b = collArrayKeys(a, b, cmp)
	}
	return types.CompareOrderForOperator(a, b, order)
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
