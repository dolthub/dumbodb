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

func TestNullEqualityReadsTheNullRange(t *testing.T) {
	var docs []*types.Document
	for i := int32(1); i <= 100; i++ {
		switch {
		case i%10 == 0:
			docs = append(docs, must.NotFail(types.NewDocument("_id", i, "n", types.Null)))
		case i%25 == 1:
			docs = append(docs, must.NotFail(types.NewDocument("_id", i)))
		default:
			docs = append(docs, must.NotFail(types.NewDocument("_id", i, "n", i)))
		}
	}
	c := newSortWalkCollection(t, backends.IndexInfo{Name: "n", Key: []backends.IndexKeyPair{{Field: "n"}}}, docs...)
	for _, filter := range []*types.Document{
		must.NotFail(types.NewDocument("n", types.Null)),
		must.NotFail(types.NewDocument("n", must.NotFail(types.NewDocument("$eq", types.Null)))),
	} {
		ids, _ := sortWalkQuery(t, c, &backends.QueryParams{Filter: filter})
		slices.Sort(ids)
		// 0 is the fixture's seed document, which has no n.
		want := []int32{0, 1, 10, 20, 26, 30, 40, 50, 51, 60, 70, 76, 80, 90, 100}
		if !slices.Equal(ids, want) {
			t.Fatalf("%v: ids = %v, want %v", filter, ids, want)
		}
	}
}

func TestNullEqualityIndexable(t *testing.T) {
	key := []backends.IndexKeyPair{{Field: "n"}}
	for _, tc := range []struct {
		idx  backends.IndexInfo
		want bool
	}{
		{backends.IndexInfo{Key: key}, true},
		{backends.IndexInfo{Key: key, Sparse: true}, false},
		{backends.IndexInfo{Key: key, Multikey: true}, false},
		{backends.IndexInfo{Key: []backends.IndexKeyPair{{Field: "a.n"}}}, false},
	} {
		if got := nullEqualityIndexable(tc.idx); got != tc.want {
			t.Errorf("nullEqualityIndexable(%+v) = %v, want %v", tc.idx, got, tc.want)
		}
	}
}
