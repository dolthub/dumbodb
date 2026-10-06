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
	"reflect"
	"sort"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestTryIndexLookup_In(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)
	db := must.NotFail(b.Database("testdb"))
	coll := must.NotFail(db.Collection("testcoll"))

	docs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "a", int32(1))),
		must.NotFail(types.NewDocument("_id", int32(2), "a", int32(1))),
		must.NotFail(types.NewDocument("_id", int32(3), "a", int64(2))),
		must.NotFail(types.NewDocument("_id", int32(4), "a", float64(3))),
		must.NotFail(types.NewDocument("_id", int32(5), "a", "x")),
		must.NotFail(types.NewDocument("_id", int32(6), "a", must.NotFail(types.NewArray(int32(4), int32(5))))),
		must.NotFail(types.NewDocument("_id", int32(7), "a", types.Null)),
		must.NotFail(types.NewDocument("_id", int32(8))),
	}
	for i := int32(100); i < 120; i++ {
		docs = append(docs, must.NotFail(types.NewDocument("_id", i, "a", i)))
	}
	_, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs})
	must.NoError(err)
	_, err = coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{{Name: "a_1", Key: []backends.IndexKeyPair{{Field: "a"}}}},
	})
	must.NoError(err)

	c := &collection{db: &database{backend: b, name: "testdb", rootish: defaultBranch}, name: "testcoll"}
	m, _, state, err := c.getMap(ctx)
	must.NoError(err)

	in := func(values ...any) *types.Document {
		return must.NotFail(types.NewDocument("a", must.NotFail(types.NewDocument("$in", must.NotFail(types.NewArray(values...))))))
	}

	for _, tc := range []struct {
		name     string
		filter   *types.Document
		wantUsed bool
		wantIDs  []int32
	}{
		{"mixed numeric types and strings", in(int32(1), float64(2), "x"), true, []int32{1, 2, 3, 5}},
		{"int64 matches double", in(int64(3)), true, []int32{4}},
		{"element of an array field", in(int32(5)), true, []int32{6}},
		{"duplicate values", in(int32(1), int32(1)), true, []int32{1, 2}},
		{"empty list", in(), true, nil},
		{"null declines the index", in(types.Null), false, nil},
		{"array value declines the index", in(must.NotFail(types.NewArray(int32(4), int32(5)))), false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, used, err := c.tryIndexLookup(ctx, state, m, tc.filter, nil, nil)
			if err != nil {
				t.Fatalf("tryIndexLookup: %v", err)
			}
			if used != tc.wantUsed {
				t.Fatalf("used = %v, want %v", used, tc.wantUsed)
			}
			if !used {
				return
			}
			var ids []int32
			for _, d := range got {
				ids = append(ids, must.NotFail(d.Get("_id")).(int32))
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			if !reflect.DeepEqual(ids, tc.wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}
