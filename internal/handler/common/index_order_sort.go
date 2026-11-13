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
	"context"
	"errors"
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

// AsOneIndexScan returns the filter SortIteratorInIndexOrder reads for one
// unbounded scan of the whole index: none, so an array is reached at its
// least element, not at one in a branch's bounds.
func AsOneIndexScan(*types.Document) *types.Document {
	return types.MakeDocument(0)
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
			oneBound, _ := boundedByFewerKeys(filter, field, func(lower, upper *types.Document) (bool, error) {
				return countAdmittedKeys(docs, path, lower, cmp) < countAdmittedKeys(docs, path, upper, cmp), nil
			}, cmp)
			if admits[i] = elementAdmitter(oneBound, field, cmp); admits[i] == nil {
				bounded = false
			}
		}
	}

	keys := make([][]any, len(docs))
	for d, doc := range docs {
		row := make([]any, len(fields))
		for i := range fields {
			row[i] = indexKeyValue(indexKeys(doc, paths[i]), admits[i])
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

// indexKeys is doc's index keys on path: one key alone, or several, or a
// nested array key, gathered in an array.
func indexKeys(doc *types.Document, path types.Path) any {
	keys := commonpath.IndexKeyValues(doc, path)
	if len(keys) == 1 {
		if _, nested := keys[0].(*types.Array); !nested {
			return keys[0]
		}
	}
	arr := types.MakeArray(len(keys))
	for _, k := range keys {
		arr.Append(k)
	}
	return arr
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
// dropped when it has both, at the top level or in the one $and branch that
// holds field: a multikey walk takes one bound, and mongod's plan trial keeps
// one. keepLower decides between the lower and upper one-sided conditions.
func boundedByFewerKeys(filter *types.Document, field string, keepLower func(lower, upper *types.Document) (bool, error), cmp *collation.Comparator) (*types.Document, error) {
	v, err := filter.Get(field)
	if err != nil {
		return boundedByFewerKeysUnderAnd(filter, field, keepLower, cmp)
	}
	ops, ok := v.(*types.Document)
	if !ok {
		return filter, nil
	}
	lower, upper := firstOf(ops, "$gt", "$gte"), firstOf(ops, "$lt", "$lte")
	// A $type naming arrays scans every key, whichever bound is kept.
	if lower == "" || upper == "" || conditionAdmitter(ops, cmp) == nil {
		return filter, nil
	}
	without := func(op string) *types.Document {
		cond := ops.DeepCopy()
		cond.Remove(op)
		return cond
	}
	upperOnly, lowerOnly := without(lower), without(upper)
	useLower, err := keepLower(lowerOnly, upperOnly)
	if err != nil {
		return nil, err
	}
	kept := upperOnly
	if useLower {
		kept = lowerOnly
	}
	f := filter.DeepCopy()
	f.Set(field, kept)
	return f, nil
}

func boundedByFewerKeysUnderAnd(filter *types.Document, field string, keepLower func(lower, upper *types.Document) (bool, error), cmp *collation.Comparator) (*types.Document, error) {
	and, err := filter.Get("$and")
	branches := filterBranches(and)
	if err != nil || branches == nil {
		return filter, nil
	}
	holder := -1
	for i, b := range branches {
		if andHolds(b, field) {
			if holder >= 0 {
				return filter, nil
			}
			holder = i
		}
	}
	if holder < 0 {
		return filter, nil
	}
	reduced, err := boundedByFewerKeys(branches[holder], field, keepLower, cmp)
	if err != nil || reduced == branches[holder] {
		return filter, err
	}
	arr := types.MakeArray(len(branches))
	for i, b := range branches {
		if i == holder {
			b = reduced
		}
		arr.Append(b)
	}
	f := filter.DeepCopy()
	f.Set("$and", arr)
	return f, nil
}

// Plan trial limits, as mongod's internalQueryPlanEvaluationWorks,
// internalQueryPlanEvaluationCollFraction and
// internalQueryPlanEvaluationMaxResults.
const (
	trialMinWorks    = 10000
	trialCollFrac    = 0.3
	TrialMaxAdvanced = 101
)

// TrialBudget bounds the emulated plan trials of one query: PlanWorks is the
// work each plan may do, Works what all of the query's trials may still do,
// and NumToReturn the results that end a plan, as a limit does.
type TrialBudget struct {
	PlanWorks   int
	Works       int
	NumToReturn int
}

// NewTrialBudget is the budget of a query over a collection of count
// documents, which mongod gives each plan; the query's trials share two plans'
// worth.
func NewTrialBudget(count int64, numToReturn int) *TrialBudget {
	works := max(trialMinWorks, int(trialCollFrac*float64(count)))
	if numToReturn <= 0 || numToReturn > TrialMaxAdvanced {
		numToReturn = TrialMaxAdvanced
	}
	return &TrialBudget{PlanWorks: works, Works: 2 * works, NumToReturn: numToReturn}
}

// BoundedByTrial returns filter with only the bound of a two-sided range on
// multikey field that mongod's plan trial keeps. Each bound's scan reads, in
// index order, the documents rangeIter returns for its one-sided condition;
// a member document costs a work per distinct key it holds in range and one
// to fetch, and advances when it matches filter. Returning
// budget.NumToReturn documents ends a scan. The scans take turns by work done
// until both end, or the budget, charged for every document read, is spent;
// once one ends the other runs only while it could still end with less work.
// When both end the one with less work is kept, else the one that ended,
// else the one with more results per work; the upper on a tie.
func BoundedByTrial(ctx context.Context, filter *types.Document, field string, rangeIter func(cond *types.Document) (iter types.DocumentsIterator, member *types.Document, err error), budget *TrialBudget, cmp *collation.Comparator) (*types.Document, error) {
	path, err := types.NewPathFromString(field)
	if err != nil {
		return filter, nil
	}
	return boundedByFewerKeys(filter, field, func(lower, upper *types.Document) (bool, error) {
		plans := [2]*trialPlan{{cond: lower}, {cond: upper}}
		for _, p := range plans {
			if p.iter, p.member, err = rangeIter(p.cond); err != nil {
				return false, err
			}
			defer p.iter.Close()
			p.admit = conditionAdmitter(p.cond, cmp)
		}
		for budget.Works > 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			// Once a scan ends, the other runs only while it could still end
			// with less work.
			ended := -1
			for _, p := range plans {
				if p.eof && (ended < 0 || p.works < ended) {
					ended = p.works
				}
			}
			var next *trialPlan
			for _, p := range plans {
				if p.eof || p.cost >= budget.PlanWorks || ended >= 0 && p.works >= ended {
					continue
				}
				if next == nil || p.works < next.works {
					next = p
				}
			}
			if next == nil {
				break
			}
			before := next.cost
			if err := next.step(path, filter, cmp, budget.NumToReturn); err != nil {
				return false, err
			}
			budget.Works -= next.cost - before
		}
		switch {
		case plans[0].eof && plans[1].eof:
			return plans[0].works < plans[1].works, nil
		case plans[0].eof || plans[1].eof:
			return plans[0].eof, nil
		}
		return plans[0].productivity() > plans[1].productivity(), nil
	}, cmp)
}

// trialPlan is one bound's scan in an emulated plan trial.
type trialPlan struct {
	cond     *types.Document
	member   *types.Document
	iter     types.DocumentsIterator
	admit    func(any) bool
	works    int
	cost     int
	advanced int
	eof      bool
	// keys holds the in-range keys of the documents read, ascending.
	keys    []any
	fetched int
}

func (p *trialPlan) step(path types.Path, filter *types.Document, cmp *collation.Comparator, numToReturn int) error {
	_, doc, err := p.iter.Next()
	if errors.Is(err, iterator.ErrIteratorDone) {
		p.eof = true
		p.works = len(p.keys) + p.fetched + 1
		p.cost++
		return nil
	}
	if err != nil {
		return err
	}
	p.cost++
	if p.member != nil {
		if in, ferr := FilterDocumentColl(doc, p.member, cmp); ferr != nil || !in {
			return nil
		}
	}
	docKeys := distinctAdmittedKeyValues(doc, path, p.admit)
	if len(docKeys) == 0 {
		return nil
	}
	p.cost += len(docKeys)
	// The scan reaches doc at its least key in range, having examined every
	// key read so far up to it and fetched each document reached.
	first := docKeys[0]
	for _, k := range docKeys {
		at, _ := slices.BinarySearchFunc(p.keys, k, func(a, b any) int { return int(compareIndexOrderKeys(a, b, types.Ascending, cmp)) })
		p.keys = slices.Insert(p.keys, at, k)
		if compareIndexOrderKeys(k, first, types.Ascending, cmp) == types.Less {
			first = k
		}
	}
	p.fetched++
	examined, _ := slices.BinarySearchFunc(p.keys, first, func(a, b any) int {
		if compareIndexOrderKeys(a, b, types.Ascending, cmp) == types.Greater {
			return 1
		}
		return -1
	})
	p.works = examined + p.fetched
	if ok, ferr := FilterDocumentColl(doc, filter, cmp); ferr == nil && ok {
		p.advanced++
		p.eof = p.advanced >= numToReturn
	}
	return nil
}

func (p *trialPlan) productivity() float64 {
	if p.works == 0 {
		return 0
	}
	return float64(p.advanced) / float64(p.works)
}

// distinctAdmittedKeyValues returns doc's distinct index keys on path that
// admit accepts, as an index stores each once per document.
func distinctAdmittedKeyValues(doc *types.Document, path types.Path, admit func(any) bool) []any {
	var counted []any
	for _, key := range commonpath.IndexKeyValues(doc, path) {
		if !admit(key) || slices.ContainsFunc(counted, func(c any) bool { return types.Compare(c, key) == types.Equal }) {
			continue
		}
		counted = append(counted, key)
	}
	return counted
}

// distinctAdmittedKeys counts doc's distinct index keys on path that admit
// accepts, as an index stores each once per document.
func distinctAdmittedKeys(doc *types.Document, path types.Path, admit func(any) bool) int {
	var counted []any
	for _, key := range commonpath.IndexKeyValues(doc, path) {
		if !admit(key) || slices.ContainsFunc(counted, func(c any) bool { return types.Compare(c, key) == types.Equal }) {
			continue
		}
		counted = append(counted, key)
	}
	return len(counted)
}

// countAdmittedKeys is the work a scan of the keys cond admits does over docs'
// index keys on path: one per distinct key and one per document fetched.
func countAdmittedKeys(docs []*types.Document, path types.Path, cond *types.Document, cmp *collation.Comparator) int {
	admit := conditionAdmitter(cond, cmp)
	n := 0
	for _, doc := range docs {
		if k := distinctAdmittedKeys(doc, path, admit); k > 0 {
			n += k + 1
		}
	}
	return n
}

// andHolds reports whether filter holds field at its top level or under $and.
func andHolds(filter *types.Document, field string) bool {
	if filter.Has(field) {
		return true
	}
	and, _ := filter.Get("$and")
	for _, b := range filterBranches(and) {
		if andHolds(b, field) {
			return true
		}
	}
	return false
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
