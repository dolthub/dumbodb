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

// Regression for issue #130: an operator-form filter on a collated-index field
// panicked with "types: unexpected type index.PreEncoded" and closed the
// connection. The issue's reproduction (100 docs s000..s099, an index on s
// under fr_CA, queries under the same collation): $gt returns 9 docs, $eq
// returns 1, and the bounded $gte/$lt range returns 10.

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestCollatedIndexOperatorFilters(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)
	db, err := b.Database("collop")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("c")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	const n = 100
	docs := make([]*types.Document, 0, n)
	for i := 0; i < n; i++ {
		docs = append(docs, must.NotFail(types.NewDocument(
			"_id", int32(i),
			"s", fmt.Sprintf("s%03d", i),
			"i", int32(i),
		)))
	}
	if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	frCA := mustDoc(t, "locale", "fr_CA")
	if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{{
			Name:      "s_idx",
			Key:       []backends.IndexKeyPair{{Field: "s"}},
			Collation: frCA,
		}},
	}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	// lookup runs the query at the backend level and returns the _ids the
	// backend yields. These are index candidates -- plan.fetch does not
	// evaluate the filter (collection.go planIndexLookupIn/fetch), and the
	// handler's FilterIteratorColl re-checks candidates against the full
	// filter. Exact-id assertions are candidate-level: for single-type
	// operand bounds the candidate set equals the final match set; for
	// mixed-type bounds it is a strict superset of the final matches.
	lookup := func(t *testing.T, filter *types.Document) []int32 {
		t.Helper()
		got := drainQuery(t, ctx, coll, &backends.QueryParams{
			Filter:    filter,
			Collated:  true,
			Collation: frCA,
		})
		ids := idsInDocs(t, got)
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		return ids
	}

	t.Run("gt", func(t *testing.T) {
		op := must.NotFail(types.NewDocument("$gt", "s090"))
		ids := lookup(t, must.NotFail(types.NewDocument("s", op)))
		want := make([]int32, 0, 9)
		for i := int32(91); i < n; i++ {
			want = append(want, i)
		}
		if len(ids) != 9 || !reflect.DeepEqual(ids, want) {
			t.Errorf("$gt s090 = %v, want [91..99] (9 docs)", ids)
		}
	})

	t.Run("eq", func(t *testing.T) {
		op := must.NotFail(types.NewDocument("$eq", "s050"))
		ids := lookup(t, must.NotFail(types.NewDocument("s", op)))
		if len(ids) != 1 || !reflect.DeepEqual(ids, []int32{50}) {
			t.Errorf("$eq s050 = %v, want [50]", ids)
		}
	})

	t.Run("bounded_range", func(t *testing.T) {
		op := must.NotFail(types.NewDocument("$gte", "s010", "$lt", "s020"))
		ids := lookup(t, must.NotFail(types.NewDocument("s", op)))
		want := make([]int32, 0, 10)
		for i := int32(10); i < 20; i++ {
			want = append(want, i)
		}
		if len(ids) != 10 || !reflect.DeepEqual(ids, want) {
			t.Errorf("$gte s010, $lt s020 = %v, want [10..19] (10 docs)", ids)
		}
	})

	// Non-string operator values must pass through the collation conversion
	// unchanged (collateFilterValue only rewrites string operands). Two
	// consequences, both asserted at the candidate level drainQuery observes:
	//
	// (a) A numeric comparison against the collated field reaches the
	//     s_idx as a numeric type bracket: every stored s key is a string
	//     (leading ctype 0x3C) and the numeric bracket lies entirely below
	//     it, so the candidate set is empty -- and so is the final match
	//     set under MongoDB's type-bracketed range semantics. Exact at
	//     both layers.
	//
	// (b) Numeric bounds against the binary-indexed field i cannot consult
	//     i_idx: the identity rule (planIndexLookupIn, collection.go) keeps a
	//     collated query off collation-mismatched indexes, so the plan is a
	//     collated scan, which returns the whole collection as candidates
	//     (no byte prefilter under collation) for the handler re-filter to
	//     narrow. A full-collection candidate set fails if the planner ever
	//     regresses into serving a collated query from the binary index:
	//     byte-range bounds on collation-mismatched indexes are unsound,
	//     which is why the identity rule exists.
	t.Run("nonstring_operator_values", func(t *testing.T) {
		op := must.NotFail(types.NewDocument("$gt", int32(90)))
		sIDs := lookup(t, must.NotFail(types.NewDocument("s", op)))
		if len(sIDs) != 0 {
			t.Errorf("$gt 90 candidates on collated s = %v, want empty (0 docs)", sIDs)
		}

		if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
			Indexes: []backends.IndexInfo{{
				Name: "i_idx",
				Key:  []backends.IndexKeyPair{{Field: "i"}},
			}},
		}); err != nil {
			t.Fatalf("CreateIndexes (binary): %v", err)
		}

		opI := must.NotFail(types.NewDocument("$gte", int32(10), "$lt", int32(20)))
		iIDs := lookup(t, must.NotFail(types.NewDocument("i", opI)))
		if len(iIDs) != n {
			t.Errorf("binary-field numeric range candidates = %d, want %d (whole-collection scan candidates; the collated query must not use the binary index)", len(iIDs), n)
		}
	})

	// Mixed operand types on one side of a collated operator filter. The
	// lower-bound merge in indexBoundsForFilterValue keeps one operand by
	// BSON type order (types.Compare orders strings after numbers), so both
	// insertion orders leave the collated string bound ($gt, exclusive) as
	// the surviving lower bound and bracket the byte range by the string
	// ctype only. Whether a document can simultaneously satisfy
	// {compare-operator: number} and {compare-operator: string} is decided at
	// the handler re-filter (MongoDB type-brackets range operators, so the
	// final filtered result is 0 docs; the backend cannot express that with
	// one contiguous byte range and is documented to return a sound
	// superset). drainQuery observes backend candidates, not final results,
	// so what these cases pin is the merge itself: order-independence, no
	// panic on the collated operand (the pre-fix code panicked here), and
	// the identical candidate set [91..99] -- exactly the docs whose s sort
	// key survives the merged bound -- for both orders.
	t.Run("mixed_type_lower_bounds", func(t *testing.T) {
		op := must.NotFail(types.NewDocument("$gte", int32(5), "$gt", "s090"))
		ids := lookup(t, must.NotFail(types.NewDocument("s", op)))
		want := make([]int32, 0, 9)
		for i := int32(91); i < n; i++ {
			want = append(want, i)
		}
		if len(ids) != 9 || !reflect.DeepEqual(ids, want) {
			t.Errorf("$gte 5, $gt s090 candidates = %v, want [91..99] (9 docs)", ids)
		}
	})

	t.Run("mixed_type_lower_bounds_reversed", func(t *testing.T) {
		op := must.NotFail(types.NewDocument("$gt", "s090", "$gte", int32(5)))
		ids := lookup(t, must.NotFail(types.NewDocument("s", op)))
		want := make([]int32, 0, 9)
		for i := int32(91); i < n; i++ {
			want = append(want, i)
		}
		if len(ids) != 9 || !reflect.DeepEqual(ids, want) {
			t.Errorf("$gt s090, $gte 5 candidates = %v, want [91..99] (9 docs)", ids)
		}
	})
}

