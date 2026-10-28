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

package dolt

import (
	"context"
	"slices"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func sortWalkQuery(t *testing.T, coll backends.Collection, params *backends.QueryParams) ([]int32, bool) {
	t.Helper()
	res, err := coll.Query(context.Background(), params)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer res.Iter.Close()
	var ids []int32
	for {
		_, doc, err := res.Iter.Next()
		if err != nil {
			break
		}
		ids = append(ids, must.NotFail(doc.Get("_id")).(int32))
	}
	return ids, res.Sorted
}

func sortOn(field string, dir int64) *types.Document {
	return must.NotFail(types.NewDocument(field, dir))
}

func newSortWalkCollection(t *testing.T, idx backends.IndexInfo, docs ...*types.Document) backends.Collection {
	t.Helper()
	c := newDottedPathTestCollection(t, idx)
	for _, d := range docs {
		if err := insertDottedPathDoc(c, d); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func kDoc(id int32, k any) *types.Document {
	return must.NotFail(types.NewDocument("_id", id, "k", k))
}

func TestSortWalkOrdersByIndex(t *testing.T) {
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "k", Key: []backends.IndexKeyPair{{Field: "k"}}},
		kDoc(1, int32(30)), kDoc(2, int32(10)), kDoc(3, int32(20)),
		kDoc(4, must.NotFail(types.NewDocument("x", int32(2)))),
		kDoc(5, must.NotFail(types.NewDocument("x", int32(1)))),
	)
	ids, sorted := sortWalkQuery(t, c, &backends.QueryParams{Sort: sortOn("k", 1)})
	if !sorted {
		t.Fatal("eligible single-field sort must be served by the index walk")
	}
	// The seed doc {_id: 0} lacks k and sorts as null, first.
	if want := []int32{0, 2, 3, 1, 5, 4}; !slices.Equal(ids, want) {
		t.Fatalf("ascending walk = %v, want %v", ids, want)
	}
	ids, _ = sortWalkQuery(t, c, &backends.QueryParams{Sort: sortOn("k", -1)})
	if want := []int32{4, 5, 1, 3, 2, 0}; !slices.Equal(ids, want) {
		t.Fatalf("descending walk = %v, want %v", ids, want)
	}
}

func TestSortWalkMultikeyYieldsEachDocOnceAtItsExtreme(t *testing.T) {
	arr := func(v ...any) *types.Array { return must.NotFail(types.NewArray(v...)) }
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "k", Key: []backends.IndexKeyPair{{Field: "k"}}},
		kDoc(1, arr(int32(5), int32(1))), kDoc(2, int32(3)), kDoc(3, arr(int32(4), int32(9))),
	)
	ids, sorted := sortWalkQuery(t, c, &backends.QueryParams{Sort: sortOn("k", 1)})
	if !sorted {
		t.Fatal("multikey index without array keys must be walked")
	}
	if want := []int32{0, 1, 2, 3}; !slices.Equal(ids, want) {
		t.Fatalf("ascending multikey walk = %v, want %v (by minimum element)", ids, want)
	}
	ids, _ = sortWalkQuery(t, c, &backends.QueryParams{Sort: sortOn("k", -1)})
	if want := []int32{3, 1, 2, 0}; !slices.Equal(ids, want) {
		t.Fatalf("descending multikey walk = %v, want %v (by maximum element)", ids, want)
	}
}

func TestSortWalkMergesArrayKeys(t *testing.T) {
	arr := func(v ...any) *types.Array { return must.NotFail(types.NewArray(v...)) }
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "k", Key: []backends.IndexKeyPair{{Field: "k"}}},
		kDoc(1, int32(5)), kDoc(2, int32(7)),
		kDoc(10, arr()),
		kDoc(11, arr(arr(int32(1), int32(2)), int32(500))),
		kDoc(12, arr(arr(int32(3)))),
		kDoc(13, arr(int32(-5), arr())),
	)
	ids, sorted := sortWalkQuery(t, c, &backends.QueryParams{Sort: sortOn("k", 1)})
	if !sorted {
		t.Fatal("multikey index holding array keys must be walked")
	}
	if want := []int32{10, 0, 13, 1, 2, 11, 12}; !slices.Equal(ids, want) {
		t.Fatalf("ascending walk = %v, want %v (empty array lowest, nested arrays by array order)", ids, want)
	}
	ids, _ = sortWalkQuery(t, c, &backends.QueryParams{Sort: sortOn("k", -1)})
	if want := []int32{12, 11, 13, 2, 1, 0, 10}; !slices.Equal(ids, want) {
		t.Fatalf("descending walk = %v, want %v", ids, want)
	}
}

