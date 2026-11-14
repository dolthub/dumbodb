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

// Package commonpath contains functions used for path.
package commonpath

import (
	"errors"
	"strconv"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// FindValuesOpts sets options for FindValues.
type FindValuesOpts struct {
	// If FindArrayDocuments is true, it iterates the array to find documents that have path.
	// If FindArrayDocuments is false, it does not find documents from the array.
	// Using path `v.foo` and `v` is an array:
	//  - with FindArrayDocuments true, it finds values of `foo` of found documents;
	//  - with FindArrayDocuments false, it returns an empty array.
	// If `v` is not an array, FindArrayDocuments has no impact.
	FindArrayDocuments bool
	// If FindArrayIndex is true, it finds value at index of an array.
	// If FindArrayIndex is false, it does not find value at index of an array.
	// Using path `v.0` and `v` is an array:
	//  - with FindArrayIndex true, it finds 0-th index value of the array;
	//  - with FindArrayIndex false, it returns empty array.
	// If `v` is not an array, FindArrayIndex has no impact.
	FindArrayIndex bool
}

// FindValues returns values by path, looking up into arrays.
//
// It iterates path elements, at each path element it adds to next values to iterate:
//   - if it is a document and has path, it adds the document field value to next values;
//   - if it is an array, FindArrayIndex is true and finds value at index, it adds value to next values;
//   - if it is an array, FindArrayDocuments is true and documents in the array have path,
//     it adds field value of all documents that have path to next values.
//
// It returns next values after iterating path elements.
func FindValues(doc *types.Document, path types.Path, opts *FindValuesOpts) ([]any, error) {
	if opts == nil {
		opts = new(FindValuesOpts)
	}

	nextValues := []any{doc}

	for _, e := range path.Slice() {
		values := []any{}

		for _, next := range nextValues {
			switch next := next.(type) {
			case *types.Document:
				v, _ := next.Get(e)
				if v == nil {
					continue
				}

				values = append(values, v)

			case *types.Array:
				if opts.FindArrayIndex {
					res, err := findArrayIndex(next, e)
					if err == nil {
						values = append(values, res)
						continue
					}
				}

				if opts.FindArrayDocuments {
					res, err := lookupArrayDocuments(next, e)
					if err != nil {
						return nil, lazyerrors.Error(err)
					}

					values = append(values, res...)
				}

			default:
				// path does not exist in scalar values, nothing to do
			}
		}

		nextValues = values
	}

	return nextValues, nil
}

// findArrayIndex returns the value at the array index a path component names.
// Like MongoDB, only digits without a leading zero name an index.
//
// Error is returned if index is not such a number or does not exist in array.
func findArrayIndex(array *types.Array, index string) (any, error) {
	if !IsArrayIndexComponent(index) {
		return nil, lazyerrors.Errorf("not an array index: %q", index)
	}
	i, err := strconv.Atoi(index)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	v, err := array.Get(i)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return v, nil
}

// IsArrayIndexComponent reports whether a path component names an array
// position: digits without a leading zero.
func IsArrayIndexComponent(s string) bool {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// lookupArrayDocuments returns values for the given key in array's document.
//
// Non-document array values, documents without that key, etc. are skipped.
func lookupArrayDocuments(array *types.Array, documentKey string) ([]any, error) {
	iter := array.Iterator()
	defer iter.Close()

	res := []any{}

	for {
		_, v, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		doc, ok := v.(*types.Document)
		if !ok {
			continue
		}

		if v, _ = doc.Get(documentKey); v != nil {
			res = append(res, v)
		}
	}

	return res, nil
}

// HasMissingBranch reports whether resolving path against doc ends early on
// any branch: at a document lacking the next field, or at a non-array scalar
// before the path is exhausted. MongoDB matches such a branch against null and
// sorts it as null. Arrays are entered through their document elements (and by
// position for a numeric component); other array elements, and a position out
// of range, contribute no branch.
func HasMissingBranch(doc *types.Document, path types.Path) bool {
	return hasMissingBranch(doc, path.Slice())
}

func hasMissingBranch(v any, keys []string) bool {
	if len(keys) == 0 {
		return false
	}

	switch v := v.(type) {
	case *types.Document:
		next, err := v.Get(keys[0])
		if err != nil {
			return true
		}
		return hasMissingBranch(next, keys[1:])

	case *types.Array:
		if el, err := findArrayIndex(v, keys[0]); err == nil && isComposite(el) && hasMissingBranch(el, keys[1:]) {
			return true
		}
		for i := 0; i < v.Len(); i++ {
			el, _ := v.Get(i)
			if elDoc, ok := el.(*types.Document); ok && hasMissingBranch(elDoc, keys) {
				return true
			}
		}
		return false

	default:
		return true
	}
}

func isComposite(v any) bool {
	switch v.(type) {
	case *types.Document, *types.Array:
		return true
	}
	return false
}

// SortKeyValue returns the value d sorts by on path. When a dotted path reaches
// several values through arrays, or a missing branch beside values, they are
// gathered into one array so the sort takes its min or max like MongoDB.
// Sort order treats null and a non-existent field as equivalent.
func SortKeyValue(d *types.Document, path types.Path) any {
	if path.Len() == 1 {
		if v, err := d.Get(path.Prefix()); err == nil {
			return v
		}
		return types.Null
	}

	vals, err := FindValues(d, path, &FindValuesOpts{
		FindArrayIndex:     true,
		FindArrayDocuments: true,
	})
	if err != nil {
		return types.Null
	}
	missing := HasMissingBranch(d, path)

	switch {
	case len(vals) == 0:
		return types.Null
	case len(vals) == 1 && !missing:
		return vals[0]
	}

	gathered := types.MakeArray(len(vals) + 1)
	for _, v := range vals {
		if arr, ok := v.(*types.Array); ok {
			for i := 0; i < arr.Len(); i++ {
				gathered.Append(must.NotFail(arr.Get(i)))
			}
			continue
		}
		gathered.Append(v)
	}
	if missing {
		gathered.Append(types.Null)
	}
	return gathered
}

// IndexKeyValues returns the keys MongoDB generates for doc in an index on
// path. A missing field, a scalar before the path ends, an empty array before
// the path ends, and a non-document element of an array before the path ends
// each produce null. At the end of the path an empty array produces undefined
// and any other array produces its elements.
func IndexKeyValues(doc *types.Document, path types.Path) []any {
	return appendIndexKeyValues(nil, doc, path.Slice())
}

func appendIndexKeyValues(out []any, v any, keys []string) []any {
	if len(keys) == 0 {
		arr, ok := v.(*types.Array)
		switch {
		case !ok:
			return append(out, v)
		case arr.Len() == 0:
			return append(out, types.Undefined)
		}
		for i := 0; i < arr.Len(); i++ {
			el, _ := arr.Get(i)
			out = append(out, el)
		}
		return out
	}

	switch v := v.(type) {
	case *types.Document:
		next, err := v.Get(keys[0])
		if err != nil {
			return append(out, types.Null)
		}
		return appendIndexKeyValues(out, next, keys[1:])

	case *types.Array:
		if v.Len() == 0 {
			return append(out, types.Null)
		}
		for i := 0; i < v.Len(); i++ {
			el, _ := v.Get(i)
			if elDoc, ok := el.(*types.Document); ok {
				out = appendIndexKeyValues(out, elDoc, keys)
			} else {
				out = append(out, types.Null)
			}
		}
		return out

	default:
		return append(out, types.Null)
	}
}
