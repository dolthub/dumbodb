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

// Expectations were recorded against MongoDB 8.0.

func TestSparseExcludes(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	x := backends.IndexInfo{Sparse: true, Key: []backends.IndexKeyPair{{Field: "x"}}}
	ab := backends.IndexInfo{Sparse: true, Key: []backends.IndexKeyPair{{Field: "a.b"}}}
	compound := backends.IndexInfo{Sparse: true, Key: []backends.IndexKeyPair{{Field: "x"}, {Field: "y"}}}
	for _, tc := range []struct {
		name string
		idx  backends.IndexInfo
		doc  *types.Document
		want bool
	}{
		{"missing", x, doc(), true},
		{"explicit null", x, doc("x", types.Null), false},
		{"value", x, doc("x", int32(1)), false},
		{"dotted explicit null", ab, doc("a", doc("b", types.Null)), false},
		{"dotted scalar intermediate", ab, doc("a", int32(5)), true},
		{"dotted missing leaf", ab, doc("a", doc("c", int32(1))), true},
		{"dotted through array", ab, doc("a", must.NotFail(types.NewArray(doc("b", int32(1))))), false},
		{"compound one present", compound, doc("y", types.Null), false},
		{"compound none present", compound, doc("z", int32(1)), true},
		{"not sparse", backends.IndexInfo{Key: x.Key}, doc(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sparseExcludes(tc.doc, tc.idx); got != tc.want {
				t.Fatalf("sparseExcludes = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSparseUniqueExplicitNullCollides(t *testing.T) {
	c := newDottedPathTestCollection(t, backends.IndexInfo{
		Name: "x_sparse", Unique: true, Sparse: true,
		Key: []backends.IndexKeyPair{{Field: "x"}},
	})
	if err := insertDottedPathDoc(c, must.NotFail(types.NewDocument("_id", int32(1)))); err != nil {
		t.Fatalf("second missing-field doc must not collide: %v", err)
	}
	if err := insertDottedPathDoc(c, must.NotFail(types.NewDocument("_id", int32(2), "x", types.Null))); err != nil {
		t.Fatalf("first explicit null: %v", err)
	}
	err := insertDottedPathDoc(c, must.NotFail(types.NewDocument("_id", int32(3), "x", types.Null)))
	if !backends.ErrorCodeIs(err, backends.ErrorCodeInsertDuplicateID) {
		t.Fatalf("second explicit null must collide, got %v", err)
	}
}

func TestQueryHintedSparseIndexReturnsOnlyMembers(t *testing.T) {
	ctx := context.Background()
	c := newDottedPathTestCollection(t, backends.IndexInfo{
		Name: "x_sparse", Sparse: true,
		Key: []backends.IndexKeyPair{{Field: "x"}},
	})
	for _, d := range []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "x", types.Null)),
		must.NotFail(types.NewDocument("_id", int32(2), "x", int32(1))),
	} {
		if err := insertDottedPathDoc(c, d); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(hint any) []int32 {
		res, err := c.Query(ctx, &backends.QueryParams{Filter: must.NotFail(types.NewDocument("x", types.Null)), Hint: hint})
		if err != nil {
			t.Fatal(err)
		}
		defer res.Iter.Close()
		var out []int32
		for {
			_, doc, err := res.Iter.Next()
			if err != nil {
				break
			}
			out = append(out, must.NotFail(doc.Get("_id")).(int32))
		}
		slices.Sort(out)
		return out
	}
	// The backend returns candidates; the handler applies the filter afterwards.
	if got := ids(nil); !slices.Equal(got, []int32{0, 1, 2}) {
		t.Fatalf("unhinted candidates = %v, want [0 1 2]", got)
	}
	if got := ids("x_sparse"); !slices.Equal(got, []int32{1, 2}) {
		t.Fatalf("hinted candidates = %v, want sparse members [1 2]", got)
	}
}
