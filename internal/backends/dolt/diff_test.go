// Copyright 2024 Dolt Inc.
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
	"log/slog"
	"os"
	"testing"

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/types"
)

// newTestBackend creates a temporary Backend for tests and registers a cleanup.
func newTestBackend(t *testing.T) *Backend {
	t.Helper()

	dir, err := os.MkdirTemp("", "dongo-diff-test-*")
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

	res, err := b.DongoCommit(context.Background(), &backends.CommitParams{
		DBName:  dbName,
		Message: message,
	})
	if err != nil {
		t.Fatalf("DongoCommit(%q, %q): %v", dbName, message, err)
	}

	return res.Hash
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

// ── Field-level diff unit tests ────────────────────────────────────────────────

// TestDiffDocumentFields_NoChanges verifies that two identical docs return nil, nil.
func TestDiffDocumentFields_NoChanges(t *testing.T) {
	docA := mustDoc(t, "_id", int64(1), "x", int64(42), "y", "hello")
	docB := mustDoc(t, "_id", int64(1), "x", int64(42), "y", "hello")

	aDiff, bDiff, err := diffDocumentFields(docA, docB)
	if err != nil {
		t.Fatalf("diffDocumentFields: %v", err)
	}

	if aDiff != nil || bDiff != nil {
		t.Errorf("expected nil diffs for identical docs, got aDiff=%v bDiff=%v", aDiff, bDiff)
	}
}

// TestDiffDocumentFields_AddField verifies that a field added in b is absent from a and present in b.
func TestDiffDocumentFields_AddField(t *testing.T) {
	docA := mustDoc(t, "_id", int64(1), "x", int64(1))
	docB := mustDoc(t, "_id", int64(1), "x", int64(1), "y", int64(2))

	aDiff, bDiff, err := diffDocumentFields(docA, docB)
	if err != nil {
		t.Fatalf("diffDocumentFields: %v", err)
	}

	if aDiff == nil || bDiff == nil {
		t.Fatalf("expected non-nil diffs, got aDiff=%v bDiff=%v", aDiff, bDiff)
	}

	// y must be absent from aDiff
	if aDiff.Has("y") {
		t.Errorf("added field 'y' should be absent from aDiff")
	}

	// y must be present in bDiff with value 2
	yVal, err := bDiff.Get("y")
	if err != nil {
		t.Fatalf("bDiff.Get(y): %v", err)
	}

	if yVal != int64(2) {
		t.Errorf("bDiff['y'] = %v, want 2", yVal)
	}
}

// TestDiffDocumentFields_RemoveField verifies that a field removed from b is present in a and absent from b.
func TestDiffDocumentFields_RemoveField(t *testing.T) {
	docA := mustDoc(t, "_id", int64(1), "x", int64(1), "y", int64(99))
	docB := mustDoc(t, "_id", int64(1), "x", int64(1))

	aDiff, bDiff, err := diffDocumentFields(docA, docB)
	if err != nil {
		t.Fatalf("diffDocumentFields: %v", err)
	}

	if aDiff == nil || bDiff == nil {
		t.Fatalf("expected non-nil diffs, got aDiff=%v bDiff=%v", aDiff, bDiff)
	}

	// y must be present in aDiff with old value
	yVal, err := aDiff.Get("y")
	if err != nil {
		t.Fatalf("aDiff.Get(y): %v", err)
	}

	if yVal != int64(99) {
		t.Errorf("aDiff['y'] = %v, want 99", yVal)
	}

	// y must be absent from bDiff
	if bDiff.Has("y") {
		t.Errorf("removed field 'y' should be absent from bDiff")
	}
}

// TestDiffDocumentFields_ChangeField verifies that only the changed field appears in the diff.
func TestDiffDocumentFields_ChangeField(t *testing.T) {
	docA := mustDoc(t, "_id", int64(1), "x", int64(10), "y", "unchanged")
	docB := mustDoc(t, "_id", int64(1), "x", int64(20), "y", "unchanged")

	aDiff, bDiff, err := diffDocumentFields(docA, docB)
	if err != nil {
		t.Fatalf("diffDocumentFields: %v", err)
	}

	if aDiff == nil || bDiff == nil {
		t.Fatalf("expected non-nil diffs, got aDiff=%v bDiff=%v", aDiff, bDiff)
	}

	// Only x should differ; y must be absent from both diffs.
	if aDiff.Has("y") || bDiff.Has("y") {
		t.Errorf("unchanged field 'y' should not appear in diffs")
	}

	aX, err := aDiff.Get("x")
	if err != nil {
		t.Fatalf("aDiff.Get(x): %v", err)
	}

	bX, err := bDiff.Get("x")
	if err != nil {
		t.Fatalf("bDiff.Get(x): %v", err)
	}

	if aX != int64(10) {
		t.Errorf("aDiff['x'] = %v, want 10", aX)
	}

	if bX != int64(20) {
		t.Errorf("bDiff['x'] = %v, want 20", bX)
	}
}

// TestDiffDocumentFields_NestedDocChange verifies that only the changed nested key appears.
func TestDiffDocumentFields_NestedDocChange(t *testing.T) {
	innerA := mustDoc(t, "p", int64(1), "q", int64(2))
	innerB := mustDoc(t, "p", int64(1), "q", int64(99))

	docA := mustDoc(t, "_id", int64(1), "nested", innerA, "top", "same")
	docB := mustDoc(t, "_id", int64(1), "nested", innerB, "top", "same")

	aDiff, bDiff, err := diffDocumentFields(docA, docB)
	if err != nil {
		t.Fatalf("diffDocumentFields: %v", err)
	}

	if aDiff == nil || bDiff == nil {
		t.Fatalf("expected non-nil diffs, got aDiff=%v bDiff=%v", aDiff, bDiff)
	}

	// top must be absent (unchanged).
	if aDiff.Has("top") || bDiff.Has("top") {
		t.Errorf("unchanged field 'top' should not appear in diffs")
	}

	// nested must appear in both diffs.
	aNested, err := aDiff.Get("nested")
	if err != nil {
		t.Fatalf("aDiff.Get(nested): %v", err)
	}

	bNested, err := bDiff.Get("nested")
	if err != nil {
		t.Fatalf("bDiff.Get(nested): %v", err)
	}

	// The nested diffs should contain only the changed sub-field q.
	aNestedDoc, ok := aNested.(*types.Document)
	if !ok {
		t.Fatalf("aDiff['nested'] is %T, want *types.Document", aNested)
	}

	bNestedDoc, ok := bNested.(*types.Document)
	if !ok {
		t.Fatalf("bDiff['nested'] is %T, want *types.Document", bNested)
	}

	if aNestedDoc.Has("p") || bNestedDoc.Has("p") {
		t.Errorf("unchanged nested field 'p' should not appear in nested diffs")
	}

	aQ, err := aNestedDoc.Get("q")
	if err != nil {
		t.Fatalf("aDiff['nested']['q']: %v", err)
	}

	bQ, err := bNestedDoc.Get("q")
	if err != nil {
		t.Fatalf("bDiff['nested']['q']: %v", err)
	}

	if aQ != int64(2) {
		t.Errorf("aDiff['nested']['q'] = %v, want 2", aQ)
	}

	if bQ != int64(99) {
		t.Errorf("bDiff['nested']['q'] = %v, want 99", bQ)
	}
}

// ── Backend DongoDiff tests ────────────────────────────────────────────────────

// TestDongoDiff_NoChanges verifies that diffing HEAD vs working set with no writes
// returns empty collections.
func TestDongoDiff_NoChanges(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Insert and commit a doc so the DB is initialized.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	commitDB(t, b, "testdb", "init")

	// No writes after commit — diff should be empty.
	res, err := b.DongoDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DongoDiff: %v", err)
	}

	if len(res.Collections) != 0 {
		t.Errorf("expected 0 changed collections after no writes, got %d", len(res.Collections))
	}
}

