// Copyright 2021 FerretDB Inc.
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
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/commonpath"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// SortDocuments sorts given documents in place according to the given sorting conditions.
//
// If sort path is invalid, it returns a possibly wrapped types.PathError.
func SortDocuments(docs []*types.Document, sortDoc *types.Document) error {
	return sortDocuments(docs, sortDoc, nil)
}

// sortDocuments sorts docs with strings ordered under coll, nil for binary.
func sortDocuments(docs []*types.Document, sortDoc *types.Document, coll *collation.Comparator) error {
	if sortDoc.Len() == 0 {
		return nil
	}

	sk, err := newSortKeyer(sortDoc, coll)
	if err != nil {
		return err
	}

	// Decorate-sort-undecorate: extract all sort keys for each doc once
	// up front, then sort into that table. This drops per-compare
	// GetByPath and collation key calls (~2*N*log N) to N.
	rows := make([]sortRow, len(docs))
	for i, d := range docs {
		rows[i] = sortRow{doc: d, keys: sk.keysOf(d)}
	}
	sk.sortRows(rows)
	for i, r := range rows {
		docs[i] = r.doc
	}

	return nil
}

// sortRow is a document with its extracted sort keys.
type sortRow struct {
	doc  *types.Document
	keys []any
}

// sortKeyer extracts and compares the sort keys of a sort specification.
type sortKeyer struct {
	sortTypes []types.SortType
	// isMeta[k] short-circuits comparison when key k is {$meta: "textScore"}.
	isMeta []bool
	paths  []types.Path
	// wholeID: mongod assumes _id is never an array, as in stored documents,
	// so a sort on _id alone compares whole values, arrays included.
	wholeID bool
	coll    *collation.Comparator
}

func newSortKeyer(sortDoc *types.Document, coll *collation.Comparator) (*sortKeyer, error) {
	if sortDoc.Len() > 32 {
		return nil, lazyerrors.Errorf("maximum sort keys exceeded: %v", sortDoc.Len())
	}

	nKeys := sortDoc.Len()
	sk := &sortKeyer{
		sortTypes: make([]types.SortType, nKeys),
		isMeta:    make([]bool, nKeys),
		paths:     make([]types.Path, nKeys),
		coll:      coll,
	}

	for i, sortKey := range sortDoc.Keys() {
		if sortKey != "$natural" {
			for _, field := range strings.Split(sortKey, ".") {
				if strings.HasPrefix(field, "$") {
					return nil, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrFieldPathInvalidName,
						"FieldPath field names may not start with '$'. Consider using $getField or $setField.",
						"sort",
					)
				}
			}
		}

		sortField := must.NotFail(sortDoc.Get(sortKey))

		if isMetaTextScore(sortField) {
			// All docs have the same text score (0.0); preserve stable order.
			sk.isMeta[i] = true
			sk.sortTypes[i] = types.Ascending
			continue
		}

		sortType, err := GetSortType(sortKey, sortField)
		if err != nil {
			return nil, err
		}

		sortPath, err := types.NewPathFromString(sortKey)
		if err != nil {
			return nil, err
		}

		sk.sortTypes[i] = sortType
		sk.paths[i] = sortPath
	}

	sk.wholeID = nKeys == 1 && !sk.isMeta[0] && sortDoc.Command() == "_id"

	return sk, nil
}

// keysOf returns d's sort keys, collation-keyed.
func (sk *sortKeyer) keysOf(d *types.Document) []any {
	row := make([]any, len(sk.paths))
	for k := range row {
		if sk.isMeta[k] {
			continue
		}
		if sk.wholeID {
			id, err := d.Get("_id")
			if err != nil {
				id = types.Null
			}
			row[k] = sk.coll.KeyValue(must.NotFail(types.NewArray(id)))
			continue
		}
		row[k] = sortElement(sk.coll.KeyValue(commonpath.SortKeyValue(d, sk.paths[k])), sk.sortTypes[k])
	}
	return row
}

// sortElement returns the element of a non-empty array that a sort in order
// compares, its least or greatest, so comparisons need not find it again.
// Other values, and arrays whose element is itself an array, are returned
// unchanged.
func sortElement(v any, order types.SortType) any {
	arr, ok := v.(*types.Array)
	if !ok || arr.Len() == 0 {
		return v
	}
	var e any
	if order == types.Ascending {
		e = arr.Min()
	} else {
		e = arr.Max()
	}
	if _, nested := e.(*types.Array); nested {
		return v
	}
	return e
}

// sortRows sorts rows stably by their keys. Sorting positions with ties
// broken by position is stable, and pdqsort does far fewer comparisons than
// sort.Stable's insertion-merge.
func (sk *sortKeyer) sortRows(rows []sortRow) {
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(i, j int) int {
		a, b := rows[i].keys, rows[j].keys
		for k := range a {
			if sk.isMeta[k] {
				continue
			}
			switch compareSortKeys(a[k], b[k], sk.sortTypes[k]) {
			case types.Less:
				return -1
			case types.Greater:
				return 1
			}
		}
		return cmp.Compare(i, j)
	})
	sorted := make([]sortRow, len(rows))
	for i, o := range order {
		sorted[i] = rows[o]
	}
	copy(rows, sorted)
}