func TestSortWalkReadsArrayKeysLazily(t *testing.T) {
	arr := func(v ...any) *types.Array { return must.NotFail(types.NewArray(v...)) }
	var docs []*types.Document
	for i := int32(1); i <= 300; i++ {
		switch i % 30 {
		case 0:
			docs = append(docs, kDoc(i, arr()))
		case 1:
			docs = append(docs, kDoc(i, arr(arr(i))))
		default:
			docs = append(docs, kDoc(i, i))
		}
	}
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "k", Key: []backends.IndexKeyPair{{Field: "k"}}}, docs...)

	first := func(sort *types.Document, n int) []int32 {
		t.Helper()
		res, err := c.Query(context.Background(), &backends.QueryParams{Sort: sort})
		if err != nil {
			t.Fatal(err)
		}
		defer res.Iter.Close()
		if !res.Sorted {
			t.Fatal("multikey index holding array keys must be walked")
		}
		var ids []int32
		for len(ids) < n {
			_, doc, err := res.Iter.Next()
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, must.NotFail(doc.Get("_id")).(int32))
		}
		return ids
	}

	keys, read := queryExecutorCounters.keys.Load(), queryExecutorCounters.docs.Load()
	ids := first(sortOn("k", 1), 3)
	gotKeys, gotDocs := queryExecutorCounters.keys.Load()-keys, queryExecutorCounters.docs.Load()-read
	for _, id := range ids {
		if id%30 != 0 {
			t.Fatalf("ascending = %v, want empty arrays (ids divisible by 30) first", ids)
		}
	}
	if gotKeys > 4 || gotDocs > 3 {
		t.Fatalf("first 3 of an ascending walk examined %d keys, %d docs; want at most 4, 3", gotKeys, gotDocs)
	}

	keys, read = queryExecutorCounters.keys.Load(), queryExecutorCounters.docs.Load()
	ids = first(sortOn("k", -1), 3)
	gotKeys, gotDocs = queryExecutorCounters.keys.Load()-keys, queryExecutorCounters.docs.Load()-read
	if want := []int32{271, 241, 211}; !slices.Equal(ids, want) {
		t.Fatalf("descending = %v, want nested arrays first by value %v", ids, want)
	}
	if gotKeys > 11 || gotDocs > 10 {
		t.Fatalf("first 3 of a descending walk examined %d keys, %d docs; want only the nested-array group (10) plus one", gotKeys, gotDocs)
	}
}

func TestSortWalkDeclines(t *testing.T) {
	k := []backends.IndexKeyPair{{Field: "k"}}
	for _, tc := range []struct {
		name   string
		idx    backends.IndexInfo
		docs   []*types.Document
		params *backends.QueryParams
	}{
		{"sparse index", backends.IndexInfo{Name: "k", Key: k, Sparse: true}, nil, &backends.QueryParams{Sort: sortOn("k", 1)}},
		{"hidden index", backends.IndexInfo{Name: "k", Key: k, Hidden: true}, nil, &backends.QueryParams{Sort: sortOn("k", 1)}},
		{"hint names another index", backends.IndexInfo{Name: "k", Key: k}, nil, &backends.QueryParams{Sort: sortOn("k", 1), Hint: "_id_"}},
		{"natural hint", backends.IndexInfo{Name: "k", Key: k}, nil, &backends.QueryParams{Sort: sortOn("k", 1), Hint: must.NotFail(types.NewDocument("$natural", int32(1)))}},
		{"sort on unindexed field", backends.IndexInfo{Name: "k", Key: k}, nil, &backends.QueryParams{Sort: sortOn("other", 1)}},
		{"collated query, binary index", backends.IndexInfo{Name: "k", Key: k}, nil, &backends.QueryParams{Sort: sortOn("k", 1), Collated: true, Collation: enCollation()}},
		{"binary query, collated index", backends.IndexInfo{Name: "k", Key: k, Collation: enCollation()}, nil, &backends.QueryParams{Sort: sortOn("k", 1)}},
		{"collated query without its collation", backends.IndexInfo{Name: "k", Key: k}, nil, &backends.QueryParams{Sort: sortOn("k", 1), Collated: true}},
		{"other collation", backends.IndexInfo{Name: "k", Key: k, Collation: enCollation()}, nil, &backends.QueryParams{Sort: sortOn("k", 1), Collated: true, Collation: must.NotFail(types.NewDocument("locale", "fr_CA"))}},
		{"collated multikey index", backends.IndexInfo{Name: "k", Key: k, Collation: enCollation()},
			[]*types.Document{kDoc(1, must.NotFail(types.NewArray("b", "A")))}, &backends.QueryParams{Sort: sortOn("k", 1), Collated: true, Collation: enCollation()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newSortWalkCollection(t, tc.idx, tc.docs...)
			if _, sorted := sortWalkQuery(t, c, tc.params); sorted {
				t.Fatal("query must not report a sorted index walk")
			}
		})
	}
}