// TestCollatedIndexOperatorSeeksFewerNodes checks, at the seek level (not just
// "no panic"), that an operator filter on a collated index is served by the
// sort-key index: it must read fewer prolly-tree nodes than a full scan.
func TestCollatedIndexOperatorSeeksFewerNodes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)
	db, err := b.Database("collopseek")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("c")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	const n = 5000
	docs := make([]*types.Document, 0, n)
	for i := 0; i < n; i++ {
		docs = append(docs, must.NotFail(types.NewDocument(
			"_id", int32(i),
			"s", fmt.Sprintf("s%04d", i),
		)))
	}
	if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	frCA := mustDoc(t, "locale", "fr_CA")
	if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{{
			Name:      "s_idx",
			Key:       []backends.IndexKeyPair{{Field: "s"}},
			Collation: frCA,
		}},
	}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	seekCtx, seek := WithSeekCounter(ctx)
	seekDocs := drainQuery(t, seekCtx, coll, &backends.QueryParams{
		Filter: must.NotFail(types.NewDocument("s",
			must.NotFail(types.NewDocument("$gte", "s0100", "$lt", "s0110")))),
		Collated:  true,
		Collation: frCA,
	})
	if len(seekDocs) != 10 {
		t.Errorf("collated range candidates = %d, want 10", len(seekDocs))
	}

	scanCtx, scan := WithSeekCounter(ctx)
	allDocs := drainQuery(t, scanCtx, coll, &backends.QueryParams{
		Filter: must.NotFail(types.NewDocument()),
	})
	if len(allDocs) != n {
		t.Fatalf("full scan returned %d docs, want %d", len(allDocs), n)
	}

	t.Logf("collated range nodes=%d; full scan nodes=%d", seek.Nodes(), scan.Nodes())

	if seek.Nodes() == 0 {
		t.Fatal("instrumentation dead: collated seek counted 0 node reads")
	}
	if scan.Nodes() == 0 {
		t.Fatal("instrumentation dead: full scan counted 0 node reads")
	}
	if seek.Nodes() >= scan.Nodes() {
		t.Fatalf("collated operator seek not served by the index: seek nodes=%d >= scan nodes=%d",
			seek.Nodes(), scan.Nodes())
	}
}

