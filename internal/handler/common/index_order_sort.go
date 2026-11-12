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
	"strings"

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
	if branches := orBranchesInPlanOrder(filter, pattern.Keys()[0]); branches != nil {
		return sortByOrBranches(docs, pattern, branches, cmp)
	}
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
			if admits[i] = elementAdmitter(boundedByFewerKeys(docs, filter, field, path, cmp), field, cmp); admits[i] == nil {
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
			if t := conditionAdmitter(v, cmp); t != nil {
				tests = append(tests, t)
			}
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

// boundedByFewerKeys returns filter with field's lower or upper comparison
// dropped when it has both: a multikey walk takes one bound, and mongod's plan
// trial keeps the one whose range holds fewer keys, the upper on a tie.
func boundedByFewerKeys(docs []*types.Document, filter *types.Document, field string, path types.Path, cmp *collation.Comparator) *types.Document {
	v, err := filter.Get(field)
	ops, ok := v.(*types.Document)
	if err != nil || !ok {
		return filter
	}
	lower, upper := firstOf(ops, "$gt", "$gte"), firstOf(ops, "$lt", "$lte")
	// A $type naming arrays scans every key, whichever bound is kept.
	if lower == "" || upper == "" || conditionAdmitter(ops, cmp) == nil {
		return filter
	}
	without := func(op string) *types.Document {
		cond := ops.DeepCopy()
		cond.Remove(op)
		f := filter.DeepCopy()
		f.Set(field, cond)
		return f
	}
	keysIn := func(f *types.Document) int {
		admit := conditionAdmitter(must.NotFail(f.Get(field)), cmp)
		n := 0
		for _, doc := range docs {
			v := commonpath.SortKeyValue(doc, path)
			if arr, isArr := v.(*types.Array); isArr {
				for j := 0; j < arr.Len(); j++ {
					if admit(must.NotFail(arr.Get(j))) {
						n++
					}
				}
			} else if admit(v) {
				n++
			}
		}
		return n
	}
	upperOnly, lowerOnly := without(lower), without(upper)
	if keysIn(lowerOnly) < keysIn(upperOnly) {
		return lowerOnly
	}
	return upperOnly
}

// firstOf returns the first of names that ops holds, or "".
func firstOf(ops *types.Document, names ...string) string {
	for _, n := range names {
		if ops.Has(n) {
			return n
		}
	}
	return ""
}

// orBranchesInPlanOrder returns the branches of a filter that is a lone $or,
// in the order mongod's OR stage runs them on an index led by field, or nil
// when mongod reads the index in one scan: when a branch does not bound field,
// so the whole index is scanned, or when each branch is one comparison of
// field, whose bounds merge. Such single comparisons beside other branches
// merge into one scan that runs last.
func orBranchesInPlanOrder(filter *types.Document, field string) []*types.Document {
	if filter.Len() != 1 || filter.Keys()[0] != "$or" {
		return nil
	}
	branches := filterBranches(must.NotFail(filter.Get("$or")))
	if len(branches) < 2 {
		return nil
	}
	var ordered []*types.Document
	leaves := types.MakeArray(0)
	for _, b := range branches {
		if !b.Has(field) {
			return nil
		}
		if b.Len() == 1 && isSingleComparison(must.NotFail(b.Get(field))) {
			leaves.Append(b)
		} else {
			ordered = append(ordered, b)
		}
	}
	if len(ordered) == 0 {
		return nil
	}
	slices.SortStableFunc(ordered, func(a, b *types.Document) int {
		if c := branchRank(a) - branchRank(b); c != 0 {
			return c
		}
		return strings.Compare(strings.Join(a.Keys(), "."), strings.Join(b.Keys(), "."))
	})
	if leaves.Len() > 0 {
		ordered = append(ordered, must.NotFail(types.NewDocument("$or", leaves)))
	}
	return ordered
}

// isSingleComparison returns whether field condition v is one predicate; a
// $regex with its $options is one.
func isSingleComparison(v any) bool {
	ops, isOps := v.(*types.Document)
	if !isOps || ops.Len() <= 1 || !strings.HasPrefix(ops.Keys()[0], "$") {
		return true
	}
	return ops.Len() == 2 && ops.Has("$regex") && ops.Has("$options")
}

// branchRank is an $or branch's place in mongod's canonical order of match
// expressions: conjunctions first, then each predicate by its kind.
func branchRank(b *types.Document) int {
	if b.Len() != 1 {
		return 0
	}
	key := b.Keys()[0]
	switch key {
	case "$and":
		return 0
	case "$or":
		return 1
	case "$nor":
		return 30
	}
	ops, ok := must.NotFail(b.Get(key)).(*types.Document)
	if !ok || ops.Len() == 0 || !strings.HasPrefix(ops.Keys()[0], "$") {
		return 5
	}
	if ops.Len() > 1 {
		return 0
	}
	switch ops.Keys()[0] {
	case "$elemMatch":
		return 2
	case "$size":
		return 4
	case "$eq":
		return 5
	case "$lte":
		return 6
	case "$lt":
		return 7
	case "$gt":
		return 8
	case "$gte":
		return 9
	case "$regex":
		return 10
	case "$mod":
		return 11
	case "$exists":
		return 12
	case "$in":
		return 13
	case "$not":
		return 25
	case "$type":
		return 31
	}
	return 20
}

// sortByOrBranches orders docs as mongod's OR stage returns them: the
// documents of each branch in turn, each in the index order its own bounds
// give, a document only at the first branch it matches.
func sortByOrBranches(docs []*types.Document, pattern *types.Document, branches []*types.Document, cmp *collation.Comparator) error {
	groups := make([][]*types.Document, len(branches)+1)
	for _, doc := range docs {
		g := len(branches)
		for i, b := range branches {
			if ok, err := FilterDocumentColl(doc, b, cmp); err == nil && ok {
				g = i
				break
			}
		}
		groups[g] = append(groups[g], doc)
	}
	out := docs[:0:0]
	for i, group := range groups {
		branch := types.MakeDocument(0)
		if i < len(branches) {
			branch = branches[i]
		}
		if err := sortDocumentsInIndexOrder(group, pattern, branch, cmp); err != nil {
			return err
		}
		out = append(out, group...)
	}
	copy(docs, out)
	return nil
}

// rangeOperatorOrder is the order mongod's canonical form sorts comparison
// operators in; on a multikey field it bounds the walk by the first alone.
var rangeOperatorOrder = []string{"$eq", "$lte", "$lt", "$gt", "$gte"}

// conditionAdmitter returns whether field condition v's bounds hold an array
// element, or nil when they do not bound the field: a $type that includes
// arrays scans every key. A nested array is its own key, so only an equal
// array admits it. Of several comparisons only the first in canonical order
// bounds a multikey field.
func conditionAdmitter(v any, cmp *collation.Comparator) func(any) bool {
	cond := v
	// Bounds that span every type, as from $ne, $nin and $exists, hold a
	// nested array's key too.
	spanAll := false
	if ops, ok := v.(*types.Document); ok && ops.Len() > 0 && strings.HasPrefix(ops.Keys()[0], "$") {
		typeArray := false
		if t, err := ops.Get("$type"); err == nil && typeIncludesArray(t) {
			typeArray = true
		}
		bounding := false
		for _, op := range ops.Keys() {
			switch op {
			case "$ne", "$nin", "$exists", "$not":
			case "$type":
				bounding = bounding || !typeArray
			default:
				bounding = true
			}
		}
		if typeArray && !bounding {
			return nil
		}
		spanAll = !bounding
		reduced := types.MakeDocument(ops.Len())
		for _, op := range rangeOperatorOrder {
			if ov, err := ops.Get(op); err == nil {
				reduced.Set(op, ov)
				break
			}
		}
		for _, op := range ops.Keys() {
			if !slices.Contains(rangeOperatorOrder, op) && !(op == "$type" && typeArray) {
				reduced.Set(op, must.NotFail(ops.Get(op)))
			}
		}
		cond = reduced
	}
	condDoc := must.NotFail(types.NewDocument("v", cond))
	equal, isEquality := equalityOperand(v)
	return func(elem any) bool {
		if nested, ok := elem.(*types.Array); ok {
			if spanAll {
				return true
			}
			return isEquality && types.Compare(nested, equal) == types.Equal
		}
		ok, err := FilterDocumentColl(must.NotFail(types.NewDocument("v", elem)), condDoc, cmp)
		return err == nil && ok
	}
}

// typeIncludesArray reports whether a $type operand names the array type.
func typeIncludesArray(t any) bool {
	switch t := t.(type) {
	case string:
		return t == "array"
	case int32, int64, float64:
		return types.Compare(t, int32(4)) == types.Equal
	case *types.Array:
		for i := 0; i < t.Len(); i++ {
			if typeIncludesArray(must.NotFail(t.Get(i))) {
				return true
			}
		}
	}
	return false
}

// equalityOperand returns the value a bare or $eq condition matches.
func equalityOperand(v any) (any, bool) {
	ops, ok := v.(*types.Document)
	if !ok {
		return v, true
	}
	if ops.Len() == 1 && ops.Keys()[0] == "$eq" {
		return must.NotFail(ops.Get("$eq")), true
	}
	if ops.Len() > 0 && strings.HasPrefix(ops.Keys()[0], "$") {
		return nil, false
	}
	return v, true
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
