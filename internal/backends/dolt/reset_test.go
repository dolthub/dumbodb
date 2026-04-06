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

	"github.com/dolthub/docudolt/internal/backends"
)

// countDocs returns the number of documents in a collection.
func countDocs(t *testing.T, b *Backend, dbName, collName string) int {
	t.Helper()

	ctx := context.Background()

	db, err := b.Database(dbName)
	if err != nil {
		t.Fatalf("countDocs: Database(%q): %v", dbName, err)
	}

	coll, err := db.Collection(collName)
	if err != nil {
		t.Fatalf("countDocs: Collection(%q): %v", collName, err)
	}

	res, err := coll.Query(ctx, &backends.QueryParams{})
	if err != nil {
		t.Fatalf("countDocs: Query: %v", err)
	}

	n := 0

	for {
		_, _, iterErr := res.Iter.Next()
		if iterErr != nil {
			break
		}

		n++
	}

	return n
}

// logHashes returns all commit hashes from DocudoltLog (most recent first).
func logHashes(t *testing.T, b *Backend, dbName string) []string {
	t.Helper()

	res, err := b.DocudoltLog(context.Background(), &backends.LogParams{DBName: dbName})
	if err != nil {
		t.Fatalf("logHashes: DocudoltLog: %v", err)
	}

	hashes := make([]string, len(res.Commits))
	for i, c := range res.Commits {
		hashes[i] = c.CommitID
	}

	return hashes
}

// contains returns true if slice contains elem.
func contains(slice []string, elem string) bool {
	for _, s := range slice {
		if s == elem {
			return true
		}
	}

	return false
}

// ── Soft reset tests ──────────────────────────────────────────────────────────

// TestDocudoltReset_Soft_HeadMovesToTarget verifies that after a soft reset, HEAD is
// the target commit and the working tree still contains both docs.
func TestDocudoltReset_Soft_HeadMovesToTarget(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Insert doc1 and commit.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "first commit")

	// Insert doc2 (working set only, not committed).
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))

	// Soft reset to hash1.
	res, err := b.DocudoltReset(ctx, &backends.ResetParams{
		DBName: "testdb",
		CommitID: hash1,
		Hard:   false,
	})
	if err != nil {
		t.Fatalf("DocudoltReset: %v", err)
	}

	if res.CommitID != hash1 {
		t.Errorf("DocudoltReset returned hash %q, want %q", res.CommitID, hash1)
	}

	// After soft reset: working tree should still have both docs.
	n := countDocs(t, b, "testdb", "col")
	if n != 2 {
		t.Errorf("after soft reset: working tree has %d docs, want 2", n)
	}
}

// TestDocudoltReset_Soft_LogShowsTargetAsHead verifies that DocudoltLog after soft reset
// reports the target commit as the HEAD.
func TestDocudoltReset_Soft_LogShowsTargetAsHead(t *testing.T) {
	b := newTestBackend(t)

	// Build two commits.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "first")

	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))
	hash2 := commitDB(t, b, "testdb", "second")

	// Soft reset to hash1.
	_, err := b.DocudoltReset(context.Background(), &backends.ResetParams{
		DBName: "testdb",
		CommitID: hash1,
		Hard:   false,
	})
	if err != nil {
		t.Fatalf("DocudoltReset: %v", err)
	}

	hashes := logHashes(t, b, "testdb")

	// hash2 should not appear in log (HEAD is now hash1).
	if contains(hashes, hash2) {
		t.Errorf("soft reset: hash2 still appears in log; log = %v", hashes)
	}

	// hash1 should be the first (HEAD) entry.
	if len(hashes) == 0 || hashes[0] != hash1 {
		t.Errorf("soft reset: HEAD log entry = %v, want %q first", hashes, hash1)
	}
}

// TestDocudoltReset_Soft_DiffShowsUncommittedChange verifies that after soft reset the
// working-set doc is visible as uncommitted (i.e. DocudoltDiff shows it as added).
func TestDocudoltReset_Soft_DiffShowsUncommittedChange(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Insert doc1 and commit.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "first")

	// Insert doc2 into working set (not committed).
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))

	// Soft reset to hash1.
	_, err := b.DocudoltReset(ctx, &backends.ResetParams{
		DBName: "testdb",
		CommitID: hash1,
		Hard:   false,
	})
	if err != nil {
		t.Fatalf("DocudoltReset: %v", err)
	}

	// DocudoltDiff (HEAD=hash1 vs working set) should show doc2 as added.
	diffRes, err := b.DocudoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocudoltDiff after soft reset: %v", err)
	}

	if len(diffRes.Collections) != 1 {
		t.Fatalf("expected 1 changed collection after soft reset, got %d", len(diffRes.Collections))
	}

	cd := diffRes.Collections[0]

	if len(cd.Added) != 1 {
		t.Errorf("expected 1 added doc (doc2) in diff, got %d", len(cd.Added))
	}

	if len(cd.Removed) != 0 || len(cd.Modified) != 0 {
		t.Errorf("expected no removed/modified docs, got removed=%d modified=%d", len(cd.Removed), len(cd.Modified))
	}
}

