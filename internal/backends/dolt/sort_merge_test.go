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

func TestSortMergeOfInRanges(t *testing.T) {
	b, _ := newDottedPathTestBackend(t, backends.IndexInfo{Name: "grp_k", Key: []backends.IndexKeyPair{{Field: "grp"}, {Field: "k"}}})
	c := &collection{db: &database{backend: b, name: "idxdb", rootish: defaultBranch}, name: "tests"}
	var docs []*types.Document
	for i := int32(1); i <= 1000; i++ {
		docs = append(docs, must.NotFail(types.NewDocument("_id", i, "grp", i%5, "k", i)))
	}
	if _, err := c.InsertAll(context.Background(), &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatal(err)
	}
	in := func(vals ...any) *types.Document {
		return must.NotFail(types.NewDocument("grp", must.NotFail(types.NewDocument("$in", must.NotFail(types.NewArray(vals...))))))
	}

	for _, tc := range []struct {
		name   string
		params *backends.QueryParams
		want   []int32
	}{
		{"ascending", &backends.QueryParams{Filter: in(int32(1), int32(3)), Sort: sortOn("k", 1), LimitHint: 4}, []int32{1, 3, 6, 8}},
		{"descending", &backends.QueryParams{Filter: in(int32(1), int32(3)), Sort: sortOn("k", -1), LimitHint: 4}, []int32{998, 996, 993, 991}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, examined := backends.WithExaminedCounts(context.Background())
			res, plan, err := c.query(ctx, tc.params)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Iter.Close()
			if want := "FETCH > SORT_MERGE > IXSCAN(grp_k) > IXSCAN(grp_k)"; strings.Join(planChain(plan), " > ") != want || !res.Sorted {
				t.Fatalf("plan %v sorted %v, want %s, sorted", planChain(plan), res.Sorted, want)
			}
			var ids []int32
			for len(ids) < len(tc.want) {
				_, doc, err := res.Iter.Next()
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, must.NotFail(doc.Get("_id")).(int32))
			}
			if !slices.Equal(ids, tc.want) {
				t.Errorf("ids %v, want %v", ids, tc.want)
			}
			// The limit's keys, plus one read ahead per range.
			if examined.Keys() > int64(len(tc.want)+2) {
				t.Errorf("read %d keys for %d documents", examined.Keys(), len(tc.want))
			}
		})
	}

	t.Run("another filtered field declines", func(t *testing.T) {
		filter := in(int32(1), int32(3))
		filter.Set("k", must.NotFail(types.NewDocument("$gt", int32(10))))
		res, plan, err := c.query(context.Background(), &backends.QueryParams{Filter: filter, Sort: sortOn("k", 1), LimitHint: 4})
		if err != nil {
			t.Fatal(err)
		}
		res.Iter.Close()
		if slices.Contains(planChain(plan), "SORT_MERGE") {
			t.Errorf("plan %v merges ranges the k bound cuts", planChain(plan))
		}
	})
}

// A SORT_MERGE reads at most maxMergedScans ranges, as MongoDB's explode limit
// does; a larger $in sorts another way, and both return the same documents.
func TestSortMergeScanLimit(t *testing.T) {
	b, _ := newDottedPathTestBackend(t, backends.IndexInfo{Name: "grp_k", Key: []backends.IndexKeyPair{{Field: "grp"}, {Field: "k"}}})
	c := &collection{db: &database{backend: b, name: "idxdb", rootish: defaultBranch}, name: "tests"}
	var docs []*types.Document
	for i := int32(1); i <= 2000; i++ {
		docs = append(docs, must.NotFail(types.NewDocument("_id", i, "grp", i%1000, "k", i)))
	}
	if _, err := c.InsertAll(context.Background(), &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{maxMergedScans, maxMergedScans + 1} {
		vals := make([]any, n)
		for i := range vals {
			vals[i] = int32(i)
		}
		filter := must.NotFail(types.NewDocument("grp", must.NotFail(types.NewDocument("$in", must.NotFail(types.NewArray(vals...))))))
		res, plan, err := c.query(context.Background(), &backends.QueryParams{Filter: filter, Sort: sortOn("k", 1), LimitHint: 5})
		if err != nil {
			t.Fatal(err)
		}
		res.Iter.Close()
		if merged := slices.Contains(planChain(plan), "SORT_MERGE"); merged != (n <= maxMergedScans) {
			t.Errorf("$in of %d: plan %v", n, planChain(plan))
		}
	}
}
