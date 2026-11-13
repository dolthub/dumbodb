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
	"strconv"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestQueryShape(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(v ...any) *types.Array { return must.NotFail(types.NewArray(v...)) }
	shape := func(p backends.QueryParams) string { return queryShape(&p) }
	base := backends.QueryParams{Filter: doc("grp", int32(7)), Sort: doc("k", int64(1)), LimitHint: 10}

	same := []struct {
		name string
		p    backends.QueryParams
	}{
		{"another value", backends.QueryParams{Filter: doc("grp", int32(8)), Sort: doc("k", int64(1)), LimitHint: 10}},
		{"another number type", backends.QueryParams{Filter: doc("grp", 7.5), Sort: doc("k", int64(1)), LimitHint: 10}},
	}
	for _, tc := range same {
		if shape(tc.p) != shape(base) {
			t.Errorf("%s: shape %q, want %q", tc.name, shape(tc.p), shape(base))
		}
	}

	differ := []struct {
		name string
		p    backends.QueryParams
	}{
		{"another field", backends.QueryParams{Filter: doc("x", int32(7)), Sort: doc("k", int64(1)), LimitHint: 10}},
		{"another value class", backends.QueryParams{Filter: doc("grp", "7"), Sort: doc("k", int64(1)), LimitHint: 10}},
		{"an operator", backends.QueryParams{Filter: doc("grp", doc("$lt", int32(7))), Sort: doc("k", int64(1)), LimitHint: 10}},
		{"another sort direction", backends.QueryParams{Filter: doc("grp", int32(7)), Sort: doc("k", int64(-1)), LimitHint: 10}},
		{"another sort field", backends.QueryParams{Filter: doc("grp", int32(7)), Sort: doc("j", int64(1)), LimitHint: 10}},
		{"another limit", backends.QueryParams{Filter: doc("grp", int32(7)), Sort: doc("k", int64(1)), LimitHint: 20}},
		{"no limit", backends.QueryParams{Filter: doc("grp", int32(7)), Sort: doc("k", int64(1))}},
		{"a hint", backends.QueryParams{Filter: doc("grp", int32(7)), Sort: doc("k", int64(1)), LimitHint: 10, Hint: "grp_1"}},
		{"a collation", backends.QueryParams{Filter: doc("grp", int32(7)), Sort: doc("k", int64(1)), LimitHint: 10, Collated: true, Collation: doc("locale", "fr", "strength", int32(2))}},
		{"fields in another order", backends.QueryParams{Filter: doc("grp", int32(7), "k", int32(1)), Sort: doc("k", int64(1)), LimitHint: 10}},
	}
	seen := map[string]string{shape(base): "base"}
	for _, tc := range differ {
		s := shape(tc.p)
		if prev, dup := seen[s]; dup {
			t.Errorf("%s: shape %q also belongs to %s", tc.name, s, prev)
		}
		seen[s] = tc.name
	}
	if a, b := shape(backends.QueryParams{Filter: doc("grp", int32(7), "k", int32(1))}), shape(backends.QueryParams{Filter: doc("k", int32(1), "grp", int32(7))}); a == b {
		t.Errorf("field order must be kept: both %q", a)
	}

	if a, b := shape(backends.QueryParams{LimitHint: 200}), shape(backends.QueryParams{}); a != b {
		t.Errorf("limits past the trial target share a shape: %q vs %q", a, b)
	}

	in := func(v ...any) backends.QueryParams {
		return backends.QueryParams{Filter: doc("grp", doc("$in", arr(v...)))}
	}
	if a, b := shape(in(int32(1), int32(2))), shape(in(int32(3), int32(4), 5.0)); a != b {
		t.Errorf("$in lists of one class share a shape: %q vs %q", a, b)
	}
	if a, b := shape(in(int32(1))), shape(in(int32(1), "x")); a == b {
		t.Errorf("$in lists of different classes differ: both %q", a)
	}

	or := func(v any) backends.QueryParams {
		return backends.QueryParams{Filter: doc("$or", arr(doc("a", v), doc("b", doc("$gt", int32(1)))))}
	}
	if a, b := shape(or(int32(1))), shape(or(int32(2))); a != b {
		t.Errorf("$or branches compare by shape: %q vs %q", a, b)
	}
	if a, b := shape(or(int32(1))), shape(or("x")); a == b {
		t.Errorf("$or branch value classes differ: both %q", a)
	}
}