func enCollation() *types.Document {
	return must.NotFail(types.NewDocument("locale", "en"))
}

func TestSortWalkCollatedIndex(t *testing.T) {
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "k", Key: []backends.IndexKeyPair{{Field: "k"}}, Collation: enCollation()},
		kDoc(1, "cherry"), kDoc(2, "Banana"), kDoc(3, "apple"), kDoc(4, int32(5)),
	)
	ids, sorted := sortWalkQuery(t, c, &backends.QueryParams{Sort: sortOn("k", 1), Collated: true, Collation: enCollation()})
	if !sorted {
		t.Fatal("a sort under the index's collation must be served by the index walk")
	}
	if want := []int32{0, 4, 3, 2, 1}; !slices.Equal(ids, want) {
		t.Fatalf("ascending walk = %v, want %v", ids, want)
	}
	ids, _ = sortWalkQuery(t, c, &backends.QueryParams{Sort: sortOn("k", -1), Collated: true, Collation: enCollation()})
	if want := []int32{1, 2, 3, 4, 0}; !slices.Equal(ids, want) {
		t.Fatalf("descending walk = %v, want %v", ids, want)
	}
}

// newSortVsFilterCollection holds docs 1..n with k = (i*7) mod n, p = i mod 3
// and q = i, indexed on each field.
func newSortVsFilterCollection(t *testing.T, n int32) backends.Collection {
	t.Helper()
	_, c := newSortVsFilterBackend(t, n)
	return c
}

