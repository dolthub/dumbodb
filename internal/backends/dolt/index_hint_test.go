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

// TestTryIndexLookup_HonorsHint verifies that the runtime read path honors an
// index hint, the same selection Explain reports. The runtime index choice is
// not observable over the wire (results are identical and executionStats is
// derived from the explain planner, not from execution), so this is a
// white-box assertion on tryIndexLookup's "used" return: a hint naming the
// index that covers the filter selects it (used), a hint naming an index that
// does NOT cover the filter forces a collection scan (not used), and the
// default selection still uses an index.
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
	// *collection (same db/branch) to reach the unexported tryIndexLookup.
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

	// Filter constrains field "a", which a_1 covers and b_1 does not.
	filter := must.NotFail(types.NewDocument("a", int32(3)))
	keyPatternB := must.NotFail(types.NewDocument("b", int32(1)))

	cases := []struct {
		name     string
		hint     any
		wantUsed bool
	}{
		{"no hint uses an index", nil, true},
		{"hint covering index (by name) is used", "a_1", true},
		{"hint non-covering index (by name) forces scan", "b_1", false},
		{"hint non-covering index (by key pattern) forces scan", keyPatternB, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotDocs, used, err := c.tryIndexLookup(ctx, state, m, filter, tc.hint)
			if err != nil {
				t.Fatalf("tryIndexLookup: %v", err)
			}
			if used != tc.wantUsed {
				t.Fatalf("used = %v, want %v", used, tc.wantUsed)
			}
			// When the index path is used it must return the one matching doc;
			// the result is correct regardless of which path runs.
			if used {
				if len(gotDocs) != 1 {
					t.Fatalf("index path returned %d docs, want 1", len(gotDocs))
				}
				id, _ := gotDocs[0].Get("_id")
				if id != int32(3) {
					t.Fatalf("matched _id = %v, want 3", id)
				}
			}
		})
	}
}
