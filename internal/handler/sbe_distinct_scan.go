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

package handler

import (
	"context"
	"slices"
	"strings"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// distinctScanGroup is a $group mongod 8.0 runs in its classic engine as a
// $groupByDistinctScan over a DISTINCT_SCAN of an index on its key.
type distinctScanGroup struct {
	// group is the position of the $group in the pipeline's stages.
	group int
	index backends.IndexInfo
	// key is the grouped field path, and keyFilter the conditions the
	// query puts on it.
	key       string
	keyFilter *types.Document
	// newRoot is the document $groupByDistinctScan makes of each group's
	// chosen document.
	newRoot *types.Document
	// fetch reports whether the plan fetches each chosen document.
	fetch bool
	// projection is the projection stage over the scan, "" for none.
	projection string
	// backward reports whether the scan reads the index from its end.
	backward bool
}

// findDistinctScanGroup returns the $group of specs, the stages of a pipeline
// reading the existing collection named collection, that mongod runs as a
// distinct scan, or nil. Only $match and $sort stages may come before it.
func findDistinctScanGroup(ctx context.Context, db backends.Database, collection string, specs []*types.Document, queryCollation *types.Document) *distinctScanGroup { //nolint:lll // for readability
	var conjuncts, filters []*types.Document
	var sort *types.Document
	pos := 0
	for ; pos < len(specs); pos++ {
		spec := specs[pos]
		if spec == nil || spec.Len() != 1 {
			return nil
		}
		switch spec.Command() {
		case "$match":
			filter, _ := must.NotFail(spec.Get("$match")).(*types.Document)
			if filter == nil {
				return nil
			}
			var ok bool
			if conjuncts, ok = appendFieldConjuncts(conjuncts, filter); !ok {
				return nil
			}
			filters = append(filters, filter)
			continue
		case "$sort":
			if sort != nil {
				return nil
			}
			sort, _ = must.NotFail(spec.Get("$sort")).(*types.Document)
			if sort == nil || sort.Len() == 0 {
				return nil
			}
			continue
		case "$group":
		default:
			return nil
		}
		break
	}
	if pos == len(specs) {
		return nil
	}
	group, _ := must.NotFail(specs[pos].Get("$group")).(*types.Document)
	if group == nil {
		return nil
	}
	key, idExpr, ok := distinctGroupKey(group)
	if !ok {
		return nil
	}

	sortDir := 1
	if sort != nil {
		if sort.Keys()[0] != key {
			return nil
		}
		sortDir = sortDirection(must.NotFail(sort.Get(key)))
		if sortDir == 0 {
			return nil
		}
	}
	outputs, ok := distinctGroupOutputs(group, key, idExpr, sort, sortDir)
	if !ok {
		return nil
	}
	var filterFields []string
	var keyFilter *types.Document
	for _, c := range conjuncts {
		filterFields = append(filterFields, c.Keys()[0])
		if c.Keys()[0] == key {
			keyFilter = andFilters(keyFilter, c)
		}
	}
	if len(filterFields) > 0 && keyFilter == nil {
		return nil
	}

	info, err := lookupCollectionInfo(ctx, db, collection)
	if err != nil || info == nil || info.IsView {
		return nil
	}
	coll, err := db.Collection(collection)
	if err != nil {
		return nil
	}
	res, err := coll.ListIndexes(ctx, new(backends.ListIndexesParams))
	if err != nil {
		return nil
	}
	var merged *types.Document
	for _, f := range filters {
		merged = andFilters(merged, f)
	}
	want := collation.Parse(queryCollation).Identity()
	var best *distinctScanGroup
	for _, idx := range res.Indexes {
		if !groupScansIndex(ctx, idx, key, info.ClusteredIndexName != "", filterFields, merged, sort, queryCollation) ||
			collation.Parse(idx.Collation).Identity() != want {
			continue
		}
		covered := outputs.coverable
		for _, f := range outputs.paths {
			if !slices.ContainsFunc(idx.Key, func(k backends.IndexKeyPair) bool { return k.Field == f }) {
				covered = false
			}
		}
		ds := &distinctScanGroup{group: pos, index: idx, key: key, keyFilter: keyFilter, newRoot: outputs.newRoot, fetch: !covered,
			backward: (outputs.scanDir < 0) != idx.Key[0].Descending}
		switch {
		case covered && strings.Contains(key, "."):
			ds.projection = "PROJECTION_DEFAULT"
		case covered:
			ds.projection = "PROJECTION_COVERED"
		case len(filters) == 0 && sort == nil && outputs.finite && !outputs.sorts:
			ds.projection = "PROJECTION_SIMPLE"
		}
		switch {
		case best == nil, covered && best.fetch:
			best = ds
		case covered == !best.fetch && len(idx.Key) < len(best.index.Key):
			best = ds
		}
	}
	return best
}

// appendFieldConjuncts appends to conjuncts the one-field conditions filter
// joins, or reports false for a filter of any other operator.
func appendFieldConjuncts(conjuncts []*types.Document, filter *types.Document) ([]*types.Document, bool) {
	for _, k := range filter.Keys() {
		v := must.NotFail(filter.Get(k))
		if k != "$and" {
			if strings.HasPrefix(k, "$") {
				return nil, false
			}
			conjuncts = append(conjuncts, must.NotFail(types.NewDocument(k, v)))
			continue
		}
		arr, _ := v.(*types.Array)
		if arr == nil {
			return nil, false
		}
		for i := range arr.Len() {
			sub, _ := must.NotFail(arr.Get(i)).(*types.Document)
			var ok bool
			if sub == nil {
				return nil, false
			}
			if conjuncts, ok = appendFieldConjuncts(conjuncts, sub); !ok {
				return nil, false
			}
		}
	}
	return conjuncts, true
}

// distinctGroupKey returns the field path a $group's _id groups by, and its
// _id with that path as "$path", when it is one field path or an object of
// one.
func distinctGroupKey(group *types.Document) (string, any, bool) {
	id, _ := group.Get("_id")
	pathOf := func(v any) (string, bool) {
		s, ok := v.(string)
		if !ok {
			return "", false
		}
		for _, prefix := range []string{"$$ROOT.", "$$CURRENT."} {
			if rest, ok := strings.CutPrefix(s, prefix); ok {
				s = "$" + rest
			}
		}
		if !strings.HasPrefix(s, "$") || strings.HasPrefix(s, "$$") || len(s) == 1 {
			return "", false
		}
		return s[1:], true
	}
	switch id := id.(type) {
	case string:
		key, ok := pathOf(id)
		return key, "$" + key, ok
	case *types.Document:
		if id.Len() != 1 || strings.HasPrefix(id.Keys()[0], "$") {
			return "", nil, false
		}
		key, ok := pathOf(must.NotFail(id.Get(id.Keys()[0])))
		return key, must.NotFail(types.NewDocument(id.Keys()[0], "$"+key)), ok
	}
	return "", nil, false
}

// distinctGroupOutputs checks that every accumulator of group keeps the same
// one document of its group: all $first or all $last, or all $top or all
// $bottom sorted by key alone.
func distinctGroupOutputs(group *types.Document, key string, idExpr any, sort *types.Document, sortDir int) (distinctOutputs, bool) { //nolint:lll // for readability
	out := distinctOutputs{newRoot: must.NotFail(types.NewDocument("_id", idExpr)), paths: []string{key}, coverable: true, finite: true}
	out.scanDir = sortDir
	var op string
	var sortBy *types.Document
	var roots []string
	for _, field := range group.Keys() {
		if field == "_id" {
			continue
		}
		acc, _ := must.NotFail(group.Get(field)).(*types.Document)
		if acc == nil || acc.Len() != 1 || op != "" && acc.Command() != op {
			return out, false
		}
		op = acc.Command()
		arg := must.NotFail(acc.Get(op))
		switch op {
		case "$first", "$last":
		case "$top", "$bottom":
			spec, _ := arg.(*types.Document)
			if spec == nil {
				return out, false
			}
			by, _ := spec.Get("sortBy")
			byDoc, _ := by.(*types.Document)
			if byDoc == nil || byDoc.Len() != 1 || byDoc.Keys()[0] != key || sortDirection(must.NotFail(byDoc.Get(key))) == 0 ||
				sortBy != nil && !types.Identical(byDoc, sortBy) {
				return out, false
			}
			sortBy = byDoc
			if arg, _ = spec.Get("output"); arg == nil {
				return out, false
			}
		default:
			return out, false
		}
		out.newRoot.Set(field, arg)
		out.finite = out.finite && appendExprRootFields(arg, &roots)
		if path, ok := arg.(string); ok && strings.HasPrefix(path, "$") && !strings.HasPrefix(path, "$$") {
			out.paths = append(out.paths, path[1:])
		} else {
			out.coverable = false
		}
	}
	switch op {
	case "$last":
		if sort == nil {
			out.scanDir = 1
		} else {
			out.scanDir = -sortDir
		}
	case "$top", "$bottom":
		dir := sortDirection(must.NotFail(sortBy.Get(key)))
		if sort != nil && (sort.Len() != 1 || dir != sortDir) {
			return out, false
		}
		out.scanDir = dir
		if op == "$bottom" {
			out.scanDir = -dir
		}
		out.sorts = true
	}
	return out, true
}

// distinctOutputs is what the accumulators of a distinct scan $group read.
type distinctOutputs struct {
	// newRoot is the document $groupByDistinctScan makes of the document
	// each group keeps.
	newRoot *types.Document
	// paths are the field paths newRoot reads; coverable reports that it
	// reads nothing else, and finite that it does not read the whole
	// document.
	paths             []string
	coverable, finite bool
	// scanDir is the direction the scan reads the key in, 1 or -1.
	scanDir int
	// sorts reports $top or $bottom accumulators.
	sorts bool
}

// groupScansIndex reports whether mongod can scan idx for the distinct
// values of key: an ordinary index led by key, not multikey there, that
// holds every document matching filter, bounds only filterFields, and
// gives sort's order.
func groupScansIndex(ctx context.Context, idx backends.IndexInfo, key string, clustered bool, filterFields []string, filter, sort, queryCollation *types.Document) bool { //nolint:lll // for readability
	if len(idx.Key) == 0 || idx.Key[0].Field != key || idx.Hidden || idx.Sparse || idx.Lossy || idx.KeyMultikey(0) {
		return false
	}
	if clustered && idx.Name == backends.DefaultIndexName {
		return false
	}
	for _, k := range idx.Key {
		if k.Text || k.Hashed || k.Geo2D || k.Geo2DSphere || strings.Contains(k.Field, "$**") {
			return false
		}
	}
	for _, f := range filterFields {
		if !slices.ContainsFunc(idx.Key, func(k backends.IndexKeyPair) bool { return k.Field == f }) {
			return false
		}
	}
	if idx.PartialFilterExpression != nil {
		implied, known, err := backends.QueryImpliesPartialFilter(ctx, filter, idx.PartialFilterExpression, queryCollation, idx.Collation)
		if err != nil || !known || !implied {
			return false
		}
	}
	if sort == nil {
		return true
	}
	if sort.Len() > len(idx.Key) {
		return false
	}
	var reversed bool
	for i, f := range sort.Keys() {
		dir := sortDirection(must.NotFail(sort.Get(f)))
		if f != idx.Key[i].Field || dir == 0 {
			return false
		}
		r := (dir < 0) != idx.Key[i].Descending
		if i == 0 {
			reversed = r
		} else if r != reversed {
			return false
		}
	}
	return true
}

// sortDirection is 1 or -1 for a sort direction value, else 0.
func sortDirection(v any) int {
	var f float64
	switch n := v.(type) {
	case int32:
		f = float64(n)
	case int64:
		f = float64(n)
	case float64:
		f = n
	default:
		return 0
	}
	switch f {
	case 1:
		return 1
	case -1:
		return -1
	}
	return 0
}

// countAsDistinctScan has the reads of c, which pipeline reads, count as the
// distinct scan mongod runs p's $group with: a key, and when the scan fetches,
// a document, for each group. It returns c unchanged when p has none.
func (p sbePlan) countAsDistinctScan(c backends.Collection, pipeline []aggregations.Stage) backends.Collection {
	ds := p.distinct
	if ds == nil || ds.group >= len(pipeline) {
		return c
	}
	pipeline[ds.group] = &distinctScanCounter{inner: pipeline[ds.group], fetch: ds.fetch}
	return uncountedCollection{Collection: c}
}

// uncountedCollection is a collection whose queries count no reads.
type uncountedCollection struct {
	backends.Collection
}

func (c uncountedCollection) Query(ctx context.Context, params *backends.QueryParams) (*backends.QueryResult, error) {
	return c.Collection.Query(backends.WithUncountedReads(ctx), params)
}

// distinctScanCounter runs a distinct scan $group, counting what the scan
// reads for the groups it returns once they are done.
type distinctScanCounter struct {
	inner aggregations.Stage
	fetch bool
}

func (s *distinctScanCounter) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	out, err := s.inner.Process(ctx, iter, closer)
	if err != nil {
		return nil, err
	}
	res := &distinctScanCountingIterator{ctx: ctx, iter: out, fetch: s.fetch}
	closer.Add(res)
	return res, nil
}

type distinctScanCountingIterator struct {
	ctx     context.Context
	iter    types.DocumentsIterator
	groups  int
	fetch   bool
	counted bool
}

func (it *distinctScanCountingIterator) Next() (struct{}, *types.Document, error) {
	_, doc, err := it.iter.Next()
	if err == nil {
		it.groups++
	} else {
		it.count()
	}
	return struct{}{}, doc, err
}

func (it *distinctScanCountingIterator) Close() {
	it.count()
	it.iter.Close()
}

func (it *distinctScanCountingIterator) count() {
	if it.counted {
		return
	}
	it.counted = true
	docs := 0
	if it.fetch {
		docs = it.groups
	}
	backends.CountExamined(it.ctx, it.groups, docs)
}