// TestDongoDiff_InsertShowsAdded verifies that inserting a doc then diffing shows it as added.
func TestDongoDiff_InsertShowsAdded(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Initialize the DB by inserting and committing a sentinel doc in a different collection.
	insertDoc(t, b, "testdb", "sentinel", mustDoc(t, "_id", int64(0)))
	commitDB(t, b, "testdb", "baseline")

	// Insert a doc into "users" (working set only — not committed yet).
	doc := mustDoc(t, "_id", int64(1), "name", "alice")
	insertDoc(t, b, "testdb", "users", doc)

	// Diff HEAD (no "users") vs working set (has the doc).
	res, err := b.DongoDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DongoDiff: %v", err)
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

// TestDongoDiff_DeleteShowsRemoved verifies that deleting a doc then diffing shows it as removed.
func TestDongoDiff_DeleteShowsRemoved(t *testing.T) {
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
	res, err := b.DongoDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DongoDiff: %v", err)
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

// TestDongoDiff_UpdateShowsModified verifies that updating a doc then diffing shows only
// the changed fields in a/b.
func TestDongoDiff_UpdateShowsModified(t *testing.T) {
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

	res, err := b.DongoDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DongoDiff: %v", err)
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
	if m.A.Has("y") || m.B.Has("y") {
		t.Errorf("unchanged field 'y' should not appear in modified diff")
	}

	aX, err := m.A.Get("x")
	if err != nil {
		t.Fatalf("m.A.Get(x): %v", err)
	}

	bX, err := m.B.Get("x")
	if err != nil {
		t.Fatalf("m.B.Get(x): %v", err)
	}

	if aX != int64(10) {
		t.Errorf("a['x'] = %v, want 10", aX)
	}

	if bX != int64(99) {
		t.Errorf("b['x'] = %v, want 99", bX)
	}
}

// TestDongoDiff_MixedOps verifies a mix of insert, update, and delete across two collections.
func TestDongoDiff_MixedOps(t *testing.T) {
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

	res, err := b.DongoDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DongoDiff: %v", err)
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

// TestDongoDiff_BetweenTwoCommits verifies diffing between two specific commit hashes.
func TestDongoDiff_BetweenTwoCommits(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Commit 1: insert doc1.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "commit one")

	// Commit 2: insert doc2.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))
	hash2 := commitDB(t, b, "testdb", "commit two")

	// Diff from commit1 to commit2: should see doc2 as added.
	res, err := b.DongoDiff(ctx, &backends.DiffParams{
		DBName: "testdb",
		From:   hash1,
		To:     hash2,
	})
	if err != nil {
		t.Fatalf("DongoDiff(from=%s, to=%s): %v", hash1, hash2, err)
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

// TestDongoDiff_InvalidHashReturnsError verifies that an invalid commit hash returns an error.
func TestDongoDiff_InvalidHashReturnsError(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	_, err := b.getOrOpenDB(ctx, "testdb", true)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	_, err = b.DongoDiff(ctx, &backends.DiffParams{
		DBName: "testdb",
		From:   "notavalidhash",
	})
	if err == nil {
		t.Error("expected error for invalid hash, got nil")
	}
}

// TestDongoDiff_CommitThenInsertShowsAdded verifies the integration test from the spec:
// dongoCommit then insert then dongoDiff shows the insert as added.
func TestDongoDiff_CommitThenInsertThenDiff(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Initialize DB with a first doc and commit.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	commitDB(t, b, "testdb", "baseline")

	// Insert a second document (working set only).
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(42), "msg", "hello"))

	// DongoDiff (HEAD vs working set) should show the insert.
	res, err := b.DongoDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DongoDiff: %v", err)
	}

	if len(res.Collections) != 1 {
		t.Fatalf("expected 1 changed collection, got %d", len(res.Collections))
	}

	if len(res.Collections[0].Added) != 1 {
		t.Fatalf("expected 1 added doc, got %d", len(res.Collections[0].Added))
	}
}

// TestDongoDiff_TwoCommitsDeltaCorrect verifies that diffing from commit1 to commit2 shows
// only the delta introduced by commit2.
func TestDongoDiff_TwoCommitsDeltaCorrect(t *testing.T) {
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

	res, err := b.DongoDiff(ctx, &backends.DiffParams{
		DBName: "testdb",
		From:   hash1,
		To:     hash2,
	})
	if err != nil {
		t.Fatalf("DongoDiff: %v", err)
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

	// Field v: old=1, new=2.
	m := cd.Modified[0]

	aV, err := m.A.Get("v")
	if err != nil {
		t.Fatalf("m.A.Get(v): %v", err)
	}

	bV, err := m.B.Get("v")
	if err != nil {
		t.Fatalf("m.B.Get(v): %v", err)
	}

	if aV != int64(1) {
		t.Errorf("a['v'] = %v, want 1", aV)
	}

	if bV != int64(2) {
		t.Errorf("b['v'] = %v, want 2", bV)
	}
}
