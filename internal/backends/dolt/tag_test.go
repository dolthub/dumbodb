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
)

// TestDumboDBTag_CreateListDelete exercises the full tag lifecycle: create at a
// commit hash, list to confirm visibility, then delete.
func TestDumboDBTag_CreateListDelete(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	hash1 := commitDB(t, b, "testdb", "first commit")

	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))
	commitDB(t, b, "testdb", "second commit")

	// Create a tag at hash1.
	createRes, err := b.DumboDBTag(ctx, &backends.TagParams{
		DBName:  "testdb",
		Branch:  "main",
		Name:    "v1.0",
		Hash:    hash1,
		Message: "first release",
		Author:  "alice",
		Email:   "alice@example.com",
	})
	if err != nil {
		t.Fatalf("DumboDBTag create: %v", err)
	}
	if len(createRes.Tags) != 1 {
		t.Fatalf("create: want 1 tag, got %d", len(createRes.Tags))
	}
	if createRes.Tags[0].Name != "v1.0" {
		t.Errorf("create: want name v1.0, got %q", createRes.Tags[0].Name)
	}
	if createRes.Tags[0].CommitID != hash1 {
		t.Errorf("create: want commit %s, got %s", hash1, createRes.Tags[0].CommitID)
	}
	if createRes.Tags[0].Tagger != "alice" {
		t.Errorf("create: want tagger alice, got %q", createRes.Tags[0].Tagger)
	}

	// Listing should show the tag.
	listRes, err := b.DumboDBTag(ctx, &backends.TagParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DumboDBTag list: %v", err)
	}
	if len(listRes.Tags) != 1 {
		t.Fatalf("list: want 1 tag, got %d", len(listRes.Tags))
	}
	if listRes.Tags[0].Name != "v1.0" || listRes.Tags[0].CommitID != hash1 {
		t.Errorf("list: want v1.0 -> %s, got %s -> %s", hash1, listRes.Tags[0].Name, listRes.Tags[0].CommitID)
	}
	if listRes.Tags[0].Message != "first release" {
		t.Errorf("list: want message %q, got %q", "first release", listRes.Tags[0].Message)
	}

	// Delete the tag.
	delRes, err := b.DumboDBTag(ctx, &backends.TagParams{
		DBName: "testdb",
		Name:   "v1.0",
		Delete: true,
	})
	if err != nil {
		t.Fatalf("DumboDBTag delete: %v", err)
	}
	if len(delRes.Tags) != 1 || delRes.Tags[0].Name != "v1.0" {
		t.Errorf("delete: unexpected result %+v", delRes.Tags)
	}

	// List again  -- should be empty.
	listRes2, err := b.DumboDBTag(ctx, &backends.TagParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DumboDBTag list-after-delete: %v", err)
	}
	if len(listRes2.Tags) != 0 {
		t.Errorf("list-after-delete: want 0 tags, got %d", len(listRes2.Tags))
	}
}

// TestDumboDBTag_DefaultsToBranchHEAD verifies that creating a tag without an
// explicit Hash uses the connection's branch HEAD.
func TestDumboDBTag_DefaultsToBranchHEAD(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1)))
	headHash := commitDB(t, b, "testdb", "first commit")

	res, err := b.DumboDBTag(ctx, &backends.TagParams{
		DBName: "testdb",
		Branch: "main",
		Name:   "current",
	})
	if err != nil {
		t.Fatalf("DumboDBTag create: %v", err)
	}
	if res.Tags[0].CommitID != headHash {
		t.Errorf("default-hash: want %s (main HEAD), got %s", headHash, res.Tags[0].CommitID)
	}
}

// TestDumboDBTag_DuplicateRejected verifies that creating a tag that already
// exists returns an error rather than silently overwriting.
func TestDumboDBTag_DuplicateRejected(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, "testdb", "first commit")

	if _, err := b.DumboDBTag(ctx, &backends.TagParams{
		DBName: "testdb", Branch: "main", Name: "dup",
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}

	if _, err := b.DumboDBTag(ctx, &backends.TagParams{
		DBName: "testdb", Branch: "main", Name: "dup",
	}); err == nil {
		t.Fatalf("second create: expected error for duplicate tag, got nil")
	}
}

// TestDumboDBTag_DeleteMissing verifies that deleting a non-existent tag
// returns an error (collection-does-not-exist code).
func TestDumboDBTag_DeleteMissing(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, "testdb", "first commit")

	_, err := b.DumboDBTag(ctx, &backends.TagParams{
		DBName: "testdb",
		Name:   "missing",
		Delete: true,
	})
	if err == nil {
		t.Fatalf("delete missing: expected error, got nil")
	}
}