// ── Hard reset tests ──────────────────────────────────────────────────────────

// TestDocudoltReset_Hard_WorkingTreeMatchesTarget verifies that after a hard reset,
// the working tree contains only the documents present at the target commit.
func TestDocudoltReset_Hard_WorkingTreeMatchesTarget(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Insert doc1 and commit.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "first commit")

	// Insert doc2 into working set (not yet committed).
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))

	// Hard reset to hash1.
	res, err := b.DocudoltReset(ctx, &backends.ResetParams{
		DBName: "testdb",
		CommitID: hash1,
		Hard:   true,
	})
	if err != nil {
		t.Fatalf("DocudoltReset(hard): %v", err)
	}

	if res.CommitID != hash1 {
		t.Errorf("DocudoltReset returned hash %q, want %q", res.CommitID, hash1)
	}

	// After hard reset: working tree should have only doc1.
	n := countDocs(t, b, "testdb", "col")
	if n != 1 {
		t.Errorf("after hard reset: working tree has %d docs, want 1", n)
	}
}

// TestDocudoltReset_Hard_LogShowsTargetAsHead verifies that DocudoltLog after a hard reset
// shows the target commit as HEAD.
func TestDocudoltReset_Hard_LogShowsTargetAsHead(t *testing.T) {
	b := newTestBackend(t)

	// Two commits.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "first")

	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))
	hash2 := commitDB(t, b, "testdb", "second")

	// Hard reset to hash1.
	_, err := b.DocudoltReset(context.Background(), &backends.ResetParams{
		DBName: "testdb",
		CommitID: hash1,
		Hard:   true,
	})
	if err != nil {
		t.Fatalf("DocudoltReset(hard): %v", err)
	}

	hashes := logHashes(t, b, "testdb")

	if contains(hashes, hash2) {
		t.Errorf("hard reset: hash2 still appears in log; log = %v", hashes)
	}

	if len(hashes) == 0 || hashes[0] != hash1 {
		t.Errorf("hard reset: HEAD log entry = %v, want %q first", hashes, hash1)
	}
}

// TestDocudoltReset_Hard_DiffIsClean verifies that after a hard reset, the working
// tree matches HEAD exactly (DocudoltDiff returns no changes).
func TestDocudoltReset_Hard_DiffIsClean(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Insert doc1 and commit.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "first")

	// Insert doc2 into working set.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))

	// Hard reset to hash1.
	_, err := b.DocudoltReset(ctx, &backends.ResetParams{
		DBName: "testdb",
		CommitID: hash1,
		Hard:   true,
	})
	if err != nil {
		t.Fatalf("DocudoltReset(hard): %v", err)
	}

	// DocudoltDiff should show no changes.
	diffRes, err := b.DocudoltDiff(ctx, &backends.DiffParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DocudoltDiff after hard reset: %v", err)
	}

	if len(diffRes.Collections) != 0 {
		t.Errorf("expected clean working tree after hard reset, got %d changed collections", len(diffRes.Collections))
	}
}

// ── Error case tests ──────────────────────────────────────────────────────────

// TestDocudoltReset_InvalidHash verifies that resetting to a nonexistent hash returns an error.
func TestDocudoltReset_InvalidHash(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Initialize the DB.
	_, err := b.getOrOpenDB(ctx, "testdb", true)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	_, err = b.DocudoltReset(ctx, &backends.ResetParams{
		DBName: "testdb",
		CommitID: "notavalidhash",
	})
	if err == nil {
		t.Error("expected error for invalid hash, got nil")
	}
}

// TestDocudoltReset_UnknownButValidHash verifies that a well-formed but unknown hash returns an error.
func TestDocudoltReset_UnknownButValidHash(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Initialize the DB.
	_, err := b.getOrOpenDB(ctx, "testdb", true)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	// A hash that is syntactically valid (32 hex chars) but not present in the store.
	unknownHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	_, err = b.DocudoltReset(ctx, &backends.ResetParams{
		DBName: "testdb",
		CommitID: unknownHash,
	})
	if err == nil {
		t.Error("expected error for unknown commit hash, got nil")
	}
}
