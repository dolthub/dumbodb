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
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/dolthub/docudolt/internal/backends"
	"github.com/dolthub/docudolt/internal/types"
)

// newTestBackend creates a temporary Backend for tests and registers a cleanup.
func newTestBackend(t *testing.T) *Backend {
	t.Helper()

	dir, err := os.MkdirTemp("", "dolt-diff-test-*")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { os.RemoveAll(dir) })

	b := &Backend{
		dataDir: dir,
		l:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		dbs:     make(map[string]*dbState),
	}

	t.Cleanup(b.Close)

	return b
}

// insertDoc is a test helper that inserts a single document into a named collection.
func insertDoc(t *testing.T, b *Backend, dbName, collName string, doc *types.Document) {
	t.Helper()

	db, err := b.Database(dbName)
	if err != nil {
		t.Fatalf("Database(%q): %v", dbName, err)
	}

	coll, err := db.Collection(collName)
	if err != nil {
		t.Fatalf("Collection(%q): %v", collName, err)
	}

	if _, err = coll.InsertAll(context.Background(), &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}
}

// commitDB is a test helper that commits the current working set.
func commitDB(t *testing.T, b *Backend, dbName, message string) string {
	t.Helper()

	res, err := b.DocuDoltCommit(context.Background(), &backends.CommitParams{
		DBName:  dbName,
		Message: message,
		Author:  "testuser",
	})
	if err != nil {
		t.Fatalf("DocuDoltCommit(%q, %q): %v", dbName, message, err)
	}

	return res.CommitID
}

// mustDoc is a test helper that creates a document and fatals on error.
func mustDoc(t *testing.T, pairs ...any) *types.Document {
	t.Helper()

	doc, err := types.NewDocument(pairs...)
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}

	return doc
}

// findFieldDiff searches m.Diff for an entry with the given path, fataling if not found.
func findFieldDiff(t *testing.T, m backends.ModifiedDoc, path string) backends.FieldDiff {
	t.Helper()

	for _, fd := range m.Diff {
		if fd.Path == path {
			return fd
		}
	}

	paths := make([]string, len(m.Diff))
	for i, fd := range m.Diff {
		paths[i] = fd.Path
	}

	t.Fatalf("no FieldDiff found for path %q; got paths: %v", path, paths)

	return backends.FieldDiff{}
}

// ── Backend DocuDoltDiff tests ────────────────────────────────────────────────────

// TestDocuDoltDiff_NoChanges verifies that diffing HEAD vs working set with no writes
// returns empty collections.
func TestDocuDoltDiff_NoChanges(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Insert and commit a doc so the DB is initialized.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	commitDB(t, b, "testdb", "init")

	// No writes after commit — diff should be empty.
	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	if len(res.Collections) != 0 {
		t.Errorf("expected 0 changed collections after no writes, got %d", len(res.Collections))
	}
}

// TestDocuDoltDiff_InsertShowsAdded verifies that inserting a doc then diffing shows it as added.
func TestDocuDoltDiff_InsertShowsAdded(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Initialize the DB by inserting and committing a sentinel doc in a different collection.
	insertDoc(t, b, "testdb", "sentinel", mustDoc(t, "_id", int64(0)))
	commitDB(t, b, "testdb", "baseline")

	// Insert a doc into "users" (working set only — not committed yet).
	doc := mustDoc(t, "_id", int64(1), "name", "alice")
	insertDoc(t, b, "testdb", "users", doc)

	// Diff HEAD (no "users") vs working set (has the doc).
	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	// Find the "users" collection in the result.
	var cd *backends.CollectionDiff

	for i := range res.Collections {
		if res.Collections[i].Name == "users" {
			cd = &res.Collections[i]
			break
		}
	}

	if cd == nil {
		names := make([]string, len(res.Collections))
		for i, c := range res.Collections {
			names[i] = c.Name
		}

		t.Fatalf("expected diff for 'users' collection; got: %v", names)
	}

	if len(cd.Added) != 1 {
		t.Fatalf("expected 1 added doc, got %d", len(cd.Added))
	}

	if len(cd.Removed) != 0 {
		t.Errorf("expected 0 removed docs, got %d", len(cd.Removed))
	}

	if len(cd.Modified) != 0 {
		t.Errorf("expected 0 modified docs, got %d", len(cd.Modified))
	}
}

