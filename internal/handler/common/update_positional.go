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
	"sort"
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
			idx, err := positionalMatchIndex(arrayPath, doc, filter)
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

// positionalMatchIndex returns the array index that the positional $ operator
// names for the array at arrayPath, as mongod picks it: each filter predicate
// on the array that matches an element records that element's index, and the
// last one in mongod's evaluation order wins.
//
// Returns -1 if no predicate records an index.
func positionalMatchIndex(arrayPath string, doc *types.Document, filter *types.Document) (int, error) {
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

	var preds []positionPredicate

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
			if preds, err = appendPositionPredicates(preds, filterKey, filterVal); err != nil {
				return -1, err
			}
		}
	}

	sort.SliceStable(preds, func(i, j int) bool {
		if preds[i].rank != preds[j].rank {
			return preds[i].rank < preds[j].rank
		}
		return preds[i].path < preds[j].path
	})

	suffix := arrayPath
	if dot := strings.LastIndexByte(arrayPath, '.'); dot >= 0 {
		suffix = arrayPath[dot+1:]
	}

	for p := len(preds) - 1; p >= 0; p-- {
		pred := preds[p]
		for i := range arr.Len() {
			elem := must.NotFail(arr.Get(i))

			var matched bool
			if pred.elemMatch != nil && pred.path == arrayPath {
				matched, err = elemMatchElement(elem, arrayPath, suffix, pred.elemMatch, pred.pureOperator, nil)
			} else {
				matched, err = FilterDocument(buildNestedDoc(arrayPath, elem), pred.filter)
			}
			if err != nil {
				return -1, lazyerrors.Error(err)
			}
			if matched {
				return i, nil
			}
		}
	}

	return -1, nil
}

// positionPredicate is one predicate of the filter's top-level conjunction on
// an array path, as mongod evaluates it after optimizing the query.
type positionPredicate struct {
	rank int
	path string

	// filter is {path: condition}, for every predicate but an $elemMatch on
	// the array itself.
	filter *types.Document

	elemMatch    *types.Document
	pureOperator bool
}

// Ranks follow mongod's MatchExpression::MatchType, by which it sorts the
// children of an AND before matching.
const (
	rankElemMatchObject = iota
	rankElemMatchValue
	rankEq
	rankLte
	rankLt
	rankGt
	rankGte
	rankRegex
	rankMod
	rankExists
	rankIn
	rankBitsAllSet
	rankBitsAllClear
	rankBitsAnySet
	rankBitsAnyClear
	rankType
	rankGeo
)

// positionOperatorRanks holds the operators that record an array index.
// Negations ($ne, $nin, $not) and $size never do.
var positionOperatorRanks = map[string]int{
	"$eq":            rankEq,
	"$lte":           rankLte,
	"$lt":            rankLt,
	"$gt":            rankGt,
	"$gte":           rankGte,
	"$regex":         rankRegex,
	"$mod":           rankMod,
	"$exists":        rankExists,
	"$in":            rankIn,
	"$bitsAllSet":    rankBitsAllSet,
	"$bitsAllClear":  rankBitsAllClear,
	"$bitsAnySet":    rankBitsAnySet,
	"$bitsAnyClear":  rankBitsAnyClear,
	"$type":          rankType,
	"$geoWithin":     rankGeo,
	"$geoIntersects": rankGeo,
}

// appendPositionPredicates appends the predicates of {path: value}, split the
// way mongod's optimizer splits them: one per operator, a one-value $in as an
// equality or regex, and each $all value as its own predicate.
func appendPositionPredicates(preds []positionPredicate, path string, value any) ([]positionPredicate, error) {
	single := func(rank int, cond any) positionPredicate {
		return positionPredicate{rank: rank, path: path, filter: must.NotFail(types.NewDocument(path, cond))}
	}
	literal := func(v any) positionPredicate {
		if _, isRegex := v.(types.Regex); isRegex {
			return single(rankRegex, v)
		}
		return single(rankEq, must.NotFail(types.NewDocument("$eq", v)))
	}

	ops, ok := value.(*types.Document)
	if !ok || !isOperatorDocument(ops) || isDBRefLiteral(ops) {
		return append(preds, literal(value)), nil
	}

	for _, op := range ops.Keys() {
		v := must.NotFail(ops.Get(op))
		switch op {
		case "$elemMatch":
			expr, ok := v.(*types.Document)
			if !ok {
				continue
			}
			pred, err := elemMatchPositionPredicate(path, expr)
			if err != nil {
				return nil, err
			}
			preds = append(preds, pred)

		case "$all":
			values, ok := v.(*types.Array)
			if !ok {
				continue
			}
			for i := range values.Len() {
				item := must.NotFail(values.Get(i))
				if inner, ok := elemMatchOperand(item); ok {
					pred, err := elemMatchPositionPredicate(path, inner)
					if err != nil {
						return nil, err
					}
					preds = append(preds, pred)
					continue
				}
				preds = append(preds, literal(item))
			}

		case "$in":
			if values, ok := v.(*types.Array); ok && values.Len() == 1 {
				preds = append(preds, literal(must.NotFail(values.Get(0))))
				continue
			}
			preds = append(preds, single(rankIn, must.NotFail(types.NewDocument(op, v))))

		case "$regex":
			cond := must.NotFail(types.NewDocument(op, v))
			if options, err := ops.Get("$options"); err == nil {
				cond.Set("$options", options)
			}
			preds = append(preds, single(rankRegex, cond))

		default:
			if rank, ok := positionOperatorRanks[op]; ok {
				preds = append(preds, single(rank, must.NotFail(types.NewDocument(op, v))))
			}
		}
	}

	return preds, nil
}

// elemMatchPositionPredicate returns the predicate of {path: {$elemMatch: expr}}.
func elemMatchPositionPredicate(path string, expr *types.Document) (positionPredicate, error) {
	pureOperator, err := elemMatchKind(expr)
	if err != nil {
		return positionPredicate{}, err
	}

	rank := rankElemMatchValue
	if !pureOperator || expr.Len() == 0 {
		rank = rankElemMatchObject
	}

	return positionPredicate{
		rank:         rank,
		path:         path,
		filter:       must.NotFail(types.NewDocument(path, must.NotFail(types.NewDocument("$elemMatch", expr)))),
		elemMatch:    expr,
		pureOperator: pureOperator,
	}, nil
}

// elemMatchOperand returns expr of an {$elemMatch: expr} value.
func elemMatchOperand(v any) (*types.Document, bool) {
	doc, ok := v.(*types.Document)
	if !ok || doc.Len() != 1 || doc.Keys()[0] != "$elemMatch" {
		return nil, false
	}
	expr, ok := must.NotFail(doc.Get("$elemMatch")).(*types.Document)
	return expr, ok
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
//	[{elem.grade: "B"}]  ->  matches elements where .grade == "B" for identifier "elem"
//	[{x: {$lt: 65}}]    ->  matches elements where element < 65 for identifier "x"
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
