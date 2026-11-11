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

// SortIteratorInIndexOrder sorts iter's documents in the order a walk of the
// index with key pattern returns them. The walk reaches a document first at
// the entry of an array element inside filter's bounds on that field, so such
// a field sorts by those elements alone.
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
	admits := make([]func(any) bool, len(fields))
	bounded := true
	for i, field := range fields {
		path, err := types.NewPathFromString(field)
		if err != nil {
			return err
		}
		paths[i] = path
		if orders[i], err = GetSortType(field, must.NotFail(pattern.Get(field))); err != nil {
			return err
		}
		// mongod bounds a field only below bounded ones; past an unbounded
		// field it reads every key.
		if bounded {
			if admits[i] = elementAdmitter(filter, field, cmp); admits[i] == nil {
				bounded = false
			}
		}
	}

	keys := make([][]any, len(docs))
	for d, doc := range docs {
		row := make([]any, len(fields))
		for i := range fields {
			row[i] = indexKeyValue(commonpath.SortKeyValue(doc, paths[i]), admits[i])
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

// indexKeyValue is the key an index walk first reaches for a field holding
// v: the elements of an array admit accepts, or all of them, and undefined
// for an empty array, as it is indexed.
func indexKeyValue(v any, admit func(any) bool) any {
	arr, ok := v.(*types.Array)
	if !ok {
		return v
	}
	if arr.Len() == 0 {
		return types.Undefined
	}
	if admit == nil {
		return arr
	}
	admitted := types.MakeArray(arr.Len())
	for i := 0; i < arr.Len(); i++ {
		if elem := must.NotFail(arr.Get(i)); admit(elem) {
			admitted.Append(elem)
		}
	}
	if admitted.Len() == 0 {
		return arr
	}
	return admitted
}

// elementAdmitter returns whether filter's bounds on field hold an element
// value, or nil when filter does not bound field. It reads field's condition
// at the top level and under $and, and under $or only when every branch
// bounds field.
func elementAdmitter(filter *types.Document, field string, cmp *collation.Comparator) func(any) bool {
	var tests []func(any) bool
	for _, k := range filter.Keys() {
		v := must.NotFail(filter.Get(k))
		switch k {
		case field:
			condDoc := must.NotFail(types.NewDocument("v", v))
			tests = append(tests, func(elem any) bool {
				ok, err := FilterDocumentColl(must.NotFail(types.NewDocument("v", elem)), condDoc, cmp)
				return err == nil && ok
			})
		case "$and":
			for _, branch := range filterBranches(v) {
				if t := elementAdmitter(branch, field, cmp); t != nil {
					tests = append(tests, t)
				}
			}
		case "$or":
			branches := filterBranches(v)
			alternatives := make([]func(any) bool, 0, len(branches))
			for _, branch := range branches {
				t := elementAdmitter(branch, field, cmp)
				if t == nil {
					alternatives = nil
					break
				}
				alternatives = append(alternatives, t)
			}
			if len(alternatives) > 0 {
				tests = append(tests, func(elem any) bool {
					for _, t := range alternatives {
						if t(elem) {
							return true
						}
					}
					return false
				})
			}
		}
	}
	if len(tests) == 0 {
		return nil
	}
	return func(elem any) bool {
		for _, t := range tests {
			if !t(elem) {
				return false
			}
		}
		return true
	}
}

// filterBranches returns the documents of an $and or $or operand.
func filterBranches(v any) []*types.Document {
	arr, ok := v.(*types.Array)
	if !ok {
		return nil
	}
	out := make([]*types.Document, 0, arr.Len())
	for i := 0; i < arr.Len(); i++ {
		if d, ok := must.NotFail(arr.Get(i)).(*types.Document); ok {
			out = append(out, d)
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
