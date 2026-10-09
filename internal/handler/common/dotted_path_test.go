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
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func dpDoc(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
func dpArr(vals ...any) *types.Array     { return must.NotFail(types.NewArray(vals...)) }

// dottedPathDocs mirrors the probe set whose matches were recorded against
// MongoDB 8.0.
func dottedPathDocs() []*types.Document {
	return []*types.Document{
		dpDoc("_id", "a_empty", "a", dpArr()),
		dpDoc("_id", "a_scalars", "a", dpArr(int32(1), int32(2))),
		dpDoc("_id", "a_mixed", "a", dpArr(int32(1), dpDoc("b", int32(2)))),
		dpDoc("_id", "a_nested_missing", "a", dpArr(dpArr(dpDoc("c", int32(1))))),
		dpDoc("_id", "a_doc_b_arr", "a", dpDoc("b", dpArr(int32(1), int32(3)))),
		dpDoc("_id", "a_doc_b_empty", "a", dpDoc("b", dpArr())),
		dpDoc("_id", "a_arr_b_arr_missing", "a", dpArr(dpDoc("b", dpArr(dpDoc("c", int32(1)), dpDoc("d", int32(1)))))),
		dpDoc("_id", "a_arr_null_elem", "a", dpArr(types.Null, dpDoc("b", int32(2)))),
		dpDoc("_id", "a_arr_elem_lacks_b", "a", dpArr(dpDoc("b", int32(5)), dpDoc("c", int32(1)))),
		dpDoc("_id", "a_scalar", "a", int32(5)),
		dpDoc("_id", "no_a"),
	}
}

func TestFilterDottedPathThroughArrays(t *testing.T) {
	cases := []struct {
		name   string
		filter *types.Document
		want   []string
	}{
		{"eq null", dpDoc("a.b", types.Null), []string{"a_arr_elem_lacks_b", "a_scalar", "no_a"}},
		{"eq null positional", dpDoc("a.0", types.Null), []string{
			"a_arr_b_arr_missing", "a_arr_elem_lacks_b", "a_arr_null_elem", "a_doc_b_arr", "a_doc_b_empty", "a_mixed", "a_scalar", "no_a",
		}},
		{"eq null deep", dpDoc("a.b.c", types.Null), []string{
			"a_arr_b_arr_missing", "a_arr_elem_lacks_b", "a_arr_null_elem", "a_mixed", "a_scalar", "no_a",
		}},
		{"in with null", dpDoc("a.b", dpDoc("$in", dpArr(int32(5), types.Null))), []string{"a_arr_elem_lacks_b", "a_scalar", "no_a"}},
		{"exists false", dpDoc("a.b", dpDoc("$exists", false)), []string{"a_empty", "a_nested_missing", "a_scalar", "a_scalars", "no_a"}},
		{"ne", dpDoc("a.b", dpDoc("$ne", int32(2))), []string{
			"a_arr_b_arr_missing", "a_arr_elem_lacks_b", "a_doc_b_arr", "a_doc_b_empty", "a_empty", "a_nested_missing", "a_scalar", "a_scalars", "no_a",
		}},
		{"nin", dpDoc("a.b", dpDoc("$nin", dpArr(int32(2), int32(5)))), []string{
			"a_arr_b_arr_missing", "a_doc_b_arr", "a_doc_b_empty", "a_empty", "a_nested_missing", "a_scalar", "a_scalars", "no_a",
		}},
		{"not gt", dpDoc("a.b", dpDoc("$not", dpDoc("$gt", int32(1)))), []string{
			"a_arr_b_arr_missing", "a_doc_b_empty", "a_empty", "a_nested_missing", "a_scalar", "a_scalars", "no_a",
		}},
		{"range ops evaluated separately", dpDoc("a.b", dpDoc("$gt", int32(0), "$lt", int32(2))), []string{"a_doc_b_arr"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, d := range dottedPathDocs() {
				ok, err := FilterDocument(d, tc.filter)
				if err != nil {
					t.Fatalf("FilterDocument: %v", err)
				}
				if ok {
					got = append(got, must.NotFail(d.Get("_id")).(string))
				}
			}
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("matched %v, want %v", got, want)
			}
		})
	}
}

func TestFilterDottedPathOperatorErrorWithoutBranches(t *testing.T) {
	_, err := FilterDocument(dpDoc("a", dpArr()), dpDoc("a.b", dpDoc("$in", int32(1))))
	if err == nil {
		t.Fatal("$in with a non-array argument must error even when the path reaches no values")
	}
}

func TestSortDottedPathThroughArrays(t *testing.T) {
	docs := []*types.Document{
		dpDoc("_id", int32(1), "a", dpDoc("b", int32(3))),
		dpDoc("_id", int32(2), "a", dpArr(dpDoc("b", int32(1)), dpDoc("b", int32(4)))),
		dpDoc("_id", int32(3), "a", dpArr(dpDoc("b", int32(5)), dpDoc("c", int32(1)))),
		dpDoc("_id", int32(4), "a", dpDoc("c", int32(1))),
		dpDoc("_id", int32(5), "a", dpArr(dpDoc("b", dpArr(int32(0), int32(6))), dpDoc("b", int32(2)))),
	}
	for _, tc := range []struct {
		name string
		dir  int32
		want []int32
	}{
		// Ascending takes the minimum reached value; a missing branch counts as null.
		{"asc", 1, []int32{3, 4, 5, 2, 1}},
		// Descending takes the maximum reached value.
		{"desc", -1, []int32{5, 3, 2, 1, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sorted := slices.Clone(docs)
			if err := SortDocuments(sorted, dpDoc("a.b", tc.dir, "_id", int32(1))); err != nil {
				t.Fatalf("SortDocuments: %v", err)
			}
			var got []int32
			for _, d := range sorted {
				got = append(got, must.NotFail(d.Get("_id")).(int32))
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("order %v, want %v", got, tc.want)
			}
		})
	}
}

// Expectations were recorded against MongoDB 8.0 (unfiltered distinct over an
// index on a.b, answered by DISTINCT_SCAN).
func TestIndexKeyDistinctValues(t *testing.T) {
	docs := []*types.Document{
		dpDoc("_id", int32(1), "a", dpDoc("b", int32(1))),
		dpDoc("_id", int32(2), "a", dpDoc("b", dpArr())),
		dpDoc("_id", int32(3), "a", dpDoc("c", int32(1))),
		dpDoc("_id", int32(4), "a", dpArr(dpDoc("b", int32(2)), dpDoc("c", int32(1)))),
		dpDoc("_id", int32(5), "a", dpDoc("b", dpArr(int32(3), dpArr(int32(4), int32(5))))),
		dpDoc("_id", int32(6), "a", dpDoc("b", types.Null)),
		dpDoc("_id", int32(7), "a", dpArr()),
		dpDoc("_id", int32(8), "a", int32(9)),
	}
	all := []any{types.Undefined, types.Null, int32(1), int32(2), int32(3), int32(4), int32(5)}
	for _, tc := range []struct {
		name   string
		sparse bool
	}{
		{"non-sparse", false},
		{"sparse", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := IndexKeyDistinctValues(iterator.Values(iterator.ForSlice(docs)), "a.b", tc.sparse)
			if err != nil {
				t.Fatal(err)
			}
			if got.Len() != len(all) {
				t.Fatalf("got %v, want %v", got, all)
			}
			for i, want := range all {
				if !types.Identical(must.NotFail(got.Get(i)), want) {
					t.Fatalf("got %v, want %v", got, all)
				}
			}
		})
	}
}
