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

	"github.com/dolthub/dolt/go/store/hash"

	"github.com/dolthub/dongo/internal/backends"
)

// TestRTVLLoad_CommitHash_Query verifies that connecting with a commit-hash rootish
// (mydb__<hash>) reads the collection as it existed at that commit, not the current
// working set.
func TestRTVLLoad_CommitHash_Query(t *testing.T) {
	b := newTestBackend(t)

	// Insert doc1 and commit → hash1 contains only doc1.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(10)))
	hash1 := commitDB(t, b, "testdb", "first commit")

	// Insert doc2 and commit → hash2 contains doc1 + doc2.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(20)))
	hash2 := commitDB(t, b, "testdb", "second commit")

	// Insert doc3 into working set (not committed).
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(3), "v", int64(30)))

	// Reading from hash1 (encoded: "testdb__<hash1>") should see only doc1.
	n1 := countDocs(t, b, "testdb__"+hash1, "col")
	if n1 != 1 {
		t.Errorf("mydb__%s: want 1 doc (only doc1), got %d", hash1, n1)
	}

	// Reading from hash2 should see doc1 + doc2.
	n2 := countDocs(t, b, "testdb__"+hash2, "col")
	if n2 != 2 {
		t.Errorf("mydb__%s: want 2 docs (doc1+doc2), got %d", hash2, n2)
	}

	// Reading from plain "testdb" (working set) should see all 3 docs.
	nWS := countDocs(t, b, "testdb", "col")
	if nWS != 3 {
		t.Errorf("testdb (working set): want 3 docs, got %d", nWS)
	}
}

// TestRTVLLoad_CommitHash_ListCollections verifies that ListCollections via a
// commit-hash rootish reflects the collection state at that commit.
func TestRTVLLoad_CommitHash_ListCollections(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Create "col1" and commit → hash1 has col1.
	insertDoc(t, b, "testdb", "col1", mustDoc(t, "_id", int64(1)))
	hash1 := commitDB(t, b, "testdb", "add col1")

	// Create "col2" and commit → hash2 has col1 + col2.
	insertDoc(t, b, "testdb", "col2", mustDoc(t, "_id", int64(1)))
	_ = commitDB(t, b, "testdb", "add col2")

	// ListCollections at hash1 should list only col1.
	db1, err := b.Database("testdb__" + hash1)
	if err != nil {
		t.Fatalf("Database(testdb__%s): %v", hash1, err)
	}

	res1, err := db1.ListCollections(ctx, nil)
	if err != nil {
		t.Fatalf("ListCollections at hash1: %v", err)
	}

	if len(res1.Collections) != 1 || res1.Collections[0].Name != "col1" {
		names := make([]string, len(res1.Collections))
		for i, c := range res1.Collections {
			names[i] = c.Name
		}
		t.Errorf("ListCollections at hash1: got %v, want [col1]", names)
	}

	// ListCollections at working set should list col1 + col2.
	db2, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database(testdb): %v", err)
	}

	res2, err := db2.ListCollections(ctx, nil)
	if err != nil {
		t.Fatalf("ListCollections at working set: %v", err)
	}

	if len(res2.Collections) != 2 {
		t.Errorf("ListCollections at working set: got %d collections, want 2", len(res2.Collections))
	}
}

// TestRTVLLoad_CommitHash_UnknownHash verifies that a well-formed but missing
// commit hash returns an error (not silent empty results).
func TestRTVLLoad_CommitHash_UnknownHash(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Initialize DB.
	_, err := b.getOrOpenDB(ctx, "testdb", true)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	// An unknown but syntactically valid Dolt hash (32 base32 chars).
	unknown := "00000000000000000000000000000000"

	// Query via commit hash should return an error.
	db, err := b.Database("testdb__" + unknown)
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	_, err = coll.Query(ctx, &backends.QueryParams{})
	if err == nil {
		t.Error("expected error for unknown commit hash, got nil")
	}
}