// TestDocuDoltDiff_DeleteShowsRemoved verifies that deleting a doc then diffing shows it as removed.
func TestDocuDoltDiff_DeleteShowsRemoved(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Insert and commit a doc.
	doc := mustDoc(t, "_id", int64(1), "name", "bob")
	insertDoc(t, b, "testdb", "users", doc)
	commitDB(t, b, "testdb", "add bob")

	// Delete the doc via UpdateAll — actually we need DeleteAll. Let's use the backend directly.
	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("users")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	if _, err = coll.DeleteAll(ctx, &backends.DeleteAllParams{IDs: []any{int64(1)}}); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}

	// Diff HEAD (has bob) vs working set (empty).
	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	if len(res.Collections) != 1 {
		t.Fatalf("expected 1 changed collection, got %d", len(res.Collections))
	}

	cd := res.Collections[0]

	if len(cd.Removed) != 1 {
		t.Fatalf("expected 1 removed doc, got %d", len(cd.Removed))
	}

	if len(cd.Added) != 0 {
		t.Errorf("expected 0 added docs, got %d", len(cd.Added))
	}

	if len(cd.Modified) != 0 {
		t.Errorf("expected 0 modified docs, got %d", len(cd.Modified))
	}
}

// TestDocuDoltDiff_UpdateShowsModified verifies that updating a doc then diffing shows only
// the changed fields in the path-based diff array.
func TestDocuDoltDiff_UpdateShowsModified(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Insert and commit a doc.
	original := mustDoc(t, "_id", int64(1), "x", int64(10), "y", "same")
	insertDoc(t, b, "testdb", "items", original)
	commitDB(t, b, "testdb", "original")

	// Update the doc (change x, keep y).
	updated := mustDoc(t, "_id", int64(1), "x", int64(99), "y", "same")

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("items")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{updated}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	if len(res.Collections) != 1 {
		t.Fatalf("expected 1 changed collection, got %d", len(res.Collections))
	}

	cd := res.Collections[0]

	if len(cd.Modified) != 1 {
		t.Fatalf("expected 1 modified doc, got %d", len(cd.Modified))
	}

	if len(cd.Added) != 0 || len(cd.Removed) != 0 {
		t.Errorf("expected 0 added/removed, got added=%d removed=%d", len(cd.Added), len(cd.Removed))
	}

	m := cd.Modified[0]

	// Only x should appear in the diff; y was unchanged.
	for _, fd := range m.Diff {
		if fd.Path == "$.y" {
			t.Errorf("unchanged field '$.y' should not appear in modified diff")
		}
	}

	// Verify the x diff entry.
	xDiff := findFieldDiff(t, m, "$.x")

	if xDiff.Type != "modified" {
		t.Errorf("$.x type = %q, want %q", xDiff.Type, "modified")
	}

	if xDiff.From != int64(10) {
		t.Errorf("$.x a = %v, want 10", xDiff.From)
	}

	if xDiff.To != int64(99) {
		t.Errorf("$.x b = %v, want 99", xDiff.To)
	}
}

// TestDocuDoltDiff_MixedOps verifies a mix of insert, update, and delete across two collections.
func TestDocuDoltDiff_MixedOps(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Collection "a": doc1 and doc2.
	insertDoc(t, b, "testdb", "a", mustDoc(t, "_id", int64(1), "v", int64(1)))
	insertDoc(t, b, "testdb", "a", mustDoc(t, "_id", int64(2), "v", int64(2)))
	// Collection "b": doc3.
	insertDoc(t, b, "testdb", "b", mustDoc(t, "_id", int64(3), "v", int64(3)))

	commitDB(t, b, "testdb", "baseline")

	// In working set: delete doc1 from "a", update doc2 in "a", add doc4 to "b".
	dbH, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	collA, err := dbH.Collection("a")
	if err != nil {
		t.Fatalf("Collection(a): %v", err)
	}

	if _, err = collA.DeleteAll(ctx, &backends.DeleteAllParams{IDs: []any{int64(1)}}); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}

	if _, err = collA.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(2), "v", int64(99)),
	}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	collB, err := dbH.Collection("b")
	if err != nil {
		t.Fatalf("Collection(b): %v", err)
	}

	if _, err = collB.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(4), "v", int64(4)),
	}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	// Should see changes in both collections.
	if len(res.Collections) != 2 {
		t.Fatalf("expected 2 changed collections, got %d", len(res.Collections))
	}

	// Find collection "a" and "b" diffs.
	collDiffs := make(map[string]backends.CollectionDiff, 2)
	for _, cd := range res.Collections {
		collDiffs[cd.Name] = cd
	}

	cdA, ok := collDiffs["a"]
	if !ok {
		t.Fatal("expected diff for collection 'a'")
	}

	if len(cdA.Removed) != 1 {
		t.Errorf("collection 'a': expected 1 removed, got %d", len(cdA.Removed))
	}

	if len(cdA.Modified) != 1 {
		t.Errorf("collection 'a': expected 1 modified, got %d", len(cdA.Modified))
	}

	cdB, ok := collDiffs["b"]
	if !ok {
		t.Fatal("expected diff for collection 'b'")
	}

	if len(cdB.Added) != 1 {
		t.Errorf("collection 'b': expected 1 added, got %d", len(cdB.Added))
	}
}