func TestPlanCacheLRU(t *testing.T) {
	var pc planCache
	pc.put("a", planCacheEntry{walkIndex: "k"})
	pc.put("b", planCacheEntry{})
	if e, ok := pc.get("a"); !ok || e.walkIndex != "k" {
		t.Fatalf("get a = %v, %v", e, ok)
	}
	pc.evict("a")
	if _, ok := pc.get("a"); ok {
		t.Fatal("evicted entry still cached")
	}
	pc.put("p\x00x", planCacheEntry{})
	pc.put("p\x00y", planCacheEntry{})
	pc.clearPrefix("p\x00")
	if pc.len() != 1 {
		t.Fatalf("after clearPrefix %d entries, want only b", pc.len())
	}
	for i := 0; i < planCacheMaxEntries+5; i++ {
		pc.put(string(rune('A'+i%26))+strconv.Itoa(i), planCacheEntry{})
	}
	if pc.len() != planCacheMaxEntries {
		t.Fatalf("%d entries, want the bound %d", pc.len(), planCacheMaxEntries)
	}
	if _, ok := pc.get("b"); ok {
		t.Fatal("least recently used entry survived past the bound")
	}
}

func TestPlanCacheSkipsTheTrial(t *testing.T) {
	b, _ := newSortVsFilterBackend(t, 3000)
	c := rawCollection(b, "idxdb", "tests")
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	examined := func(params *backends.QueryParams) (keys, docs int64, walked bool) {
		t.Helper()
		k0, d0 := queryExecutorCounters.keys.Load(), queryExecutorCounters.docs.Load()
		res, err := c.Query(context.Background(), params)
		if err != nil {
			t.Fatal(err)
		}
		for i := int64(0); i < 5; i++ {
			if _, _, err := res.Iter.Next(); err != nil {
				break
			}
		}
		res.Iter.Close()
		return queryExecutorCounters.keys.Load() - k0, queryExecutorCounters.docs.Load() - d0, res.Sorted
	}

	unselective := func(p int32) *backends.QueryParams {
		return &backends.QueryParams{Filter: doc("p", p), Sort: sortOn("k", 1), LimitHint: 5}
	}
	firstKeys, _, walked := examined(unselective(1))
	if !walked || b.planCache.len() != 1 {
		t.Fatalf("first query walked = %v with %d cache entries; want a walk recorded", walked, b.planCache.len())
	}
	keys, _, walked := examined(unselective(2))
	if !walked {
		t.Fatal("cached walk not taken")
	}
	if keys >= firstKeys || keys > 20 {
		t.Fatalf("cached walk examined %d keys (trial run %d); want only the walk's few", keys, firstKeys)
	}

	moderate := func(q int32) *backends.QueryParams {
		return &backends.QueryParams{Filter: doc("q", doc("$lt", q)), Sort: sortOn("k", 1)}
	}
	if _, _, walked := examined(moderate(300)); walked || b.planCache.len() != 2 {
		t.Fatalf("filter-won trial walked = %v with %d cache entries; want the filter recorded", walked, b.planCache.len())
	}
	keys, docs, walked := examined(moderate(250))
	if walked || keys != 249 || docs != 249 {
		t.Fatalf("cached filter plan examined %d keys, %d docs (walked %v); want only the filter's 249", keys, docs, walked)
	}

	ctx := context.Background()
	_, _, state, err := c.getMap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	infos, _, err := resolveIndexes(ctx, c, state)
	if err != nil {
		t.Fatal(err)
	}
	before := c.planCacheKey(infos, moderate(1))
	if _, err := c.CreateIndexes(ctx, &backends.CreateIndexesParams{Indexes: []backends.IndexInfo{{Name: "pq", Key: []backends.IndexKeyPair{{Field: "p"}, {Field: "q"}}}}}); err != nil {
		t.Fatal(err)
	}
	infos, _, err = resolveIndexes(ctx, c, state)
	if err != nil {
		t.Fatal(err)
	}
	if c.planCacheKey(infos, moderate(1)) == before {
		t.Fatal("creating an index left the cache key unchanged")
	}

	if err := c.db.DropCollection(ctx, &backends.DropCollectionParams{Name: "tests"}); err != nil {
		t.Fatal(err)
	}
	if n := b.planCache.len(); n != 0 {
		t.Fatalf("%d cache entries survive dropping the collection", n)
	}
}