// TestRTVLLoad_CommitHash_MainBranchUnchanged verifies that the plain "main" rootish
// (no __suffix) continues to read from the current working set, not affected by the
// encoded-name changes.
func TestRTVLLoad_CommitHash_MainBranchUnchanged(t *testing.T) {
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, "testdb", "first")

	// Add a second doc to the working set (not committed).
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2)))

	// Reading from plain "testdb" (or "testdb__main") should see both docs.
	nPlain := countDocs(t, b, "testdb", "col")
	if nPlain != 2 {
		t.Errorf("plain testdb: want 2 docs (working set), got %d", nPlain)
	}

	nMain := countDocs(t, b, "testdb__main", "col")
	if nMain != 2 {
		t.Errorf("testdb__main: want 2 docs (working set), got %d", nMain)
	}
}

// TestSplitEncodedDBName verifies the name parsing logic.
func TestSplitEncodedDBName(t *testing.T) {
	cases := []struct {
		encoded      string
		wantBaseName string
		wantRootish  string
	}{
		{"mydb", "mydb", "main"},
		{"mydb__main", "mydb", "main"},
		{"mydb__na7kfra98h45fr2u5qtr30o2ggm7vh61", "mydb", "na7kfra98h45fr2u5qtr30o2ggm7vh61"},
		{"mydb__feature-branch", "mydb", "feature-branch"},
		{"mydb__main~3", "mydb", "main~3"},
		{"a__b__c", "a", "b__c"},
		// Percent-encoded rootish: dots and slashes in branch names must be encoded
		// because '.' and '/' are invalid in MongoDB database names.
		{"mydb__v1%2E0", "mydb", "v1.0"},
		{"mydb__feature%2Ffoo", "mydb", "feature/foo"},
		{"mydb__main%7E1", "mydb", "main~1"}, // ~1 encoded (passthrough — still ancestor expr)
		// Invalid percent sequence: falls back to raw value.
		{"mydb__%ZZ", "mydb", "%ZZ"},
	}

	for _, tc := range cases {
		base, rootish := splitEncodedDBName(tc.encoded)
		if base != tc.wantBaseName {
			t.Errorf("splitEncodedDBName(%q) base = %q, want %q", tc.encoded, base, tc.wantBaseName)
		}
		if rootish != tc.wantRootish {
			t.Errorf("splitEncodedDBName(%q) rootish = %q, want %q", tc.encoded, rootish, tc.wantRootish)
		}
	}
}

// TestRTVLLoad_AncestorExpr_Query verifies that mydb__main~N returns the correct
// historical document set for each ancestor depth.
//
// Setup: 3 commits, each adding one document.
//   - main~0 (or main) → all 3 docs
//   - main~1           → 2 docs (third commit not visible)
//   - main~2           → 1 doc
//   - main~3           → 0 docs (initial empty commit)
func TestRTVLLoad_AncestorExpr_Query(t *testing.T) {
	b := newTestBackend(t)

	// First commit: add doc1.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	commitDB(t, b, "testdb", "add doc1")

	// Second commit: add doc2.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))
	commitDB(t, b, "testdb", "add doc2")

	// Third commit: add doc3.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(3), "v", int64(3)))
	commitDB(t, b, "testdb", "add doc3")

	cases := []struct {
		rootish  string
		wantDocs int
	}{
		{"testdb__main~0", 3},
		{"testdb__main~1", 2},
		{"testdb__main~2", 1},
		{"testdb__main~3", 0},
	}

	for _, tc := range cases {
		n := countDocs(t, b, tc.rootish, "col")
		if n != tc.wantDocs {
			t.Errorf("%s: want %d docs, got %d", tc.rootish, tc.wantDocs, n)
		}
	}
}