// TestDocuDoltDiff_BetweenTwoCommits verifies diffing between two specific commit hashes.
func TestDocuDoltDiff_BetweenTwoCommits(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Commit 1: insert doc1.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "commit one")

	// Commit 2: insert doc2.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))
	hash2 := commitDB(t, b, "testdb", "commit two")

	// Diff from commit1 to commit2: should see doc2 as added.
	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{
		DBName: "testdb",
		From:   hash1,
		To:     hash2,
	})
	if err != nil {
		t.Fatalf("DocuDoltDiff(from=%s, to=%s): %v", hash1, hash2, err)
	}

	if len(res.Collections) != 1 {
		t.Fatalf("expected 1 changed collection, got %d", len(res.Collections))
	}

	cd := res.Collections[0]

	if len(cd.Added) != 1 {
		t.Fatalf("expected 1 added doc, got %d", len(cd.Added))
	}

	if len(cd.Removed) != 0 || len(cd.Modified) != 0 {
		t.Errorf("expected 0 removed/modified, got removed=%d modified=%d", len(cd.Removed), len(cd.Modified))
	}
}

// TestDocuDoltDiff_InvalidHashReturnsError verifies that an invalid commit hash returns an error.
func TestDocuDoltDiff_InvalidHashReturnsError(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	_, err := b.getOrOpenDB(ctx, "testdb", true)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	_, err = b.DocuDoltDiff(ctx, &backends.DiffParams{
		DBName: "testdb",
		From:   "notavalidhash",
	})
	if err == nil {
		t.Error("expected error for invalid hash, got nil")
	}
}

// TestDocuDoltDiff_CommitThenInsertShowsAdded verifies the integration test from the spec:
// docuDoltCommit then insert then docuDoltDiff shows the insert as added.
func TestDocuDoltDiff_CommitThenInsertThenDiff(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Initialize DB with a first doc and commit.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	commitDB(t, b, "testdb", "baseline")

	// Insert a second document (working set only).
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(42), "msg", "hello"))

	// DocuDoltDiff (HEAD vs working set) should show the insert.
	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	if len(res.Collections) != 1 {
		t.Fatalf("expected 1 changed collection, got %d", len(res.Collections))
	}

	if len(res.Collections[0].Added) != 1 {
		t.Fatalf("expected 1 added doc, got %d", len(res.Collections[0].Added))
	}
}

// TestDocuDoltDiff_TwoCommitsDeltaCorrect verifies that diffing from commit1 to commit2 shows
// only the delta introduced by commit2.
func TestDocuDoltDiff_TwoCommitsDeltaCorrect(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Commit 1: one doc.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "first")

	// Commit 2: update that doc.
	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(1), "v", int64(2)),
	}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	hash2 := commitDB(t, b, "testdb", "second")

	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{
		DBName: "testdb",
		From:   hash1,
		To:     hash2,
	})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	if len(res.Collections) != 1 {
		t.Fatalf("expected 1 changed collection, got %d", len(res.Collections))
	}

	cd := res.Collections[0]

	if len(cd.Modified) != 1 {
		t.Fatalf("expected 1 modified doc, got %d", len(cd.Modified))
	}

	if len(cd.Added) != 0 || len(cd.Removed) != 0 {
		t.Errorf("expected 0 added/removed, got added=%d removed=%d", len(cd.Added), len(cd.Removed))
	}

	m := cd.Modified[0]

	// Field v: path=$.v, type=modified, a=1, b=2.
	vDiff := findFieldDiff(t, m, "$.v")

	if vDiff.Type != "modified" {
		t.Errorf("$.v type = %q, want %q", vDiff.Type, "modified")
	}

	if vDiff.From != int64(1) {
		t.Errorf("$.v a = %v, want 1", vDiff.From)
	}

	if vDiff.To != int64(2) {
		t.Errorf("$.v b = %v, want 2", vDiff.To)
	}
}

