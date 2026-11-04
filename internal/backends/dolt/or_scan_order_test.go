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
	"strings"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// newCreationOrderCollection holds 300 documents with k = q = _id, indexed by
// k and then by a_q on q and a_q_k on {q, k}, so creation and name order differ.
func newCreationOrderCollection(t *testing.T) *collection {
	t.Helper()
	b, c := newDottedPathTestBackend(t, backends.IndexInfo{Name: "k", Key: []backends.IndexKeyPair{{Field: "k"}}})
	var docs []*types.Document
	for i := int32(1); i <= 300; i++ {
		docs = append(docs, must.NotFail(types.NewDocument("_id", i, "k", i, "q", i)))
	}
	if _, err := c.InsertAll(context.Background(), &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatal(err)
	}
	for _, idx := range []backends.IndexInfo{
		{Name: "a_q", Key: []backends.IndexKeyPair{{Field: "q"}}},
		{Name: "a_q_k", Key: []backends.IndexKeyPair{{Field: "q"}, {Field: "k"}}},
	} {
		if _, err := c.CreateIndexes(context.Background(), &backends.CreateIndexesParams{Indexes: []backends.IndexInfo{idx}}); err != nil {
			t.Fatal(err)
		}
	}
	return &collection{db: &database{backend: b, name: "idxdb", rootish: defaultBranch}, name: "tests"}
}

func TestIndexesResolveInCreationOrder(t *testing.T) {
	c := newCreationOrderCollection(t)
	state := must.NotFail(c.db.backend.getOrOpenDB(context.Background(), c.db.name, false))
	state.mu.RLock()
	infos, _, err := resolveIndexes(context.Background(), c, state)
	state.mu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, info := range infos {
		names = append(names, info.Name)
	}
	if want := []string{"k", "a_q", "a_q_k"}; !slices.Equal(names, want) {
		t.Errorf("indexes %v, want %v", names, want)
	}
}

// TestOrBranchScans checks an $or's index scans come in the order MongoDB's OR
// stage reads them, the single-predicate branches on one index merged into one
// scan of their intervals, with the keys that scan examines between them.
func TestOrBranchScans(t *testing.T) {
	c := newCreationOrderCollection(t)
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	for _, tc := range []struct {
		name   string
		filter *types.Document
		want   []string
		keys   int64
	}{
		{"by index creation order", orFilter(doc("q", int32(2)), doc("k", int32(1))), []string{"IXSCAN(k)", "IXSCAN(a_q)"}, 2},
		{"one scan per index", orFilter(doc("k", int32(1)), doc("q", int32(2)), doc("k", int32(5))), []string{"IXSCAN(k)", "IXSCAN(a_q)"}, 5},
		{"several predicates scan first", orFilter(doc("k", int32(2)), doc("q", int32(1), "k", doc("$gte", int32(0)))), []string{"IXSCAN(a_q_k)", "IXSCAN(k)"}, 2},
		{"a branch its index does not bound fetches", orFilter(doc("q", int32(2)), doc("k", int32(1), "z", int32(1))), []string{"FETCH", "IXSCAN(k)", "IXSCAN(a_q)"}, 2},
		{"overlapping intervals join", orFilter(doc("k", doc("$gt", int32(295))), doc("k", int32(298)), doc("q", int32(1))), []string{"IXSCAN(k)", "IXSCAN(a_q)"}, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, examined := backends.WithExaminedCounts(context.Background())
			res, plan, err := c.query(ctx, &backends.QueryParams{Filter: tc.filter})
			if err != nil {
				t.Fatal(err)
			}
			for {
				if _, _, err := res.Iter.Next(); err != nil {
					break
				}
			}
			res.Iter.Close()
			got := planChain(plan)
			if !slices.Equal(got[2:], tc.want) {
				t.Errorf("plan %s, want OR of %s", strings.Join(got, " > "), strings.Join(tc.want, ", "))
			}
			if examined.Keys() != tc.keys {
				t.Errorf("examined %d keys, want %d", examined.Keys(), tc.keys)
			}
		})
	}
}

// TestMultiIntervalScanKeys checks a scan of several intervals counts the
// keys it lands on outside them, as MongoDB's IXSCAN does, unless each
// interval is read from its own seek.
func TestMultiIntervalScanKeys(t *testing.T) {
	c := newCreationOrderCollection(t)
	in := func(vs ...any) *types.Document {
		return must.NotFail(types.NewDocument("k", must.NotFail(types.NewDocument("$in", must.NotFail(types.NewArray(vs...))))))
	}
	for _, tc := range []struct {
		name     string
		filter   *types.Document
		seekEach bool
		keys     int64
	}{
		{"a key past each interval", in(int32(7), int32(100)), false, 4},
		{"adjacent intervals", in(int32(7), int32(8), int32(100)), false, 5},
		{"nothing past the last key", in(int32(7), int32(300)), false, 3},
		{"a missing value", in(int32(7), int32(1000), int32(100)), false, 4},
		{"seeking each interval", in(int32(7), int32(100)), true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, examined := backends.WithExaminedCounts(context.Background())
			if tc.seekEach {
				ctx = backends.WithSeekEachRange(ctx)
			}
			res, _, err := c.query(ctx, &backends.QueryParams{Filter: tc.filter})
			if err != nil {
				t.Fatal(err)
			}
			for {
				if _, _, err := res.Iter.Next(); err != nil {
					break
				}
			}
			res.Iter.Close()
			if examined.Keys() != tc.keys {
				t.Errorf("examined %d keys, want %d", examined.Keys(), tc.keys)
			}
		})
	}
}

// TestCountingScan checks the scan under a counting GROUP reads no document
// when its index bounds the whole filter.
func TestCountingScan(t *testing.T) {
	c := newCreationOrderCollection(t)
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	for _, tc := range []struct {
		name   string
		filter *types.Document
		want   []string
	}{
		{"one interval", doc("k", doc("$gte", int32(250))), []string{"GROUP", "COUNT_SCAN(k)"}},
		{"compound", doc("q", int32(5), "k", doc("$gte", int32(0))), []string{"GROUP", "COUNT_SCAN(a_q_k)"}},
		{"several intervals", doc("k", doc("$in", must.NotFail(types.NewArray(int32(1), int32(2))))), []string{"GROUP", "IXSCAN(k)"}},
		{"a field outside the index", doc("k", int32(5), "z", int32(1)), []string{"GROUP", "FETCH", "IXSCAN(k)"}},
		{"no index", doc("z", int32(1)), []string{"GROUP", "COLLSCAN"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := c.Explain(context.Background(), &backends.ExplainParams{Command: "aggregate", Filter: tc.filter, CountOnly: true, Fields: []string{}})
			if err != nil {
				t.Fatal(err)
			}
			wp := must.NotFail(res.QueryPlanner.Get("winningPlan")).(*types.Document)
			var got []string
			for wp != nil {
				entry := must.NotFail(wp.Get("stage")).(string)
				if name, _ := wp.Get("indexName"); name != nil {
					entry += "(" + name.(string) + ")"
				}
				got = append(got, entry)
				next, _ := wp.Get("inputStage")
				wp, _ = next.(*types.Document)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("plan %s, want %s", strings.Join(got, " > "), strings.Join(tc.want, " > "))
			}
		})
	}
}