// TestRTVLLoad_AncestorExpr_IsolatedFromMain verifies that an open ~N cursor is
// not affected by subsequent inserts on main.
func TestRTVLLoad_AncestorExpr_IsolatedFromMain(t *testing.T) {
	b := newTestBackend(t)

	// Three commits.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, "testdb", "add doc1")

	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2)))
	commitDB(t, b, "testdb", "add doc2")

	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(3)))
	commitDB(t, b, "testdb", "add doc3")

	// main~1 should see 2 docs at this point.
	nBefore := countDocs(t, b, "testdb__main~1", "col")
	if nBefore != 2 {
		t.Fatalf("testdb__main~1 before new insert: want 2, got %d", nBefore)
	}

	// Insert a new doc (uncommitted) on main — the ~N view must remain unchanged.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(4)))

	nAfter := countDocs(t, b, "testdb__main~1", "col")
	if nAfter != 2 {
		t.Errorf("testdb__main~1 after uncommitted insert: want 2, got %d", nAfter)
	}

	// Commit the new doc and verify ~1 still points to the same snapshot.
	commitDB(t, b, "testdb", "add doc4")

	nCommitted := countDocs(t, b, "testdb__main~1", "col")
	if nCommitted != 3 {
		// After a 4th commit, main~1 now points to the 3-doc snapshot.
		t.Errorf("testdb__main~1 after 4th commit: want 3, got %d", nCommitted)
	}
}

// TestRTVLLoad_AncestorExpr_TooDeep verifies that requesting an ancestor
// beyond the commit history returns an error rather than silent empty results.
//
// When a database is first opened, Dolt creates an initial STRT commit with no
// parent. After one user-level commitDB call, "main" has 2 commits:
//   - STRT (no parent)
//   - "only commit"
//
// So main~1 resolves to the STRT commit (0 docs, no error), and main~2 tries to
// walk past the STRT commit's non-existent parent — that is the "too deep" case.
func TestRTVLLoad_AncestorExpr_TooDeep(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// One user commit on top of the STRT commit; main~2 exceeds history.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, "testdb", "only commit")

	db, err := b.Database("testdb__main~2")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	_, err = coll.Query(ctx, &backends.QueryParams{})
	if err == nil {
		t.Error("expected error when ancestor depth exceeds history, got nil")
	}
}

// TestRTVLLoad_Tag_Query verifies that a tag rootish reads from the tagged commit.
func TestRTVLLoad_Tag_Query(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Insert doc1 and commit.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "v1 release")

	// Insert doc2 and commit.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))
	commitDB(t, b, "testdb", "v2 release")

	// Create a tag "v1.0" pointing to hash1 by writing a tag dataset directly.
	state, err := b.getOrOpenDB(ctx, "testdb", false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	state.mu.Lock()
	tagHash, ok := hash.MaybeParse(hash1)
	if !ok {
		state.mu.Unlock()
		t.Fatalf("invalid hash: %q", hash1)
	}
	// Create a tag dataset pointing to hash1.
	tagDS, err := state.doltDB.GetDataset(ctx, "refs/tags/v1.0")
	if err != nil {
		state.mu.Unlock()
		t.Fatalf("GetDataset(refs/tags/v1.0): %v", err)
	}
	tagDS, err = state.doltDB.SetHead(ctx, tagDS, tagHash, "")
	if err != nil {
		state.mu.Unlock()
		t.Fatalf("SetHead(tag v1.0 → %s): %v", hash1, err)
	}
	_ = tagDS
	state.mu.Unlock()

	// Reading from tag "v1.0" should see only doc1 (as at hash1).
	n := countDocs(t, b, "testdb__v1.0", "col")
	if n != 1 {
		t.Errorf("testdb__v1.0: want 1 doc, got %d", n)
	}
}