// TestDocuDoltDiff_AddFieldShowsAdded verifies that when a field is added to a document,
// the diff reports type="added" with path="$.fieldname" and only a "b" value.
func TestDocuDoltDiff_AddFieldShowsAdded(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Commit a doc without field "y".
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "x", int64(1)))
	commitDB(t, b, "testdb", "baseline")

	// Update to add field "y".
	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(1), "x", int64(1), "y", int64(2)),
	}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	if len(res.Collections) != 1 || len(res.Collections[0].Modified) != 1 {
		t.Fatalf("expected 1 modified doc")
	}

	m := res.Collections[0].Modified[0]

	// y must be present as "added" with b=2; a must be absent.
	yDiff := findFieldDiff(t, m, "$.y")

	if yDiff.Type != "added" {
		t.Errorf("$.y type = %q, want %q", yDiff.Type, "added")
	}

	if yDiff.From != nil {
		t.Errorf("$.y a should be nil for added, got %v", yDiff.From)
	}

	if yDiff.To != int64(2) {
		t.Errorf("$.y b = %v, want 2", yDiff.To)
	}

	// x must not appear (unchanged).
	for _, fd := range m.Diff {
		if fd.Path == "$.x" {
			t.Errorf("unchanged field '$.x' should not appear in diff")
		}
	}
}

// TestDocuDoltDiff_RemoveFieldShowsRemoved verifies that when a field is removed from a document,
// the diff reports type="removed" with path="$.fieldname" and only an "a" value.
func TestDocuDoltDiff_RemoveFieldShowsRemoved(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Commit a doc with field "y".
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "x", int64(1), "y", int64(99)))
	commitDB(t, b, "testdb", "baseline")

	// Update to remove field "y".
	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(1), "x", int64(1)),
	}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	if len(res.Collections) != 1 || len(res.Collections[0].Modified) != 1 {
		t.Fatalf("expected 1 modified doc")
	}

	m := res.Collections[0].Modified[0]

	// y must be present as "removed" with a=99; b must be absent.
	yDiff := findFieldDiff(t, m, "$.y")

	if yDiff.Type != "removed" {
		t.Errorf("$.y type = %q, want %q", yDiff.Type, "removed")
	}

	if yDiff.From != int64(99) {
		t.Errorf("$.y a = %v, want 99", yDiff.From)
	}

	if yDiff.To != nil {
		t.Errorf("$.y b should be nil for removed, got %v", yDiff.To)
	}
}

// TestDocuDoltDiff_NestedFieldChange verifies that changing a nested field reports
// the full JSON path (e.g. "$.address.city") rather than just the top-level key.
func TestDocuDoltDiff_NestedFieldChange(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Build a doc with a nested "address" sub-document.
	addrA := mustDoc(t, "city", "Seattle", "zip", "98101")
	addrB := mustDoc(t, "city", "Portland", "zip", "98101")

	insertDoc(t, b, "testdb", "col",
		mustDoc(t, "_id", int64(1), "address", addrA, "name", "alice"))
	commitDB(t, b, "testdb", "baseline")

	// Update: change address.city only.
	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(1), "address", addrB, "name", "alice"),
	}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	if len(res.Collections) != 1 || len(res.Collections[0].Modified) != 1 {
		t.Fatalf("expected 1 modified doc")
	}

	m := res.Collections[0].Modified[0]

	// $.address.city must be modified; $.address.zip and $.name must be absent.
	cityDiff := findFieldDiff(t, m, "$.address.city")

	if cityDiff.Type != "modified" {
		t.Errorf("$.address.city type = %q, want %q", cityDiff.Type, "modified")
	}

	if cityDiff.From != "Seattle" {
		t.Errorf("$.address.city a = %v, want Seattle", cityDiff.From)
	}

	if cityDiff.To != "Portland" {
		t.Errorf("$.address.city b = %v, want Portland", cityDiff.To)
	}

	// Unchanged fields must not appear.
	for _, fd := range m.Diff {
		if fd.Path == "$.address.zip" {
			t.Errorf("unchanged '$.address.zip' should not appear in diff")
		}

		if fd.Path == "$.name" {
			t.Errorf("unchanged '$.name' should not appear in diff")
		}
	}
}

