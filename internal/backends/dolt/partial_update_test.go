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
)

// TestUpdateAll_FieldMutations_PartialSet verifies that an UpdateAll call with
// FieldMutations populated produces a document byte-identical to the same
// update run through the full-rewrite path.
func TestUpdateAll_FieldMutations_PartialSet(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	orig := mustDoc(t, "_id", int64(1), "x", int64(10), "y", "keep", "z", int64(30))
	insertDoc(t, b, "testdb", "c", orig)

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("c")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	// Partial update: set x=99.
	updated := mustDoc(t, "_id", int64(1), "x", int64(99), "y", "keep", "z", int64(30))

	_, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{
		Docs: []*types.Document{updated},
		FieldMutations: [][]backends.FieldMutation{
			{{Key: "x", Value: int64(99)}},
		},
	})
	if err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	// Read back and verify.
	res, err := coll.Query(ctx, &backends.QueryParams{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	iter := res.Iter
	defer iter.Close()

	_, got, err := iter.Next()
	if err != nil {
		t.Fatalf("iter.Next: %v", err)
	}

	if v, _ := got.Get("x"); v != int64(99) {
		t.Errorf("x = %v, want 99", v)
	}
	if v, _ := got.Get("y"); v != "keep" {
		t.Errorf("y = %v, want keep", v)
	}
	if v, _ := got.Get("z"); v != int64(30) {
		t.Errorf("z = %v, want 30", v)
	}
}

// TestUpdateAll_FieldMutations_Unset verifies that $unset via FieldMutations
// removes the named field from the stored document.
func TestUpdateAll_FieldMutations_Unset(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	orig := mustDoc(t, "_id", int64(1), "a", int64(1), "b", int64(2), "c", int64(3))
	insertDoc(t, b, "testdb", "c", orig)

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("c")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	// Updated doc without "b".
	updated := mustDoc(t, "_id", int64(1), "a", int64(1), "c", int64(3))

	_, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{
		Docs: []*types.Document{updated},
		FieldMutations: [][]backends.FieldMutation{
			{{Key: "b", Unset: true}},
		},
	})
	if err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	res, err := coll.Query(ctx, &backends.QueryParams{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	iter := res.Iter
	defer iter.Close()

	_, got, err := iter.Next()
	if err != nil {
		t.Fatalf("iter.Next: %v", err)
	}

	if got.Has("b") {
		t.Errorf("b should have been unset; doc = %v", got)
	}
	if v, _ := got.Get("a"); v != int64(1) {
		t.Errorf("a = %v, want 1", v)
	}
	if v, _ := got.Get("c"); v != int64(3) {
		t.Errorf("c = %v, want 3", v)
	}
}

// TestUpdateAll_FieldMutations_InsertNewField verifies that setting a field
// that did not previously exist in the document works through the partial
// path (IndexedJsonDocument.Set handles this as an insert).
func TestUpdateAll_FieldMutations_InsertNewField(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	orig := mustDoc(t, "_id", int64(1), "a", int64(1))
	insertDoc(t, b, "testdb", "c", orig)

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("c")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	updated := mustDoc(t, "_id", int64(1), "a", int64(1), "touched", int64(42))

	_, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{
		Docs: []*types.Document{updated},
		FieldMutations: [][]backends.FieldMutation{
			{{Key: "touched", Value: int64(42)}},
		},
	})
	if err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	res, err := coll.Query(ctx, &backends.QueryParams{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	iter := res.Iter
	defer iter.Close()

	_, got, err := iter.Next()
	if err != nil {
		t.Fatalf("iter.Next: %v", err)
	}

	if v, _ := got.Get("touched"); v != int64(42) {
		t.Errorf("touched = %v, want 42", v)
	}
	if v, _ := got.Get("a"); v != int64(1) {
		t.Errorf("a = %v, want 1", v)
	}
}

// TestUpdateAll_FieldMutations_NilFallsBack verifies that UpdateAll without
// FieldMutations still performs the full-rewrite path and yields the correct
// document.
func TestUpdateAll_FieldMutations_NilFallsBack(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	orig := mustDoc(t, "_id", int64(1), "a", int64(1))
	insertDoc(t, b, "testdb", "c", orig)

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("c")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	updated := mustDoc(t, "_id", int64(1), "a", int64(2))

	// No FieldMutations supplied — backend must fall back to writeDocJSON.
	_, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{
		Docs: []*types.Document{updated},
	})
	if err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	res, err := coll.Query(ctx, &backends.QueryParams{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	iter := res.Iter
	defer iter.Close()

	_, got, err := iter.Next()
	if err != nil {
		t.Fatalf("iter.Next: %v", err)
	}

	if v, _ := got.Get("a"); v != int64(2) {
		t.Errorf("a = %v, want 2", v)
	}
}
