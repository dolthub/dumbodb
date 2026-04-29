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
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestDropIndexes_AfterDropAll verifies that after dropIndexes("*"), ListIndexes
// returns only the default _id_ index (regression for do-5ew7).
func TestDropIndexes_AfterDropAll(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("testcoll")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	// Insert a document so the collection exists.
	doc := must.NotFail(types.NewDocument("_id", int32(1), "x", int32(42)))
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	// Create two secondary indexes.
	_, err = coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{
			{Name: "x_1", Key: []backends.IndexKeyPair{{Field: "x"}}},
			{Name: "y_1", Key: []backends.IndexKeyPair{{Field: "y"}}},
		},
	})
	if err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	// Confirm both secondary indexes are listed.
	listRes, err := coll.ListIndexes(ctx, nil)
	if err != nil {
		t.Fatalf("ListIndexes before drop: %v", err)
	}

	if len(listRes.Indexes) != 3 { // _id_ + x_1 + y_1
		t.Fatalf("expected 3 indexes before drop, got %d: %+v", len(listRes.Indexes), listRes.Indexes)
	}

	// Drop all non-_id indexes (simulates dropIndexes("*")).
	toDrop := make([]string, 0, len(listRes.Indexes))
	for _, idx := range listRes.Indexes {
		if idx.Name != backends.DefaultIndexName {
			toDrop = append(toDrop, idx.Name)
		}
	}

	_, err = coll.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: toDrop})
	if err != nil {
		t.Fatalf("DropIndexes: %v", err)
	}

	// After drop, only _id_ should remain.
	listRes, err = coll.ListIndexes(ctx, nil)
	if err != nil {
		t.Fatalf("ListIndexes after drop: %v", err)
	}

	if len(listRes.Indexes) != 1 {
		t.Fatalf("expected 1 index after drop, got %d: %+v", len(listRes.Indexes), listRes.Indexes)
	}

	if listRes.Indexes[0].Name != backends.DefaultIndexName {
		t.Errorf("expected _id_ index, got %q", listRes.Indexes[0].Name)
	}
}

// TestDropIndexes_Single verifies that dropping a single named index removes only that index.
func TestDropIndexes_Single(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("testcoll")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	doc := must.NotFail(types.NewDocument("_id", int32(1), "x", int32(1)))
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	_, err = coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{
			{Name: "x_1", Key: []backends.IndexKeyPair{{Field: "x"}}},
			{Name: "z_1", Key: []backends.IndexKeyPair{{Field: "z"}}},
		},
	})
	if err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	_, err = coll.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: []string{"x_1"}})
	if err != nil {
		t.Fatalf("DropIndexes: %v", err)
	}

	listRes, err := coll.ListIndexes(ctx, nil)
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}

	// Should have _id_ and z_1.
	if len(listRes.Indexes) != 2 {
		t.Fatalf("expected 2 indexes, got %d: %+v", len(listRes.Indexes), listRes.Indexes)
	}

	for _, idx := range listRes.Indexes {
		if idx.Name == "x_1" {
			t.Errorf("x_1 should have been dropped but is still present")
		}
	}
}

// drainQuery runs a Query and returns every document the backend's iterator
// yields. It is the test equivalent of the handler's "consume the cursor"
// step — the index-aware planner is allowed to return a superset of matches,
// so callers must do their own re-checking when verifying exact-match
// semantics.
func drainQuery(t *testing.T, ctx context.Context, coll backends.Collection, params *backends.QueryParams) []*types.Document {
	t.Helper()

	res, err := coll.Query(ctx, params)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer res.Iter.Close()

	var docs []*types.Document
	for {
		_, doc, err := res.Iter.Next()
		if err != nil {
			if err == iterator.ErrIteratorDone {
				break
			}
			t.Fatalf("Iter.Next: %v", err)
		}
		docs = append(docs, doc)
	}
	return docs
}

// idsInDocs extracts the int32 _id of each document into a sorted set. The
// dolt backend stores _id values via canonical Extended JSON; integers
// round-trip through the wire-driver path as int32.
func idsInDocs(t *testing.T, docs []*types.Document) []int32 {
	t.Helper()
	out := make([]int32, 0, len(docs))
	for _, d := range docs {
		v, err := d.Get("_id")
		if err != nil {
			t.Fatalf("doc missing _id: %v", err)
		}
		switch x := v.(type) {
		case int32:
			out = append(out, x)
		case int64:
			out = append(out, int32(x))
		default:
			t.Fatalf("unexpected _id type %T", v)
		}
	}
	return out
}

// containsAll returns true if needles ⊆ haystack. Used for "the index path
// returned at least every matching doc" checks — false positives are allowed
// because the handler re-filters above the backend.
func containsAll(haystack, needles []int32) bool {
	set := make(map[int32]struct{}, len(haystack))
	for _, h := range haystack {
		set[h] = struct{}{}
	}
	for _, n := range needles {
		if _, ok := set[n]; !ok {
			return false
		}
	}
	return true
}