// newSortVsFilterBackend holds idxdb.tests: n documents with k a permutation
// of 0..n-1 (index k), p = _id % 3 (index p), and q = _id (index q).
func newSortVsFilterBackend(t *testing.T, n int32) (*Backend, backends.Collection) {
	t.Helper()
	b, c := newDottedPathTestBackend(t, backends.IndexInfo{Name: "k", Key: []backends.IndexKeyPair{{Field: "k"}}})
	var docs []*types.Document
	for i := int32(1); i <= n; i++ {
		docs = append(docs, must.NotFail(types.NewDocument("_id", i, "k", (i*7)%n, "p", i%3, "q", i)))
	}
	if _, err := c.InsertAll(context.Background(), &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateIndexes(context.Background(), &backends.CreateIndexesParams{Indexes: []backends.IndexInfo{
		{Name: "p", Key: []backends.IndexKeyPair{{Field: "p"}}},
		{Name: "q", Key: []backends.IndexKeyPair{{Field: "q"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	return b, c
}

func TestSortWalkTrialAgainstFilterIndex(t *testing.T) {
	c := newSortVsFilterCollection(t, 3000)
	for _, tc := range []struct {
		name       string
		filter     *types.Document
		limit      int64
		hint       any
		wantWalked bool
	}{
		{"unselective filter with limit walks", must.NotFail(types.NewDocument("p", int32(1))), 5, nil, true},
		{"unselective filter without limit walks", must.NotFail(types.NewDocument("p", int32(1))), 0, nil, true},
		{"selective filter uses its index", must.NotFail(types.NewDocument("q", int32(7))), 5, nil, false},
		{"moderate filter without limit uses its index", must.NotFail(types.NewDocument("q", must.NotFail(types.NewDocument("$lt", int32(300))))), 0, nil, false},
		{"empty filter range uses its index", must.NotFail(types.NewDocument("q", int32(-1))), 5, nil, false},
		{"hint on the filter index skips the trial", must.NotFail(types.NewDocument("p", int32(1))), 5, "p", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, sorted := sortWalkQuery(t, c, &backends.QueryParams{Filter: tc.filter, Sort: sortOn("k", 1), LimitHint: tc.limit, Hint: tc.hint})
			if sorted != tc.wantWalked {
				t.Fatalf("walked = %v, want %v", sorted, tc.wantWalked)
			}
			if !sorted {
				return
			}
			// The walk returns every document in k order, the trial's
			// examined documents first; the caller filters.
			if len(ids) != 3001 {
				t.Fatalf("walk returned %d documents, want 3001", len(ids))
			}
			for i := 1; i < len(ids); i++ {
				if prev, cur := (ids[i-1]*7)%3000, (ids[i]*7)%3000; ids[i-1] != 0 && prev > cur {
					t.Fatalf("walk out of k order at %d: %v", i, ids[:i+1])
				}
			}
		})
	}
}

func TestSortWalkTrialOutcome(t *testing.T) {
	b, _ := newSortVsFilterBackend(t, 3000)
	c := rawCollection(b, "idxdb", "tests")
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		filter *types.Document
		limit  int64
		want   trialOutcome
	}{
		{"unselective filter: walk wins", must.NotFail(types.NewDocument("p", int32(1))), 5, trialWalkWon},
		{"moderate filter: filter wins", must.NotFail(types.NewDocument("q", must.NotFail(types.NewDocument("$lt", int32(300))))), 0, trialFilterWon},
		{"filter fits the limit: not run", must.NotFail(types.NewDocument("q", int32(7))), 5, trialNotRun},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := &backends.QueryParams{Filter: tc.filter, Sort: sortOn("k", 1), LimitHint: tc.limit}
			primary, _, state, err := c.getMap(ctx)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := c.planIndexLookup(ctx, state, primary, params.Filter, nil, nil, false)
			if err != nil || plan == nil {
				t.Fatalf("planIndexLookup = %v, %v", plan, err)
			}
			iter, got, err := c.trySortWalkTrial(ctx, state, primary, params, plan)
			if err != nil {
				t.Fatal(err)
			}
			if iter != nil {
				iter.Close()
			}
			if got != tc.want {
				t.Fatalf("outcome = %d, want %d", got, tc.want)
			}
			if (iter != nil) != (got == trialWalkWon) {
				t.Fatalf("iterator returned = %v for outcome %d", iter != nil, got)
			}
		})
	}
}

func TestSortWalkTrialNotRunWhenFilterFitsTheLimit(t *testing.T) {
	c := newSortVsFilterCollection(t, 3000)
	q := must.NotFail(types.NewDocument("q", must.NotFail(types.NewDocument("$gte", int32(100), "$lt", int32(105)))))
	for _, tc := range []struct {
		name       string
		limit      int64
		keys, docs int64
	}{
		{"limit equal to the matches", 5, 5, 5},
		{"limit above the matches", 10, 5, 5},
		{"no limit", 0, 5, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, docs := queryExecutorCounters.keys.Load(), queryExecutorCounters.docs.Load()
			ids, sorted := sortWalkQuery(t, c, &backends.QueryParams{Filter: q, Sort: sortOn("k", 1), LimitHint: tc.limit})
			gotKeys, gotDocs := queryExecutorCounters.keys.Load()-keys, queryExecutorCounters.docs.Load()-docs
			if sorted || len(ids) != 5 {
				t.Fatalf("walked = %v with %d documents, want the filter index's 5", sorted, len(ids))
			}
			if gotKeys != tc.keys || gotDocs != tc.docs {
				t.Fatalf("examined %d keys, %d docs; want %d, %d (no trial walk)", gotKeys, gotDocs, tc.keys, tc.docs)
			}
		})
	}
}

func TestIndexScanInSortOrder(t *testing.T) {
	doc := func(id, grp int32, k any) *types.Document {
		return must.NotFail(types.NewDocument("_id", id, "grp", grp, "k", k))
	}
	sub := func(x int32) *types.Document { return must.NotFail(types.NewDocument("x", x)) }
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "grp_k", Key: []backends.IndexKeyPair{{Field: "grp"}, {Field: "k", Descending: true}}},
		doc(1, 1, int32(30)), doc(2, 1, int32(10)), doc(3, 2, int32(5)), doc(4, 1, int32(20)),
		doc(5, 1, sub(2)), doc(6, 1, sub(1)), doc(7, 2, sub(0)),
	)
	insertFiller(t, c, 20)
	grp1 := must.NotFail(types.NewDocument("grp", int32(1)))
	ids, sorted := sortWalkQuery(t, c, &backends.QueryParams{Filter: grp1, Sort: sortOn("k", 1)})
	if !sorted {
		t.Fatal("an equality prefix scan yields the next key in order")
	}
	if want := []int32{2, 4, 1, 6, 5}; !slices.Equal(ids, want) {
		t.Fatalf("ascending = %v, want %v (subdocuments ordered by value)", ids, want)
	}
	ids, _ = sortWalkQuery(t, c, &backends.QueryParams{Filter: grp1, Sort: sortOn("k", -1)})
	if want := []int32{5, 6, 1, 4, 2}; !slices.Equal(ids, want) {
		t.Fatalf("descending = %v, want %v", ids, want)
	}

	in := must.NotFail(types.NewDocument("grp", must.NotFail(types.NewDocument("$in", must.NotFail(types.NewArray(int32(1), int32(2)))))))
	if _, sorted := sortWalkQuery(t, c, &backends.QueryParams{Filter: in, Sort: sortOn("k", 1)}); sorted {
		t.Fatal("$in on the prefix spans several prefixes; the scan is not in k order")
	}
}