// TestCollatedIndexOperatorFiltersCollationOrder uses case and accent variants,
// whose collated order differs from their binary order, so a bound built from
// the wrong key space drops or admits documents. Numeric fillers keep string
// ranges selective enough that the planner does not fall back to a scan.
func TestCollatedIndexOperatorFiltersCollationOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	vals := []any{
		"a", "A", "ab", "aB", "Ab", "b", "B", "c", "C", "cote", "cot\u00e9", "c\u00f4te", "c\u00f4t\u00e9",
		"e", "E", "\u00e9", "z", "Z", "", int32(5), int32(50), true,
	}
	const fillers = 200
	op := func(kv ...any) *types.Document {
		return must.NotFail(types.NewDocument("s", must.NotFail(types.NewDocument(kv...))))
	}
	arr := func(v ...any) *types.Array { return must.NotFail(types.NewArray(v...)) }

	// exact: single-type string bounds, where the index range is the match set.
	type filterCase struct {
		name   string
		filter *types.Document
		exact  bool
	}
	cases := []filterCase{
		{"eq_B", op("$eq", "B"), true},
		{"gt_b", op("$gt", "b"), true},
		{"gte_B", op("$gte", "B"), true},
		{"lt_c", op("$lt", "c"), true},
		{"lte_C", op("$lte", "C"), true},
		{"range_b_d", op("$gte", "b", "$lt", "d"), true},
		{"range_cote", op("$gt", "cote", "$lte", "c\u00f4t\u00e9"), true},
		{"range_E_f", op("$gte", "E", "$lt", "f"), true},
		{"in_B_z", op("$in", arr("B", "z")), false},
		{"in_mixed", op("$in", arr("A", int32(5))), false},
		{"ne_B", op("$ne", "B"), false},
		{"not_gt_b", op("$not", must.NotFail(types.NewDocument("$gt", "b"))), false},
		{"mixed_lower", op("$gte", int32(5), "$gt", "b"), false},
		{"mixed_upper", op("$lt", int32(60), "$lte", "c"), false},
		{"eq_and_gte", op("$eq", "b", "$gte", "B"), false},
	}

	for _, spec := range [][]any{
		{"locale", "en", "strength", int32(1)},
		{"locale", "en", "strength", int32(2)},
		{"locale", "fr_CA"},
	} {
		collDoc := must.NotFail(types.NewDocument(spec...))
		t.Run(fmt.Sprint(spec), func(t *testing.T) {
			t.Parallel()

			b := newTestBackend(t)
			db := must.NotFail(b.Database("collorder"))
			coll := must.NotFail(db.Collection("c"))
			docs := make([]*types.Document, 0, len(vals)+fillers)
			for i, v := range vals {
				docs = append(docs, must.NotFail(types.NewDocument("_id", int32(i), "s", v)))
			}
			for i := len(vals); i < len(vals)+fillers; i++ {
				docs = append(docs, must.NotFail(types.NewDocument("_id", int32(i), "s", int32(1000+i))))
			}
			if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs}); err != nil {
				t.Fatalf("InsertAll: %v", err)
			}
			if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
				Indexes: []backends.IndexInfo{{
					Name:      "s_idx",
					Key:       []backends.IndexKeyPair{{Field: "s"}},
					Collation: collDoc,
				}},
			}); err != nil {
				t.Fatalf("CreateIndexes: %v", err)
			}
			cmp := collation.Parse(collDoc).Comparator()

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					var want []int32
					for _, d := range docs {
						if must.NotFail(common.FilterDocumentColl(d, tc.filter, cmp)) {
							want = append(want, must.NotFail(d.Get("_id")).(int32))
						}
					}
					got := idsInDocs(t, drainQuery(t, ctx, coll, &backends.QueryParams{
						Filter:    tc.filter,
						Collated:  true,
						Collation: collDoc,
					}))
					sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })

					candidates := make(map[int32]bool, len(got))
					for _, id := range got {
						candidates[id] = true
					}
					for _, id := range want {
						if !candidates[id] {
							t.Errorf("candidates %v miss match _id %d", got, id)
						}
					}
					if !tc.exact {
						return
					}
					if !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
						t.Errorf("candidates = %v, want %v", got, want)
					}
					res, err := coll.Count(ctx, &backends.CountParams{Filter: tc.filter, Collation: collDoc})
					if err != nil {
						t.Fatalf("Count: %v", err)
					}
					if res.Filtered && res.Count != int64(len(want)) {
						t.Errorf("indexed count = %d, want %d", res.Count, len(want))
					}
				})
			}
		})
	}
}
