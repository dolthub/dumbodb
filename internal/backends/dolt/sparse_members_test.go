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

func newSparseCollection(t *testing.T) backends.Collection {
	var docs []*types.Document
	for i := int32(1); i <= 100; i++ {
		d := must.NotFail(types.NewDocument("_id", i, "pad", i))
		switch i % 10 {
		case 0:
			d.Set("sp", i)
		case 5:
			d.Set("sp", types.Null)
		}
		docs = append(docs, d)
	}
	return newSortWalkCollection(t, backends.IndexInfo{Name: "sp", Key: []backends.IndexKeyPair{{Field: "sp"}}, Sparse: true}, docs...)
}

func sparseMemberIDs() []int32 {
	var ids []int32
	for i := int32(1); i <= 100; i++ {
		if i%10 == 0 || i%10 == 5 {
			ids = append(ids, i)
		}
	}
	return ids
}

func TestExistsReadsTheSparseIndex(t *testing.T) {
	c := newSparseCollection(t)
	for _, e := range []any{true, int32(1)} {
		ids, _ := sortWalkQuery(t, c, &backends.QueryParams{Filter: must.NotFail(types.NewDocument("sp", must.NotFail(types.NewDocument("$exists", e))))})
		slices.Sort(ids)
		if !slices.Equal(ids, sparseMemberIDs()) {
			t.Fatalf("$exists %v: ids = %v, want %v", e, ids, sparseMemberIDs())
		}
	}
	if ids, _ := sortWalkQuery(t, c, &backends.QueryParams{Filter: must.NotFail(types.NewDocument("sp", must.NotFail(types.NewDocument("$exists", false))))}); len(ids) != 101 {
		t.Fatalf("$exists false must scan, returned %d documents", len(ids))
	}
}

func TestSparseHintReadsMembers(t *testing.T) {
	c := newSparseCollection(t)
	filter := must.NotFail(types.NewDocument("pad", must.NotFail(types.NewDocument("$exists", true))))
	ids, _ := sortWalkQuery(t, c, &backends.QueryParams{Filter: filter, Hint: "sp"})
	slices.Sort(ids)
	if !slices.Equal(ids, sparseMemberIDs()) {
		t.Fatalf("ids = %v, want %v", ids, sparseMemberIDs())
	}
}

func TestExistsIndexable(t *testing.T) {
	key := []backends.IndexKeyPair{{Field: "sp"}}
	for _, tc := range []struct {
		idx  backends.IndexInfo
		want bool
	}{
		{backends.IndexInfo{Key: key, Sparse: true}, true},
		{backends.IndexInfo{Key: key}, false},
		{backends.IndexInfo{Key: append(key, backends.IndexKeyPair{Field: "x"}), Sparse: true}, false},
		{backends.IndexInfo{Key: []backends.IndexKeyPair{{Field: "a.sp"}}, Sparse: true}, false},
	} {
		if got := existsIndexable(tc.idx); got != tc.want {
			t.Errorf("existsIndexable(%+v) = %v, want %v", tc.idx, got, tc.want)
		}
	}
}