func TestIndexScanInSortOrderOnSortField(t *testing.T) {
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "k", Key: []backends.IndexKeyPair{{Field: "k"}}},
		kDoc(1, int32(30)), kDoc(2, int32(10)), kDoc(3, int32(20)), kDoc(4, int32(40)),
	)
	insertFiller(t, c, 20)
	in := must.NotFail(types.NewDocument("k", must.NotFail(types.NewDocument("$in", must.NotFail(types.NewArray(int32(40), int32(10), int32(30)))))))
	ids, sorted := sortWalkQuery(t, c, &backends.QueryParams{Filter: in, Sort: sortOn("k", 1)})
	if !sorted {
		t.Fatal("$in points scanned in key order yield sort order")
	}
	if want := []int32{2, 1, 4}; !slices.Equal(ids, want) {
		t.Fatalf("$in ascending = %v, want %v", ids, want)
	}

	arr := func(v ...any) *types.Array { return must.NotFail(types.NewArray(v...)) }
	if err := insertDottedPathDoc(c, kDoc(5, arr(int32(15), int32(50)))); err != nil {
		t.Fatal(err)
	}
	gt := must.NotFail(types.NewDocument("k", must.NotFail(types.NewDocument("$gt", int32(12)))))
	if _, sorted := sortWalkQuery(t, c, &backends.QueryParams{Filter: gt, Sort: sortOn("k", 1)}); sorted {
		t.Fatal("a multikey range scan is not in sort order")
	}
}