// compareSortKeys is types.CompareOrderForSort with a fast path for two
// integers, two non-NaN doubles, or two strings.
func compareSortKeys(a, b any, order types.SortType) types.CompareResult {
	var c int
	switch x := a.(type) {
	case int32:
		switch y := b.(type) {
		case int32:
			c = cmp.Compare(x, y)
		case int64:
			c = cmp.Compare(int64(x), y)
		default:
			return types.CompareOrderForSort(a, b, order)
		}
	case int64:
		switch y := b.(type) {
		case int32:
			c = cmp.Compare(x, int64(y))
		case int64:
			c = cmp.Compare(x, y)
		default:
			return types.CompareOrderForSort(a, b, order)
		}
	case float64:
		y, ok := b.(float64)
		if !ok || math.IsNaN(x) || math.IsNaN(y) {
			return types.CompareOrderForSort(a, b, order)
		}
		c = cmp.Compare(x, y)
	case string:
		y, ok := b.(string)
		if !ok {
			return types.CompareOrderForSort(a, b, order)
		}
		c = strings.Compare(x, y)
	default:
		return types.CompareOrderForSort(a, b, order)
	}
	if order == types.Descending {
		c = -c
	}
	switch {
	case c < 0:
		return types.Less
	case c > 0:
		return types.Greater
	}
	return types.Equal
}

// SortDocumentsWithCollation sorts documents like SortDocuments, with
// strings ordered under cmp, also inside documents and arrays.
func SortDocumentsWithCollation(docs []*types.Document, sortDoc *types.Document, cmp *collation.Comparator) error {
	return sortDocuments(docs, sortDoc, cmp)
}

// ValidateSortDocument validates sort documents, and return
// proper error if it's invalid.
func ValidateSortDocument(sortDoc *types.Document) (*types.Document, error) {
	if sortDoc.Len() == 0 {
		return nil, nil
	}

	if sortDoc.Len() > 32 {
		return nil, lazyerrors.Errorf("maximum sort keys exceeded: %v", sortDoc.Len())
	}

	res := types.MakeDocument(sortDoc.Len())

	for _, sortKey := range sortDoc.Keys() {
		fields := strings.Split(sortKey, ".")

		switch {
		case sortKey == "$natural":
			if sortDoc.Len() != 1 {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					fmt.Sprintf(`Cannot include '$natural' in compound sort: %v`, types.FormatAnyValue(sortDoc)),
					"sort",
				)
			}
		default:
			for _, field := range fields {
				if strings.HasPrefix(field, "$") {
					return nil, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrFieldPathInvalidName,
						"FieldPath field names may not start with '$'. Consider using $getField or $setField.",
						"sort",
					)
				}
			}
		}

		sortField := must.NotFail(sortDoc.Get(sortKey))

		if isMetaTextScore(sortField) {
			// Preserve {$meta: "textScore"} as-is; SortDocuments handles it.
			res.Set(sortKey, sortField)
			continue
		}

		sortValue, err := getSortValue(sortKey, sortField)
		if err != nil {
			return nil, err
		}

		_, err = types.NewPathFromString(sortKey)
		if err != nil {
			return nil, err
		}

		res.Set(sortKey, sortValue)
	}

	return res, nil
}

// GetSortType determines SortType from input sort value.
func GetSortType(key string, value any) (types.SortType, error) {
	sortValue, err := getSortValue(key, value)
	if err != nil {
		return 0, err
	}

	switch sortValue {
	case 1:
		return types.Ascending, nil
	case -1:
		return types.Descending, nil
	default:
		panic("unreachable")
	}
}

// isMetaTextScore returns true if value is {$meta: "textScore"}.
func isMetaTextScore(value any) bool {
	doc, ok := value.(*types.Document)
	if !ok {
		return false
	}
	if doc.Len() != 1 {
		return false
	}
	v, err := doc.Get("$meta")
	if err != nil {
		return false
	}
	s, ok := v.(string)
	return ok && s == "textScore"
}

// getSortValue validates if the value from sort document is the proper sort field,
// and returns it.
func getSortValue(key string, value any) (int64, error) {
	// {$meta: "textScore"} is a valid sort value  -- sort descending by text score.
	// DumboDB assigns score 0 to all docs so stable order is preserved.
	if isMetaTextScore(value) {
		return -1, nil
	}

	sortValue, err := handlerparams.GetWholeNumberParam(value)
	if err != nil {
		switch {
		case errors.Is(err, handlerparams.ErrUnexpectedType):
			var formattedValue string
			if _, ok := value.(types.NullType); ok {
				formattedValue = "null"
			} else {
				formattedValue = types.FormatAnyValue(value)
			}

			return 0, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrSortBadValue,
				fmt.Sprintf(`Illegal key in $sort specification: %v: %v`, key, formattedValue),
				"$sort",
			)
		case errors.Is(err, handlerparams.ErrNotWholeNumber):
			return 0, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrSortBadOrder,
				"$sort key ordering must be 1 (for ascending) or -1 (for descending)",
				"$sort",
			)
		default:
			return 0, err
		}
	}

	if sortValue != -1 && sortValue != 1 {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrSortBadOrder,
			"$sort key ordering must be 1 (for ascending) or -1 (for descending)",
			"$sort",
		)
	}

	return sortValue, nil
}

// CompareSortKeys compares two values extracted by commonpath.SortKeyValue as
// a sort in order compares them.
func CompareSortKeys(a, b any, order types.SortType) types.CompareResult {
	return compareSortKeys(a, b, order)
}