// TestDocuDoltDiff_ArrayElementChange verifies that changing an array element reports
// the path with bracket notation (e.g. "$.scores[2]").
func TestDocuDoltDiff_ArrayElementChange(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Build docs with an array field. types.MakeArray requires appending.
	scoresA := types.MakeArray(3)
	scoresA.Append(int64(80))
	scoresA.Append(int64(85))
	scoresA.Append(int64(91))

	scoresB := types.MakeArray(3)
	scoresB.Append(int64(80))
	scoresB.Append(int64(85))
	scoresB.Append(int64(95)) // index 2 changed

	docA := mustDoc(t, "_id", int64(1), "scores", scoresA)
	docB := mustDoc(t, "_id", int64(1), "scores", scoresB)

	insertDoc(t, b, "testdb", "col", docA)
	commitDB(t, b, "testdb", "baseline")

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{docB}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	if len(res.Collections) != 1 || len(res.Collections[0].Modified) != 1 {
		t.Fatalf("expected 1 modified doc")
	}

	m := res.Collections[0].Modified[0]

	// Element at index 2 must be reported as modified.
	elemDiff := findFieldDiff(t, m, "$.scores[2]")

	if elemDiff.Type != "modified" {
		t.Errorf("$.scores[2] type = %q, want %q", elemDiff.Type, "modified")
	}

	if elemDiff.From != int64(91) {
		t.Errorf("$.scores[2] a = %v, want 91", elemDiff.From)
	}

	if elemDiff.To != int64(95) {
		t.Errorf("$.scores[2] b = %v, want 95", elemDiff.To)
	}
}

// TestDocuDoltDiff_NoChangesMeanNoDiff verifies that updating a doc with identical
// content produces no modified entries.
func TestDocuDoltDiff_NoChangesMeanNoDiff(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	original := mustDoc(t, "_id", int64(1), "x", int64(42), "y", "hello")
	insertDoc(t, b, "testdb", "col", original)
	commitDB(t, b, "testdb", "init")

	// "Update" with identical values.
	same := mustDoc(t, "_id", int64(1), "x", int64(42), "y", "hello")

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{same}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	if len(res.Collections) != 0 {
		t.Errorf("expected 0 changed collections for identical update, got %d", len(res.Collections))
	}
}

// TestDocuDoltDiff_MultipleDocsWithMixedChanges verifies that when multiple documents
// in the same collection have different change types, each is reported correctly and
// independently. Baseline has docs 1, 2, 3. Working set: doc1 deleted, doc2 field
// modified, doc3 unchanged, doc4 added.
func TestDocuDoltDiff_MultipleDocsWithMixedChanges(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "name", "alpha", "v", int64(1)))
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "name", "beta", "v", int64(2)))
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(3), "name", "gamma", "v", int64(3)))
	commitDB(t, b, "testdb", "baseline")

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	// Delete doc1.
	if _, err = coll.DeleteAll(ctx, &backends.DeleteAllParams{IDs: []any{int64(1)}}); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}

	// Modify doc2 (change v, leave name).
	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(2), "name", "beta", "v", int64(99)),
	}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	// doc3: no change.

	// Add doc4.
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(4), "name", "delta", "v", int64(4)),
	}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	if len(res.Collections) != 1 {
		t.Fatalf("expected 1 changed collection, got %d", len(res.Collections))
	}

	cd := res.Collections[0]

	// added: doc4 only.
	if len(cd.Added) != 1 {
		t.Fatalf("expected 1 added doc, got %d", len(cd.Added))
	}

	addedID, _ := cd.Added[0].Get("_id")
	if addedID != int64(4) {
		t.Errorf("added doc _id = %v, want 4", addedID)
	}

	// removed: doc1 only.
	if len(cd.Removed) != 1 {
		t.Fatalf("expected 1 removed doc, got %d", len(cd.Removed))
	}

	removedID, _ := cd.Removed[0].Get("_id")
	if removedID != int64(1) {
		t.Errorf("removed doc _id = %v, want 1", removedID)
	}

	// modified: doc2 only (doc3 unchanged).
	if len(cd.Modified) != 1 {
		t.Fatalf("expected 1 modified doc (doc2), got %d; doc3 must not appear (unchanged)", len(cd.Modified))
	}

	m := cd.Modified[0]
	if m.ID != int64(2) {
		t.Errorf("modified doc _id = %v, want 2", m.ID)
	}

	// Only $.v changed; $.name must not appear.
	vDiff := findFieldDiff(t, m, "$.v")
	if vDiff.Type != "modified" {
		t.Errorf("$.v type = %q, want %q", vDiff.Type, "modified")
	}
	if vDiff.From != int64(2) || vDiff.To != int64(99) {
		t.Errorf("$.v a=%v b=%v, want a=2 b=99", vDiff.From, vDiff.To)
	}

	for _, fd := range m.Diff {
		if fd.Path == "$.name" {
			t.Errorf("unchanged field '$.name' must not appear in diff")
		}
	}
}

