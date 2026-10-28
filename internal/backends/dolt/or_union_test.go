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
	"slices"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func orFilter(branches ...*types.Document) *types.Document {
	arr := types.MakeArray(len(branches))
	for _, b := range branches {
		arr.Append(b)
	}
	return must.NotFail(types.NewDocument("$or", arr))
}

func eqDoc(field string, v any) *types.Document {
	return must.NotFail(types.NewDocument(field, v))
}

func TestOrUnionReadsOnlyBranchMatches(t *testing.T) {
	c := newSortVsFilterCollection(t, 100)
	for _, tc := range []struct {
		name   string
		filter *types.Document
		want   []int32
	}{
		{"one match per branch", orFilter(eqDoc("k", int32(5)), eqDoc("q", int32(10))), []int32{10, 15}},
		{"overlapping branches", orFilter(eqDoc("q", int32(10)), eqDoc("q", int32(10))), []int32{10}},
		{"range branch", orFilter(eqDoc("q", must.NotFail(types.NewDocument("$lt", int32(3)))), eqDoc("k", int32(5))), []int32{1, 2, 15}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, _ := sortWalkQuery(t, c, &backends.QueryParams{Filter: tc.filter})
			slices.Sort(ids)
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}

func TestOrUnionFallsBackToScan(t *testing.T) {
	c := newSortVsFilterCollection(t, 100)
	for _, tc := range []struct {
		name   string
		filter *types.Document
	}{
		{"unindexed branch", orFilter(eqDoc("k", int32(5)), eqDoc("z", int32(1)))},
		{"union over half the collection", orFilter(eqDoc("p", int32(0)), eqDoc("p", int32(1)))},
		{"nested operator branch", orFilter(eqDoc("k", int32(5)), eqDoc("$and", types.MakeArray(0)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, _ := sortWalkQuery(t, c, &backends.QueryParams{Filter: tc.filter})
			// 101: the fixture's seed document plus 1..100.
			if len(ids) != 101 {
				t.Fatalf("returned %d documents, want a scan of all 101", len(ids))
			}
		})
	}
}
