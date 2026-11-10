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
	"slices"

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/commonpath"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// SortIteratorInIndexOrder sorts iter's documents in the order a walk
// of the index with key pattern returns them. The walk reaches a document
// first at the entry of an array element that filter admits on that field,
// so such a field sorts by those elements alone.
func SortIteratorInIndexOrder(iter types.DocumentsIterator, closer *iterator.MultiCloser, pattern, filter *types.Document, cmp *collation.Comparator) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := ConsumeDocuments(iter, "$sort")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}
	if err = sortDocumentsInIndexOrder(docs, pattern, filter, cmp); err != nil {
		return nil, lazyerrors.Error(err)
	}
	res := iterator.Values(iterator.ForSlice(docs))
	closer.Add(res)
	return res, nil
}

func sortDocumentsInIndexOrder(docs []*types.Document, pattern, filter *types.Document, cmp *collation.Comparator) error {
	fields := pattern.Keys()
	paths := make([]types.Path, len(fields))
	orders := make([]types.SortType, len(fields))
	conds := make([]any, len(fields))
	for i, field := range fields {
		path, err := types.NewPathFromString(field)
		if err != nil {
			return err
		}
		paths[i] = path
		if orders[i], err = GetSortType(field, must.NotFail(pattern.Get(field))); err != nil {
			return err
		}
		if cond, err := filter.Get(field); err == nil {
			conds[i] = cond
		}
	}

	keys := make([][]any, len(docs))
	for d, doc := range docs {
		row := make([]any, len(fields))
		for i := range fields {
			row[i] = commonpath.SortKeyValue(doc, paths[i])
			if arr, ok := row[i].(*types.Array); ok && conds[i] != nil {
				if admitted := elementsAdmitted(arr, conds[i], cmp); admitted.Len() > 0 {
					row[i] = admitted
				}
			}
		}
		keys[d] = row
	}

	order := make([]int, len(docs))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		for i := range fields {
			switch compareIndexOrderKeys(keys[a][i], keys[b][i], orders[i], cmp) {
			case types.Less:
				return -1
			case types.Greater:
				return 1
			}
		}
		// The index orders equal keys by RecordId, which is _id order.
		aID, aErr := docs[a].Get("_id")
		bID, bErr := docs[b].Get("_id")
		if aErr != nil || bErr != nil {
			return 0
		}
		return int(types.CompareOrderForSort(aID, bID, types.Ascending))
	})
	sorted := make([]*types.Document, len(docs))
	for i, o := range order {
		sorted[i] = docs[o]
	}
	copy(docs, sorted)
	return nil
}

// elementsAdmitted returns the elements of arr that cond, a filter's
// condition on the field holding arr, matches on their own.
func elementsAdmitted(arr *types.Array, cond any, cmp *collation.Comparator) *types.Array {
	condDoc := must.NotFail(types.NewDocument("v", cond))
	out := types.MakeArray(arr.Len())
	for i := 0; i < arr.Len(); i++ {
		elem := must.NotFail(arr.Get(i))
		if ok, err := FilterDocumentColl(must.NotFail(types.NewDocument("v", elem)), condDoc, cmp); err == nil && ok {
			out.Append(elem)
		}
	}
	return out
}

func compareIndexOrderKeys(a, b any, order types.SortType, cmp *collation.Comparator) types.CompareResult {
	aStr, aIsStr := a.(string)
	bStr, bIsStr := b.(string)
	if cmp == nil || !aIsStr || !bIsStr {
		return types.CompareOrderForSort(a, b, order)
	}
	sign := cmp.CompareStrings(aStr, bStr)
	if order == types.Descending {
		sign = -sign
	}
	switch {
	case sign < 0:
		return types.Less
	case sign > 0:
		return types.Greater
	}
	return types.Equal
}
