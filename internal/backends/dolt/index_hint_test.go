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
	"github.com/dolthub/dumbodb/internal/util/must"
)

// This is a white-box assertion on tryIndexLookup's "used" return and the
// documents it reads.
func TestTryIndexLookup_HonorsHint(t *testing.T) {
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

	docs := make([]*types.Document, 0, 6)
	for i := int32(1); i <= 6; i++ {
		docs = append(docs, must.NotFail(types.NewDocument("_id", i, "a", i, "b", 100+i)))
	}
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	if _, err = coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{
			{Name: "a_1", Key: []backends.IndexKeyPair{{Field: "a"}}},
			{Name: "b_1", Key: []backends.IndexKeyPair{{Field: "b"}}},
		},
	}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	// db.Collection wraps the collection in a contract; rebuild the raw
	// *collection to reach the unexported tryIndexLookup.
	c := &collection{
		db:   &database{backend: b, name: "testdb", rootish: defaultBranch},
		name: "testcoll",
	}

	m, exists, state, err := c.getMap(ctx)
	if err != nil {
		t.Fatalf("getMap: %v", err)
	}
	if !exists {
		t.Fatal("collection map does not exist")
	}

	// a_1 covers field "a"; b_1 does not.
	filter := must.NotFail(types.NewDocument("a", int32(3)))
	keyPatternB := must.NotFail(types.NewDocument("b", int32(1)))

	// A hinted index the filter does not bound is read whole, as on MongoDB.
	cases := []struct {
		name     string
		hint     any
		wantDocs int
	}{
		{"no hint uses an index", nil, 1},
		{"hint covering index (by name) is used", "a_1", 1},
		{"hint non-covering index (by name) is read whole", "b_1", 6},
		{"hint non-covering index (by key pattern) is read whole", keyPatternB, 6},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotDocs, used, err := c.tryIndexLookup(ctx, state, m, filter, tc.hint, nil)
			if err != nil {
				t.Fatalf("tryIndexLookup: %v", err)
			}
			if !used {
				t.Fatal("used = false, want the index")
			}
			if len(gotDocs) != tc.wantDocs {
				t.Fatalf("index path returned %d docs, want %d", len(gotDocs), tc.wantDocs)
			}
			if tc.wantDocs == 1 {
				if id, _ := gotDocs[0].Get("_id"); id != int32(3) {
					t.Fatalf("matched _id = %v, want 3", id)
				}
			}
		})
	}
}
