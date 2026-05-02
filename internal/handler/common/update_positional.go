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
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// hasPositionalOp returns true if the key contains any positional array operator
// ($, $[], or $[identifier]).
func hasPositionalOp(key string) bool {
	return strings.Contains(key, ".$")
}

// expandPositionalOps expands positional array operators in key to concrete dot-notation paths.
//
// Supported operators:
//   - $ (positional first): replaced with the index of the first array element matching filter.
//   - $[] (all positional): expanded to one path per array element.
//   - $[identifier] (filtered positional): expanded to paths for elements matching arrayFilters.
//
// Returns nil (empty slice) when no elements match (e.g. $ with no filter match).
// Returns the original key (single-element slice) if no positional operator is found.
func expandPositionalOps(key string, doc *types.Document, filter *types.Document, arrayFilters *types.Array) ([]string, error) {
	parts := strings.Split(key, ".")

	for i, part := range parts {
		switch {
		case strings.HasPrefix(part, "$[") && strings.HasSuffix(part, "]"):
			// Handles both $[] (all positional) and $[identifier] (filtered positional).
			identifier := part[2 : len(part)-1]
			arrayPath := strings.Join(parts[:i], ".")

			if identifier == "" {
				// $[]  -- all positional
				arr, err := getArrayByPath(doc, arrayPath)
				if err != nil {
					return nil, lazyerrors.Error(err)
				}
				if arr == nil {
					return nil, nil
				}
				result := make([]string, arr.Len())
				for j := range arr.Len() {
					expanded := make([]string, len(parts))
					copy(expanded, parts)
					expanded[i] = strconv.Itoa(j)
					result[j] = strings.Join(expanded, ".")
				}
				return result, nil
			}

			// $[identifier]  -- filtered positional
			arr, err := getArrayByPath(doc, arrayPath)
			if err != nil {
				return nil, lazyerrors.Error(err)
			}
			if arr == nil {
				return nil, nil
			}
			indices, err := findFilteredIndices(arr, identifier, arrayFilters)
			if err != nil {
				return nil, err
			}
			result := make([]string, 0, len(indices))
			for _, idx := range indices {
				expanded := make([]string, len(parts))
				copy(expanded, parts)
				expanded[i] = strconv.Itoa(idx)
				result = append(result, strings.Join(expanded, "."))
			}
			return result, nil

		case part == "$":
			// $  -- positional first match
			arrayPath := strings.Join(parts[:i], ".")
			idx, err := findFirstMatchingIndex(arrayPath, doc, filter)
			if err != nil {
				return nil, err
			}
			if idx < 0 {
				// No matching element found  -- no update.
				return nil, nil
			}
			expanded := make([]string, len(parts))
			copy(expanded, parts)
			expanded[i] = strconv.Itoa(idx)
			return []string{strings.Join(expanded, ".")}, nil
		}
	}

	return []string{key}, nil
}

// getArrayByPath returns the *types.Array at the given dot-notation path in doc.
// Returns nil (no error) if the path is empty, the path does not exist, or the value is not an array.
func getArrayByPath(doc *types.Document, path string) (*types.Array, error) {
	if path == "" {
		return nil, nil
	}

	p, err := types.NewPathFromString(path)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	val, err := doc.GetByPath(p)
	if err != nil {
		return nil, nil //nolint:nilerr // path not found is not an error here
	}

	arr, ok := val.(*types.Array)
	if !ok {
		return nil, nil
	}

	return arr, nil
}

// findFirstMatchingIndex finds the index of the first array element in the document
// at arrayPath that satisfies the query filter conditions for that path.
//
// Returns -1 if no matching element is found or the filter has no conditions for the array.
func findFirstMatchingIndex(arrayPath string, doc *types.Document, filter *types.Document) (int, error) {
	if filter == nil {
		return -1, nil
	}

	arr, err := getArrayByPath(doc, arrayPath)
	if err != nil {
		return -1, err
	}
	if arr == nil {
		return -1, nil
	}

	// Collect filter conditions that apply to this array field.
	relevantFilter := must.NotFail(types.NewDocument())

	filterIter := filter.Iterator()
	defer filterIter.Close()

	for {
		filterKey, filterVal, iterErr := filterIter.Next()
		if errors.Is(iterErr, iterator.ErrIteratorDone) {
			break
		}
		if iterErr != nil {
			return -1, lazyerrors.Error(iterErr)
		}

		if filterKey == arrayPath || strings.HasPrefix(filterKey, arrayPath+".") {
			relevantFilter.Set(filterKey, filterVal)
		}
	}

	if relevantFilter.Len() == 0 {
		return -1, nil
	}

	// Test each element against the relevant filter conditions.
	for i := range arr.Len() {
		elem := must.NotFail(arr.Get(i))

		var testDoc *types.Document
		if strings.ContainsRune(arrayPath, '.') {
			testDoc = buildNestedDoc(arrayPath, elem)
		} else {
			testDoc = must.NotFail(types.NewDocument(arrayPath, elem))
		}

		matched, matchErr := FilterDocument(testDoc, relevantFilter)
		if matchErr != nil {
			return -1, lazyerrors.Error(matchErr)
		}

		if matched {
			return i, nil
		}
	}

	return -1, nil
}

// buildNestedDoc creates a *types.Document where value is nested at the given
// dot-notation path. For example, buildNestedDoc("a.b", v) returns {a: {b: v}}.
func buildNestedDoc(path string, value any) *types.Document {
	parts := strings.Split(path, ".")
	var current any = value

	for i := len(parts) - 1; i >= 0; i-- {
		current = must.NotFail(types.NewDocument(parts[i], current))
	}

	return current.(*types.Document)
}

// findFilteredIndices returns the indices of array elements that match the
// filter condition for the given identifier in arrayFilters.
//
// The arrayFilters array contains filter documents keyed by identifier, e.g.:
//
//	[{elem.grade: "B"}]  →  matches elements where .grade == "B" for identifier "elem"
//	[{x: {$lt: 65}}]    →  matches elements where element < 65 for identifier "x"
func findFilteredIndices(arr *types.Array, identifier string, arrayFilters *types.Array) ([]int, error) {
	if arrayFilters == nil {
		return nil, fmt.Errorf("no array filter found for identifier $[%s]", identifier)
	}

	// Find the filter document for this identifier.
	var identifierFilter *types.Document
	for i := range arrayFilters.Len() {
		filterDoc, ok := must.NotFail(arrayFilters.Get(i)).(*types.Document)
		if !ok {
			continue
		}

		for _, key := range filterDoc.Keys() {
			if key == identifier || strings.HasPrefix(key, identifier+".") {
				identifierFilter = filterDoc
				break
			}
		}

		if identifierFilter != nil {
			break
		}
	}

	if identifierFilter == nil {
		return nil, fmt.Errorf("no array filter found for identifier $[%s]", identifier)
	}

	var result []int

	for i := range arr.Len() {
		elem := must.NotFail(arr.Get(i))

		// Wrap element as {identifier: elem} so that filter keys like
		// "identifier" or "identifier.subfield" resolve correctly.
		testDoc := must.NotFail(types.NewDocument(identifier, elem))

		matched, err := FilterDocument(testDoc, identifierFilter)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		if matched {
			result = append(result, i)
		}
	}

	return result, nil
}