// TestIndexLookup_RangeAndCompound exercises the new range / compound /
// equality paths in tryIndexLookup. Each query is checked for soundness
// (every actual match appears in the result set) and for selectivity (the
// range path returns far fewer than the full collection on a tight bound).
func TestIndexLookup_RangeAndCompound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("docs")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	// Seed 100 documents with a numeric "i" field 0..99 and a "grp" field
	// i%5 to exercise compound queries.
	const n = 100
	docs := make([]*types.Document, 0, n)
	for i := int32(0); i < n; i++ {
		d := must.NotFail(types.NewDocument(
			"_id", i,
			"i", i,
			"grp", i%5,
			"tag", "row",
		))
		docs = append(docs, d)
	}
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	if _, err = coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{
			{Name: "i_1", Key: []backends.IndexKeyPair{{Field: "i"}}},
			{Name: "grp_1", Key: []backends.IndexKeyPair{{Field: "grp"}}},
		},
	}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	// --- Equality on indexed field ----------------------------------------
	// Verifies the equality fast-path still works after the rewrite to
	// IterKeyRange; expected matches are i==42 only.
	t.Run("equality", func(t *testing.T) {
		params := &backends.QueryParams{
			Filter: must.NotFail(types.NewDocument("i", int32(42))),
		}
		got := drainQuery(t, ctx, coll, params)
		if !containsAll(idsInDocs(t, got), []int32{42}) {
			t.Fatalf("equality: expected to contain {42}, got %v", idsInDocs(t, got))
		}
		// The handler re-filters above the backend, but the backend itself
		// should already be tight: only one entry in the index has i=42.
		if len(got) != 1 {
			t.Errorf("equality: expected 1 candidate from backend, got %d", len(got))
		}
	})

	// --- Range with $gte / $lt --------------------------------------------
	// 50 ≤ i < 60 → ten matches. The bounded scan should not return docs
	// outside this window.
	t.Run("range_gte_lt", func(t *testing.T) {
		opDoc := must.NotFail(types.NewDocument("$gte", int32(50), "$lt", int32(60)))
		params := &backends.QueryParams{
			Filter: must.NotFail(types.NewDocument("i", opDoc)),
		}
		got := drainQuery(t, ctx, coll, params)
		ids := idsInDocs(t, got)
		expected := []int32{50, 51, 52, 53, 54, 55, 56, 57, 58, 59}
		if !containsAll(ids, expected) {
			t.Fatalf("range_gte_lt: missing matches; got %v", ids)
		}
		// Bounded-scan tightness: no doc outside [50, 60) should leak
		// through, because the index bounds are encoded for ints.
		for _, id := range ids {
			if id < 50 || id >= 60 {
				t.Errorf("range_gte_lt: candidate id %d out of [50,60)", id)
			}
		}
	})

	// --- Range with $gt / $lte (exclusive low, inclusive high) ------------
	t.Run("range_gt_lte", func(t *testing.T) {
		opDoc := must.NotFail(types.NewDocument("$gt", int32(10), "$lte", int32(15)))
		params := &backends.QueryParams{
			Filter: must.NotFail(types.NewDocument("i", opDoc)),
		}
		got := drainQuery(t, ctx, coll, params)
		ids := idsInDocs(t, got)
		expected := []int32{11, 12, 13, 14, 15}
		if !containsAll(ids, expected) {
			t.Fatalf("range_gt_lte: missing matches; got %v", ids)
		}
		for _, id := range ids {
			if id <= 10 || id > 15 {
				t.Errorf("range_gt_lte: candidate id %d out of (10,15]", id)
			}
		}
	})

	// --- Open-ended range --------------------------------------------------
	// $gte:95 with no upper bound. The lookup is sound (returns every match)
	// but is allowed to be loose: nothing outside the type bracket appears
	// in this collection, so the handler still gets a small candidate set.
	t.Run("range_gte_open", func(t *testing.T) {
		opDoc := must.NotFail(types.NewDocument("$gte", int32(95)))
		params := &backends.QueryParams{
			Filter: must.NotFail(types.NewDocument("i", opDoc)),
		}
		got := drainQuery(t, ctx, coll, params)
		expected := []int32{95, 96, 97, 98, 99}
		if !containsAll(idsInDocs(t, got), expected) {
			t.Fatalf("range_gte_open: missing matches; got %v", idsInDocs(t, got))
		}
	})

	// --- Compound filter (indexed grp + non-indexed tag) ------------------
	// The index on grp narrows to 20 candidates (i%5 == 2 over 100 docs);
	// the handler then applies the tag predicate. The backend should
	// return at most the 20 grp=2 docs — i.e. the index path was used.
	t.Run("compound_indexed_plus_unindexed", func(t *testing.T) {
		params := &backends.QueryParams{
			Filter: must.NotFail(types.NewDocument(
				"grp", int32(2),
				"tag", "row",
			)),
		}
		got := drainQuery(t, ctx, coll, params)
		if len(got) > 20 {
			t.Errorf("compound: expected backend to narrow via grp index (≤20 docs); got %d", len(got))
		}
		// Soundness: every doc with grp==2 must be in the candidate set.
		var expected []int32
		for i := int32(0); i < n; i++ {
			if i%5 == 2 {
				expected = append(expected, i)
			}
		}
		if !containsAll(idsInDocs(t, got), expected) {
			t.Fatalf("compound: missing matches; got %v", idsInDocs(t, got))
		}
	})

	// --- Filter on un-indexed field falls back to scan --------------------
	// "tag" has no index. The backend should still return matching docs;
	// we only assert correctness here, not the path taken.
	t.Run("unindexed_field_fallback", func(t *testing.T) {
		params := &backends.QueryParams{
			Filter: must.NotFail(types.NewDocument("tag", "row")),
		}
		got := drainQuery(t, ctx, coll, params)
		if len(got) < n {
			// All seeded docs have tag="row"; the scan should yield all of
			// them. (False negatives here would mean the index path
			// silently dropped docs.)
			t.Errorf("unindexed_field_fallback: expected ≥%d docs, got %d", n, len(got))
		}
	})
}
