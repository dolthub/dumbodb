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

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/must"
)

// TestDropIndexes_AfterDropAll verifies that after dropIndexes("*"), ListIndexes
// returns only the default _id_ index (regression for do-5ew7).
func TestDropIndexes_AfterDropAll(t *testing.T) {
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

	// Insert a document so the collection exists.
	doc := must.NotFail(types.NewDocument("_id", int32(1), "x", int32(42)))
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	// Create two secondary indexes.
	_, err = coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{
			{Name: "x_1", Key: []backends.IndexKeyPair{{Field: "x"}}},
			{Name: "y_1", Key: []backends.IndexKeyPair{{Field: "y"}}},
		},
	})
	if err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	// Confirm both secondary indexes are listed.
	listRes, err := coll.ListIndexes(ctx, nil)
	if err != nil {
		t.Fatalf("ListIndexes before drop: %v", err)
	}

	if len(listRes.Indexes) != 3 { // _id_ + x_1 + y_1
		t.Fatalf("expected 3 indexes before drop, got %d: %+v", len(listRes.Indexes), listRes.Indexes)
	}

	// Drop all non-_id indexes (simulates dropIndexes("*")).
	toDrop := make([]string, 0, len(listRes.Indexes))
	for _, idx := range listRes.Indexes {
		if idx.Name != backends.DefaultIndexName {
			toDrop = append(toDrop, idx.Name)
		}
	}

	_, err = coll.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: toDrop})
	if err != nil {
		t.Fatalf("DropIndexes: %v", err)
	}

	// After drop, only _id_ should remain.
	listRes, err = coll.ListIndexes(ctx, nil)
	if err != nil {
		t.Fatalf("ListIndexes after drop: %v", err)
	}

	if len(listRes.Indexes) != 1 {
		t.Fatalf("expected 1 index after drop, got %d: %+v", len(listRes.Indexes), listRes.Indexes)
	}

	if listRes.Indexes[0].Name != backends.DefaultIndexName {
		t.Errorf("expected _id_ index, got %q", listRes.Indexes[0].Name)
	}
}

// TestDropIndexes_Single verifies that dropping a single named index removes only that index.
func TestDropIndexes_Single(t *testing.T) {
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

	doc := must.NotFail(types.NewDocument("_id", int32(1), "x", int32(1)))
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	_, err = coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{
			{Name: "x_1", Key: []backends.IndexKeyPair{{Field: "x"}}},
			{Name: "z_1", Key: []backends.IndexKeyPair{{Field: "z"}}},
		},
	})
	if err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	_, err = coll.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: []string{"x_1"}})
	if err != nil {
		t.Fatalf("DropIndexes: %v", err)
	}

	listRes, err := coll.ListIndexes(ctx, nil)
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}

	// Should have _id_ and z_1.
	if len(listRes.Indexes) != 2 {
		t.Fatalf("expected 2 indexes, got %d: %+v", len(listRes.Indexes), listRes.Indexes)
	}

	for _, idx := range listRes.Indexes {
		if idx.Name == "x_1" {
			t.Errorf("x_1 should have been dropped but is still present")
		}
	}
}