// TestRTVLLoad_DongoBranch_FromHash verifies that a branch can be created from a
// commit-hash rootish. The new branch points to that exact commit, not main HEAD.
func TestRTVLLoad_DongoBranch_FromHash(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Commit doc1 → hash1. This is the commit the new branch will point at.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "first commit")

	// Commit doc2 on main so main HEAD advances past hash1.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))
	commitDB(t, b, "testdb", "second commit")

	// Create a branch "snap" rooted at hash1 (not main HEAD).
	_, err := b.DongoBranch(ctx, &backends.BranchParams{
		DBName: "testdb",
		Name:   "snap",
		From:   hash1,
	})
	if err != nil {
		t.Fatalf("DongoBranch from hash: %v", err)
	}

	// Reading from the new branch should see only doc1 (the state at hash1).
	n := countDocs(t, b, "testdb__snap", "col")
	if n != 1 {
		t.Errorf("testdb__snap: want 1 doc (at hash1), got %d", n)
	}
}

// TestRTVLLoad_DongoBranch_FromAncestor verifies that a branch can be created from
// an ancestor-expression rootish. The new branch points to the resolved ancestor commit.
func TestRTVLLoad_DongoBranch_FromAncestor(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Three commits.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	commitDB(t, b, "testdb", "first commit")
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))
	commitDB(t, b, "testdb", "second commit")
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(3), "v", int64(3)))
	commitDB(t, b, "testdb", "third commit")

	// Create branch "back1" from main~1 (parent of HEAD = second commit = 2 docs).
	_, err := b.DongoBranch(ctx, &backends.BranchParams{
		DBName: "testdb",
		Name:   "back1",
		From:   "main~1",
	})
	if err != nil {
		t.Fatalf("DongoBranch from main~1: %v", err)
	}

	// back1 should see 2 docs (state at main~1 = second commit).
	n := countDocs(t, b, "testdb__back1", "col")
	if n != 2 {
		t.Errorf("testdb__back1: want 2 docs (at main~1), got %d", n)
	}
}

// TestRTVLLoad_BranchWrite_Isolation verifies that writes via a non-main branch
// rootish (mydb__feature) go to that branch's working set and are isolated from main.
//
// Setup:
//   - Commit one doc on main → main has 1 doc committed.
//   - Create branch "feature" from main HEAD.
//   - Write a second doc via "testdb__feature" (into feature's working set, uncommitted).
//   - Write a third doc via "testdb" / "testdb__main" (into main's working set).
//
// Expected:
//   - testdb__feature sees 2 docs (inherited committed doc + feature working-set write).
//   - testdb / testdb__main sees 2 docs (committed doc + main working-set write).
//   - The two branches see different second documents (isolated writes).
func TestRTVLLoad_BranchWrite_Isolation(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// Commit one doc on main.
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", "main-committed"))
	commitDB(t, b, "testdb", "baseline commit")

	// Create branch "feature" from main HEAD.
	_, err := b.DongoBranch(ctx, &backends.BranchParams{
		DBName: "testdb",
		Name:   "feature",
		From:   "main",
	})
	if err != nil {
		t.Fatalf("DongoBranch: %v", err)
	}

	// Write to feature branch working set (not committed).
	insertDoc(t, b, "testdb__feature", "col", mustDoc(t, "_id", int64(2), "v", "feature-only"))

	// Write to main working set (not committed).
	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(3), "v", "main-only"))

	// feature branch should see doc1 (committed) + doc2 (feature write) = 2.
	nFeature := countDocs(t, b, "testdb__feature", "col")
	if nFeature != 2 {
		t.Errorf("testdb__feature: want 2 docs, got %d", nFeature)
	}

	// main should see doc1 (committed) + doc3 (main write) = 2.
	nMain := countDocs(t, b, "testdb", "col")
	if nMain != 2 {
		t.Errorf("testdb (main): want 2 docs, got %d", nMain)
	}

	// feature branch must NOT see doc3 (main-only write).
	// main must NOT see doc2 (feature-only write).
	// We verify this indirectly: if both branches showed 3 docs, the writes leaked.
	// The count checks above cover this since each branch has exactly 2 docs.
}