// TestDocuDoltDiff_SingleDocMixedFieldOps verifies that a single document update that
// simultaneously modifies one field, adds a new field, and removes an existing field
// produces exactly three FieldDiff entries — one per operation.
func TestDocuDoltDiff_SingleDocMixedFieldOps(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Baseline: doc has fields x (will be modified), y (will be removed), _id.
	insertDoc(t, b, "testdb", "col",
		mustDoc(t, "_id", int64(1), "x", int64(10), "y", "remove-me"))
	commitDB(t, b, "testdb", "baseline")

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	// Update: x modified (10→99), y removed, z added.
	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(1), "x", int64(99), "z", "new-field"),
	}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	if len(res.Collections) != 1 || len(res.Collections[0].Modified) != 1 {
		t.Fatalf("expected 1 collection with 1 modified doc")
	}

	m := res.Collections[0].Modified[0]

	// $.x: modified, a=10, b=99.
	xDiff := findFieldDiff(t, m, "$.x")
	if xDiff.Type != "modified" {
		t.Errorf("$.x type = %q, want %q", xDiff.Type, "modified")
	}
	if xDiff.From != int64(10) || xDiff.To != int64(99) {
		t.Errorf("$.x a=%v b=%v, want a=10 b=99", xDiff.From, xDiff.To)
	}

	// $.y: removed, a="remove-me", b=nil.
	yDiff := findFieldDiff(t, m, "$.y")
	if yDiff.Type != "removed" {
		t.Errorf("$.y type = %q, want %q", yDiff.Type, "removed")
	}
	if yDiff.From != "remove-me" {
		t.Errorf("$.y a = %v, want 'remove-me'", yDiff.From)
	}
	if yDiff.To != nil {
		t.Errorf("$.y b should be nil for removed, got %v", yDiff.To)
	}

	// $.z: added, a=nil, b="new-field".
	zDiff := findFieldDiff(t, m, "$.z")
	if zDiff.Type != "added" {
		t.Errorf("$.z type = %q, want %q", zDiff.Type, "added")
	}
	if zDiff.From != nil {
		t.Errorf("$.z a should be nil for added, got %v", zDiff.From)
	}
	if zDiff.To != "new-field" {
		t.Errorf("$.z b = %v, want 'new-field'", zDiff.To)
	}

	// Exactly three diff entries (x modified, y removed, z added).
	if len(m.Diff) != 3 {
		paths := make([]string, len(m.Diff))
		for i, fd := range m.Diff {
			paths[i] = fmt.Sprintf("%s(%s)", fd.Path, fd.Type)
		}
		t.Errorf("expected exactly 3 field diffs, got %d: %v", len(m.Diff), paths)
	}
}

// TestDocuDoltDiff_FieldTypeChange verifies that changing a field's type (e.g. int64 → string)
// is reported as "modified" with the correct old and new values.
func TestDocuDoltDiff_FieldTypeChange(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Baseline: field "val" is an int64.
	insertDoc(t, b, "testdb", "col",
		mustDoc(t, "_id", int64(1), "val", int64(42), "stable", "unchanged"))
	commitDB(t, b, "testdb", "baseline")

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	// Update: val changes from int64(42) to string "forty-two".
	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(1), "val", "forty-two", "stable", "unchanged"),
	}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocuDoltDiff: %v", err)
	}

	if len(res.Collections) != 1 || len(res.Collections[0].Modified) != 1 {
		t.Fatalf("expected 1 collection with 1 modified doc")
	}

	m := res.Collections[0].Modified[0]

	// $.val: modified, a=int64(42) (old type), b="forty-two" (new type).
	valDiff := findFieldDiff(t, m, "$.val")
	if valDiff.Type != "modified" {
		t.Errorf("$.val type = %q, want %q", valDiff.Type, "modified")
	}
	if valDiff.From != int64(42) {
		t.Errorf("$.val a = %v (%T), want int64(42)", valDiff.From, valDiff.From)
	}
	if valDiff.To != "forty-two" {
		t.Errorf("$.val b = %v (%T), want string 'forty-two'", valDiff.To, valDiff.To)
	}

	// $.stable must not appear (unchanged).
	for _, fd := range m.Diff {
		if fd.Path == "$.stable" {
			t.Errorf("unchanged field '$.stable' must not appear in diff")
		}
	}
}

