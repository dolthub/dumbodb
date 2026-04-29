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
	"errors"
	"fmt"
	"sort"
	"strings"

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
	if sortDoc.Len() == 0 {
		return nil
	}

	if sortDoc.Len() > 32 {
		return lazyerrors.Errorf("maximum sort keys exceeded: %v", sortDoc.Len())
	}

	nKeys := sortDoc.Len()
	sortTypes := make([]types.SortType, nKeys)
	// isMeta[k] short-circuits comparison when key k is {$meta: "textScore"}.
	isMeta := make([]bool, nKeys)
	paths := make([]types.Path, nKeys)

	for i, sortKey := range sortDoc.Keys() {
		fields := strings.Split(sortKey, ".")

		switch {
		case sortKey == "$natural":
		default:
			// TODO https://github.com/dolthub/dumbodb/issues/3127
			for _, field := range fields {
				if strings.HasPrefix(field, "$") {
					return handlererrors.NewCommandErrorMsgWithArgument(
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
			isMeta[i] = true
			sortTypes[i] = types.Ascending
			continue
		}

		sortType, err := GetSortType(sortKey, sortField)
		if err != nil {
			return err
		}

		sortPath, err := types.NewPathFromString(sortKey)
		if err != nil {
			return err
		}

		sortTypes[i] = sortType
		paths[i] = sortPath
	}

	// Decorate-sort-undecorate: extract all sort keys for each doc once
	// up front, then sort indices into that table. This drops per-compare
	// GetByPath calls (~2*N*log N) to N.
	keys := make([][]any, len(docs))
	for di, d := range docs {
		row := make([]any, nKeys)
		for k := 0; k < nKeys; k++ {
			if isMeta[k] {
				continue
			}
			v, err := d.GetByPath(paths[k])
			if err != nil {
				// sort order treats null and non-existent field equivalent
				v = types.Null
			}
			row[k] = v
		}
		keys[di] = row
	}

	sorter := &decoratedSorter{
		docs:   docs,
		keys:   keys,
		types:  sortTypes,
		isMeta: isMeta,
	}
	sort.Stable(sorter)

	return nil
}

// decoratedSorter sorts docs and the parallel keys table in lockstep, using
// the precomputed keys to avoid GetByPath calls inside Less.
type decoratedSorter struct {
	docs   []*types.Document
	keys   [][]any
	types  []types.SortType
	isMeta []bool
}

func (s *decoratedSorter) Len() int { return len(s.docs) }

func (s *decoratedSorter) Swap(i, j int) {
	s.docs[i], s.docs[j] = s.docs[j], s.docs[i]
	s.keys[i], s.keys[j] = s.keys[j], s.keys[i]
}

func (s *decoratedSorter) Less(i, j int) bool {
	a, b := s.keys[i], s.keys[j]
	for k := 0; k < len(s.types); k++ {
		if s.isMeta[k] {
			continue
		}
		switch types.CompareOrderForSort(a[k], b[k], s.types[k]) {
		case types.Less:
			return true
		case types.Greater:
			return false
		}
		// Equal: fall through to the next key.
	}
	return false
}

// SortDocumentsWithCollation sorts documents like SortDocuments but uses
// case-insensitive string comparison when caseInsensitive is true.
func SortDocumentsWithCollation(docs []*types.Document, sortDoc *types.Document, caseInsensitive bool) error {
	if !caseInsensitive {
		return SortDocuments(docs, sortDoc)
	}

	if sortDoc.Len() == 0 {
		return nil
	}

	if sortDoc.Len() > 32 {
		return lazyerrors.Errorf("maximum sort keys exceeded: %v", sortDoc.Len())
	}

	sortFuncs := make([]sortFunc, sortDoc.Len())

	for i, sortKey := range sortDoc.Keys() {
		fields := strings.Split(sortKey, ".")

		switch {
		case sortKey == "$natural":
		default:
			for _, field := range fields {
				if strings.HasPrefix(field, "$") {
					return handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrFieldPathInvalidName,
						"FieldPath field names may not start with '$'. Consider using $getField or $setField.",
						"sort",
					)
				}
			}
		}

		sortField := must.NotFail(sortDoc.Get(sortKey))

		if isMetaTextScore(sortField) {
			sortFuncs[i] = func(a, b *types.Document) bool { return false }
			continue
		}

		sortType, err := GetSortType(sortKey, sortField)
		if err != nil {
			return err
		}

		sortPath, err := types.NewPathFromString(sortKey)
		if err != nil {
			return err
		}

		sortFuncs[i] = lessFuncCaseInsensitive(sortPath, sortType)
	}

	if len(sortFuncs) == 0 {
		return nil
	}

	sorter := &docsSorter{docs: docs, sorts: sortFuncs}
	sort.Stable(sorter)

	return nil
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
			// TODO https://github.com/dolthub/dumbodb/issues/3127
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

type sortFunc func(a, b *types.Document) bool

type docsSorter struct {
	docs  []*types.Document
	sorts []sortFunc
}

func (ds *docsSorter) Len() int {
	return len(ds.docs)
}

func (ds *docsSorter) Swap(i, j int) {
	ds.docs[i], ds.docs[j] = ds.docs[j], ds.docs[i]
}

func (ds *docsSorter) Less(i, j int) bool {
	p, q := ds.docs[i], ds.docs[j]
	// Try all but the last comparison.
	var k int
	for k = 0; k < len(ds.sorts)-1; k++ {
		sortFunc := ds.sorts[k]

		switch {
		case sortFunc(p, q):
			// p < q, so we have a decision.
			return true
		case sortFunc(q, p):
			// p > q, so we have a decision.
			return false
		}
		// p == q; try the next comparison.
	}
	// All comparisons to here said "equal", so just return whatever
	// the final comparison reports.
	return ds.sorts[k](p, q)
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
	// {$meta: "textScore"} is a valid sort value — sort descending by text score.
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