func TestPlanCacheReplansOverBudget(t *testing.T) {
	b, _ := newSortVsFilterBackend(t, 3000)
	c := rawCollection(b, "idxdb", "tests")
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	run := func(params *backends.QueryParams) (keys int64, walked bool, entry planCacheEntry, cached bool) {
		t.Helper()
		ctx := context.Background()
		k0 := queryExecutorCounters.keys.Load()
		res, err := c.Query(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		res.Iter.Close()
		_, _, state, err := c.getMap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		infos, _, err := resolveIndexes(ctx, c, state)
		if err != nil {
			t.Fatal(err)
		}
		entry, cached = b.planCache.get(c.planCacheKey(infos, params))
		return queryExecutorCounters.keys.Load() - k0, res.Sorted, entry, cached
	}

	t.Run("cached walk over budget replans", func(t *testing.T) {
		byP := func(p int32) *backends.QueryParams {
			return &backends.QueryParams{Filter: doc("p", p), Sort: sortOn("k", 1), LimitHint: 5}
		}
		if _, walked, entry, cached := run(byP(1)); !walked || !cached || entry.walkIndex != "k" {
			t.Fatalf("p=1 walked %v, cached %v %+v; want the walk on k cached", walked, cached, entry)
		}
		// No document has p=7: the walk would read the whole index for no
		// match. Over its budget it gives up and the query replans onto p.
		keys, walked, _, cached := run(byP(7))
		if walked || cached {
			t.Fatalf("p=7 walked %v, still cached %v; want a replan onto the filter index", walked, cached)
		}
		if keys > 3000/2 {
			t.Fatalf("p=7 examined %d keys; the walk should stop at its budget", keys)
		}
	})

	t.Run("cached filter over budget replans", func(t *testing.T) {
		byQ := func(q int32) *backends.QueryParams {
			return &backends.QueryParams{Filter: doc("q", doc("$lt", q)), Sort: sortOn("k", 1), LimitHint: 5}
		}
		if _, walked, entry, cached := run(byQ(10)); walked || !cached || entry.walkIndex != "" {
			t.Fatalf("q<10 walked %v, cached %v %+v; want the filter cached", walked, cached, entry)
		}
		// q<1000 holds a third of the documents: the filter plan reads 999
		// keys, past its budget, so the trial runs again and the walk wins.
		_, walked, entry, cached := run(byQ(1000))
		if !walked || !cached || entry.walkIndex != "k" {
			t.Fatalf("q<1000 walked %v, cached %v %+v; want a replan onto the walk", walked, cached, entry)
		}
	})
}

func TestPlanCacheStatsAndClear(t *testing.T) {
	b, _ := newSortVsFilterBackend(t, 3000)
	c := rawCollection(b, "idxdb", "tests")
	ctx := context.Background()
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	query := func(filter *types.Document) {
		t.Helper()
		res, err := c.Query(ctx, &backends.QueryParams{Filter: filter, Sort: sortOn("k", 1), LimitHint: 5})
		if err != nil {
			t.Fatal(err)
		}
		res.Iter.Close()
	}
	stats := func() []backends.PlanCacheEntry {
		t.Helper()
		entries, err := c.PlanCacheStats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}

	query(doc("p", int32(1)))
	query(doc("q", doc("$lt", int32(10))))
	entries := stats()
	if len(entries) != 2 {
		t.Fatalf("%d entries, want 2", len(entries))
	}
	walk := entries[1]
	if got := must.NotFail(walk.CachedPlan.Get("stage")); got != "LIMIT" || walk.ShapeHash == "" || walk.Key == "" || walk.Works <= 0 {
		t.Fatalf("walk entry = %+v", walk)
	}

	if err := c.PlanCacheClear(ctx, &backends.PlanCacheClearParams{Filter: doc("p", int32(9)), Sort: sortOn("k", 1)}); err != nil {
		t.Fatal(err)
	}
	if entries = stats(); len(entries) != 1 || must.NotFail(entries[0].CachedPlan.Get("stage")) != "SORT" {
		t.Fatalf("after clearing the p shape: %+v, want only the filter entry", entries)
	}
	if err := c.PlanCacheClear(ctx, &backends.PlanCacheClearParams{}); err != nil {
		t.Fatal(err)
	}
	if entries = stats(); len(entries) != 0 {
		t.Fatalf("after clearing the collection: %d entries", len(entries))
	}

	missing := rawCollection(b, "idxdb", "missing")
	if _, err := missing.PlanCacheStats(ctx); !backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) {
		t.Fatalf("stats on a missing collection: %v", err)
	}
}

func TestIndexFingerprintTracksMultikeyFields(t *testing.T) {
	idx := backends.IndexInfo{Name: "a_b", Key: []backends.IndexKeyPair{{Field: "a"}, {Field: "b"}}, Multikey: true, MultikeyFieldsKnown: true}
	first, second := idx, idx
	first.MultikeyFields, second.MultikeyFields = 1, 2
	if indexFingerprint([]backends.IndexInfo{first}) == indexFingerprint([]backends.IndexInfo{second}) {
		t.Fatal("indexes multikey on different fields share a fingerprint")
	}
}