// ── Rootish expression tests ──────────────────────────────────────────────────

// TestDocuDoltDiff_HeadFromTo verifies that from="HEAD" and to="HEAD" resolve to the
// committed tip of the connection's branch (ConnRootish="main").
func TestDocuDoltDiff_HeadFromTo(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Commit 1: one doc.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "commit one")

	// Commit 2: update that doc.
	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(1), "v", int64(2)),
	}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	hash2 := commitDB(t, b, "testdb", "commit two")

	// from=hash1, to="HEAD" — HEAD resolves to main's committed tip (hash2).
	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{
		DBName:      "testdb",
		ConnRootish: "main",
		From:        hash1,
		To:          "HEAD",
	})
	if err != nil {
		t.Fatalf("DocuDoltDiff(from=%s, to=HEAD): %v", hash1, err)
	}

	if len(res.Collections) != 1 {
		t.Fatalf("expected 1 changed collection, got %d", len(res.Collections))
	}

	if len(res.Collections[0].Modified) != 1 {
		t.Fatalf("expected 1 modified doc, got %d", len(res.Collections[0].Modified))
	}

	// from="HEAD", to=hash2 — HEAD resolves to main's tip; result should be empty (same commit).
	res2, err := b.DocuDoltDiff(ctx, &backends.DiffParams{
		DBName:      "testdb",
		ConnRootish: "main",
		From:        "HEAD",
		To:          hash2,
	})
	if err != nil {
		t.Fatalf("DocuDoltDiff(from=HEAD, to=%s): %v", hash2, err)
	}

	if len(res2.Collections) != 0 {
		t.Errorf("HEAD and hash2 are the same commit; expected empty diff, got %d collections", len(res2.Collections))
	}
}

// TestDocuDoltDiff_HeadTilde verifies that HEAD~N ancestor expressions in from/to
// resolve correctly relative to ConnRootish.
func TestDocuDoltDiff_HeadTilde(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Three commits: doc v=1, then v=2, then v=3.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	commitDB(t, b, "testdb", "c1")

	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(1), "v", int64(2)),
	}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	commitDB(t, b, "testdb", "c2")

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(1), "v", int64(3)),
	}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	commitDB(t, b, "testdb", "c3")

	// HEAD~2 → c1, HEAD → c3: should see v=1→3.
	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{
		DBName:      "testdb",
		ConnRootish: "main",
		From:        "HEAD~2",
		To:          "HEAD",
	})
	if err != nil {
		t.Fatalf("DocuDoltDiff(HEAD~2, HEAD): %v", err)
	}

	if len(res.Collections) != 1 {
		t.Fatalf("expected 1 changed collection, got %d", len(res.Collections))
	}

	m := res.Collections[0].Modified[0]
	vDiff := findFieldDiff(t, m, "$.v")

	if vDiff.From != int64(1) {
		t.Errorf("expected from=1, got %v", vDiff.From)
	}

	if vDiff.To != int64(3) {
		t.Errorf("expected to=3, got %v", vDiff.To)
	}

	// HEAD~1 → c2, HEAD~0 → c3: should see v=2→3.
	res2, err := b.DocuDoltDiff(ctx, &backends.DiffParams{
		DBName:      "testdb",
		ConnRootish: "main",
		From:        "HEAD~1",
		To:          "HEAD~0",
	})
	if err != nil {
		t.Fatalf("DocuDoltDiff(HEAD~1, HEAD~0): %v", err)
	}

	if len(res2.Collections) != 1 {
		t.Fatalf("expected 1 changed collection, got %d", len(res2.Collections))
	}

	m2 := res2.Collections[0].Modified[0]
	vDiff2 := findFieldDiff(t, m2, "$.v")

	if vDiff2.From != int64(2) {
		t.Errorf("expected from=2, got %v", vDiff2.From)
	}

	if vDiff2.To != int64(3) {
		t.Errorf("expected to=3, got %v", vDiff2.To)
	}
}