// insertFiller adds n documents lacking the indexed fields, so test filters
// stay under the planner's half-the-collection cap.
func insertFiller(t *testing.T, c backends.Collection, n int32) {
	t.Helper()
	for i := int32(0); i < n; i++ {
		if err := insertDottedPathDoc(c, must.NotFail(types.NewDocument("_id", 1000+i))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIndexKeysSortedBeforeFetch(t *testing.T) {
	doc := func(id, grp int32, k any) *types.Document {
		return must.NotFail(types.NewDocument("_id", id, "grp", grp, "k", k))
	}
	sub := func(x int32) *types.Document { return must.NotFail(types.NewDocument("x", x)) }
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "grp_k", Key: []backends.IndexKeyPair{{Field: "grp"}, {Field: "k"}}},
		doc(1, 2, int32(30)), doc(2, 3, 2.5), doc(3, 2, int32(-4)), doc(4, 3, "s"), doc(5, 2, sub(2)),
		doc(6, 3, sub(1)), doc(7, 1, int32(0)), doc(8, 3, 2.25), doc(9, 2, int32(3)),
	)
	insertFiller(t, c, 20)
	gte2 := must.NotFail(types.NewDocument("grp", must.NotFail(types.NewDocument("$gte", int32(2)))))

	ids, sorted := sortWalkQuery(t, c, &backends.QueryParams{Filter: gte2, Sort: sortOn("k", 1)})
	if !sorted {
		t.Fatal("a range prefix scan is sorted on its keys")
	}
	if want := []int32{3, 8, 2, 9, 1, 4, 6, 5}; !slices.Equal(ids, want) {
		t.Fatalf("ascending = %v, want %v", ids, want)
	}
	ids, _ = sortWalkQuery(t, c, &backends.QueryParams{Filter: gte2, Sort: sortOn("k", -1)})
	if want := []int32{5, 6, 4, 1, 9, 2, 8, 3}; !slices.Equal(ids, want) {
		t.Fatalf("descending = %v, want %v", ids, want)
	}

	res, err := c.Query(context.Background(), &backends.QueryParams{Filter: gte2, Sort: sortOn("k", 1)})
	if err != nil {
		t.Fatal(err)
	}
	docs := queryExecutorCounters.docs.Load()
	for range 2 {
		if _, _, err := res.Iter.Next(); err != nil {
			t.Fatal(err)
		}
	}
	res.Iter.Close()
	if read := queryExecutorCounters.docs.Load() - docs; read != 2 {
		t.Fatalf("reading 2 sorted documents fetched %d", read)
	}

	residual := must.NotFail(types.NewDocument("grp", must.NotFail(types.NewDocument("$gte", int32(2))), "other", int32(1)))
	if _, sorted := sortWalkQuery(t, c, &backends.QueryParams{Filter: residual, Sort: sortOn("k", 1)}); sorted {
		t.Fatal("a filter field outside the index is decided after the fetch; the keys cannot be sorted first")
	}
}

func TestStreamSortedRangeCapsOtherPredicates(t *testing.T) {
	var docs []*types.Document
	for i := int32(1); i <= 100; i++ {
		grp := int32(0)
		if i%4 == 0 {
			grp = 1
		}
		docs = append(docs, must.NotFail(types.NewDocument("_id", i, "grp", grp, "k", i, "odd", i%2 == 1)))
	}
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "grp_k", Key: []backends.IndexKeyPair{{Field: "grp"}, {Field: "k"}}}, docs...)
	query := func(filter *types.Document) ([]int32, bool) {
		return sortWalkQuery(t, c, &backends.QueryParams{Filter: filter, Sort: sortOn("k", -1), LimitHint: 3})
	}
	ids, sorted := query(must.NotFail(types.NewDocument("grp", int32(0))))
	if !sorted || !slices.Equal(ids[:3], []int32{99, 98, 97}) {
		t.Fatalf("index-bound filter: ids %v sorted %v, want a sorted stream from 99", ids, sorted)
	}
	if _, sorted := query(must.NotFail(types.NewDocument("grp", int32(0), "odd", true))); sorted {
		t.Fatal("a predicate outside the index over most of the collection must not stream")
	}
	if ids, sorted := query(must.NotFail(types.NewDocument("grp", int32(1), "odd", false))); !sorted || !slices.Equal(ids[:3], []int32{100, 96, 92}) {
		t.Fatalf("narrow range with another predicate: ids %v sorted %v, want a sorted stream from 100", ids, sorted)
	}
}

func TestSortWalkSkipsFilteredSortWithoutLimit(t *testing.T) {
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "k", Key: []backends.IndexKeyPair{{Field: "k"}}},
		kDoc(1, int32(30)), kDoc(2, int32(10)), kDoc(3, int32(20)),
	)
	other := must.NotFail(types.NewDocument("z", int32(1)))
	if _, sorted := sortWalkQuery(t, c, &backends.QueryParams{Filter: other, Sort: sortOn("k", 1)}); sorted {
		t.Fatal("a filtered sort without a limit must scan and sort, not walk the index")
	}
	if _, sorted := sortWalkQuery(t, c, &backends.QueryParams{Filter: other, Sort: sortOn("k", 1), LimitHint: 2}); !sorted {
		t.Fatal("a filtered sort with a limit must walk the index")
	}
	if _, sorted := sortWalkQuery(t, c, &backends.QueryParams{Sort: sortOn("k", 1)}); !sorted {
		t.Fatal("an unfiltered sort must walk the index")
	}
}