// TestDocuDoltDiff_BranchNameRootish verifies that bare branch names and branch~N
// ancestor expressions work as from/to params.
func TestDocuDoltDiff_BranchNameRootish(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Commit 1 on main: doc v=1.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	commitDB(t, b, "testdb", "c1")

	// Create a feature branch from main.
	if _, err := b.DocuDoltBranch(ctx, &backends.BranchParams{
		DBName: "testdb",
		From:   "main",
		Name:   "feature",
	}); err != nil {
		t.Fatalf("DocuDoltBranch: %v", err)
	}

	// Commit 2 on main: doc v=2.
	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(1), "v", int64(2)),
	}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	commitDB(t, b, "testdb", "c2")

	// Diff from="feature" (c1) to="main" (c2): should see v=1→2.
	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{
		DBName:      "testdb",
		ConnRootish: "main",
		From:        "feature",
		To:          "main",
	})
	if err != nil {
		t.Fatalf("DocuDoltDiff(from=feature, to=main): %v", err)
	}

	if len(res.Collections) != 1 {
		t.Fatalf("expected 1 changed collection, got %d", len(res.Collections))
	}

	m := res.Collections[0].Modified[0]
	vDiff := findFieldDiff(t, m, "$.v")

	if vDiff.From != int64(1) {
		t.Errorf("expected from=1, got %v", vDiff.From)
	}

	if vDiff.To != int64(2) {
		t.Errorf("expected to=2, got %v", vDiff.To)
	}

	// Diff from="main~1" (c1) to="main" (c2): same result via ancestor expression.
	res2, err := b.DocuDoltDiff(ctx, &backends.DiffParams{
		DBName:      "testdb",
		ConnRootish: "main",
		From:        "main~1",
		To:          "main",
	})
	if err != nil {
		t.Fatalf("DocuDoltDiff(from=main~1, to=main): %v", err)
	}

	if len(res2.Collections) != 1 {
		t.Fatalf("expected 1 changed collection (main~1 vs main), got %d", len(res2.Collections))
	}

	m2 := res2.Collections[0].Modified[0]
	vDiff2 := findFieldDiff(t, m2, "$.v")

	if vDiff2.From != int64(1) {
		t.Errorf("expected from=1, got %v", vDiff2.From)
	}

	if vDiff2.To != int64(2) {
		t.Errorf("expected to=2, got %v", vDiff2.To)
	}
}

// TestDocuDoltDiff_HeadOnNonMainBranch verifies that HEAD resolves to the connection's
// own branch tip, not main, when ConnRootish is a non-main branch.
func TestDocuDoltDiff_HeadOnNonMainBranch(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Commit 1 on main: doc v=1.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "c1-main")

	// Create feature branch from c1.
	if _, err := b.DocuDoltBranch(ctx, &backends.BranchParams{
		DBName: "testdb",
		From:   "main",
		Name:   "feature",
	}); err != nil {
		t.Fatalf("DocuDoltBranch: %v", err)
	}

	// Commit 2 on main: doc v=2.
	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{
		mustDoc(t, "_id", int64(1), "v", int64(2)),
	}}); err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}

	commitDB(t, b, "testdb", "c2-main")

	// from=hash1, to="HEAD" with ConnRootish="feature":
	// HEAD should resolve to feature branch tip (c1), NOT main (c2).
	// So hash1 == feature HEAD → diff should be empty.
	res, err := b.DocuDoltDiff(ctx, &backends.DiffParams{
		DBName:      "testdb",
		ConnRootish: "feature",
		From:        hash1,
		To:          "HEAD",
	})
	if err != nil {
		t.Fatalf("DocuDoltDiff(from=hash1, to=HEAD, ConnRootish=feature): %v", err)
	}

	if len(res.Collections) != 0 {
		t.Errorf("HEAD on feature branch must resolve to feature tip (same as hash1); expected empty diff, got %d collections", len(res.Collections))
	}

	// Now verify HEAD on main IS different from hash1.
	res2, err := b.DocuDoltDiff(ctx, &backends.DiffParams{
		DBName:      "testdb",
		ConnRootish: "main",
		From:        hash1,
		To:          "HEAD",
	})
	if err != nil {
		t.Fatalf("DocuDoltDiff(from=hash1, to=HEAD, ConnRootish=main): %v", err)
	}

	if len(res2.Collections) != 1 {
		t.Fatalf("HEAD on main must resolve to c2 (different from hash1); expected 1 changed collection, got %d", len(res2.Collections))
	}
}
