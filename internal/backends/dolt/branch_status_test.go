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

// These tests port dolt's BranchStatusTableFunctionScriptTests graphs to the
// backend layer, building the same topologies with empty commits and asserting
// the same (ahead, behind) expectations.

func emptyCommit(t *testing.T, b *Backend, dbName, branch, msg string) string {
	t.Helper()
	res, err := b.DumboDBCommit(context.Background(), &backends.CommitParams{
		DBName:     dbName,
		Branch:     branch,
		Message:    msg,
		Author:     "testuser <test@dolthub.com>",
		AllowEmpty: true,
	})
	if err != nil {
		t.Fatalf("emptyCommit(%q, %q): %v", branch, msg, err)
	}
	return res.CommitID
}

func bsBranch(t *testing.T, b *Backend, dbName, from, name string) {
	t.Helper()
	if _, err := b.DumboDBBranch(context.Background(), &backends.BranchParams{
		DBName: dbName,
		From:   from,
		Name:   name,
	}); err != nil {
		t.Fatalf("bsBranch(%q from %q): %v", name, from, err)
	}
}

func makeTag(t *testing.T, b *Backend, dbName, name, hashish string) {
	t.Helper()
	if _, err := b.DumboDBTag(context.Background(), &backends.TagParams{
		DBName: dbName,
		Name:   name,
		Hash:   hashish,
	}); err != nil {
		t.Fatalf("makeTag(%q -> %q): %v", name, hashish, err)
	}
}

func mergeBranch(t *testing.T, b *Backend, dbName, into, from string) {
	t.Helper()
	if _, err := b.DumboDBMerge(context.Background(), &backends.MergeParams{
		DBName: dbName,
		Into:   into,
		From:   from,
	}); err != nil {
		t.Fatalf("mergeBranch(%q into %q): %v", from, into, err)
	}
}

// branchStatusMap calls DumboDBBranchStatus and returns the result plus a map of
// target -> entry for order-independent assertions.
func branchStatusMap(t *testing.T, b *Backend, dbName, base string, targets ...string) (*backends.BranchStatusResult, map[string]backends.BranchStatusEntry) {
	t.Helper()
	res, err := b.DumboDBBranchStatus(context.Background(), &backends.BranchStatusParams{
		DBName:  dbName,
		Base:    base,
		Targets: targets,
	})
	if err != nil {
		t.Fatalf("DumboDBBranchStatus(base=%q, targets=%v): %v", base, targets, err)
	}
	m := make(map[string]backends.BranchStatusEntry, len(res.Entries))
	for _, e := range res.Entries {
		m[e.Target] = e
	}
	return res, m
}

func assertAheadBehind(t *testing.T, m map[string]backends.BranchStatusEntry, target string, ahead, behind int32) {
	t.Helper()
	e, ok := m[target]
	if !ok {
		t.Fatalf("no entry for target %q", target)
	}
	if e.CommitsAhead != ahead || e.CommitsBehind != behind {
		t.Errorf("target %q: got (ahead=%d, behind=%d), want (ahead=%d, behind=%d)",
			target, e.CommitsAhead, e.CommitsBehind, ahead, behind)
	}
}

// initBaseline creates testdb with a single committed baseline ("anc") on main and
// returns the backend.
func initBaseline(t *testing.T) *Backend {
	t.Helper()
	b := newTestBackend(t)
	insertDoc(t, b, "testdb", "c", mustDoc(t, "_id", int64(0)))
	commitDB(t, b, "testdb", "anc")
	return b
}

// TestDumboDBBranchStatus_DivergentGraph ports dolt's first scenario (time flows
// left to right; anc is the shared baseline):
//
//	          * b1 --- * b2
//	         /
//	* anc
//	         \
//	          * main --- * b3 --- * b4 --- * b5
func TestDumboDBBranchStatus_DivergentGraph(t *testing.T) {
	b := initBaseline(t)

	bsBranch(t, b, "testdb", "main", "b1") // b1 from anc
	emptyCommit(t, b, "testdb", "main", "main")

	emptyCommit(t, b, "testdb", "b1", "b1")
	bsBranch(t, b, "testdb", "b1", "b2")
	emptyCommit(t, b, "testdb", "b2", "b2")

	bsBranch(t, b, "testdb", "main", "b3")
	emptyCommit(t, b, "testdb", "b3", "b3")
	bsBranch(t, b, "testdb", "b3", "b4")
	emptyCommit(t, b, "testdb", "b4", "b4")
	bsBranch(t, b, "testdb", "b4", "b5")
	b5Hash := emptyCommit(t, b, "testdb", "b5", "b5")

	makeTag(t, b, "testdb", "t1", "b1")
	makeTag(t, b, "testdb", "t2", "b2")
	makeTag(t, b, "testdb", "t3", "b3")
	makeTag(t, b, "testdb", "t4", "b4")
	makeTag(t, b, "testdb", "t5", "b5")

	res, m := branchStatusMap(t, b, "testdb", "main", "main", "b1", "b2", "b3", "b4", "b5")
	if res.BaseTarget != "main" {
		t.Errorf("BaseTarget = %q, want main", res.BaseTarget)
	}
	if len(res.BaseHash) != 32 {
		t.Errorf("BaseHash = %q, want a 32-char hash", res.BaseHash)
	}
	assertAheadBehind(t, m, "main", 0, 0)
	assertAheadBehind(t, m, "b1", 1, 1)
	assertAheadBehind(t, m, "b2", 2, 1)
	assertAheadBehind(t, m, "b3", 1, 0)
	assertAheadBehind(t, m, "b4", 2, 0)
	assertAheadBehind(t, m, "b5", 3, 0)

	// Tags resolve the same as their target branches.
	_, mt := branchStatusMap(t, b, "testdb", "main", "t1", "t2", "t3", "t4", "t5")
	assertAheadBehind(t, mt, "t1", 1, 1)
	assertAheadBehind(t, mt, "t2", 2, 1)
	assertAheadBehind(t, mt, "t3", 1, 0)
	assertAheadBehind(t, mt, "t4", 2, 0)
	assertAheadBehind(t, mt, "t5", 3, 0)

	// b2 as base, b5 as target.
	_, m2 := branchStatusMap(t, b, "testdb", "b2", "b5")
	assertAheadBehind(t, m2, "b5", 4, 2)

	// Ancestor expressions resolve relative to the named branch.
	_, m3 := branchStatusMap(t, b, "testdb", "main", "b5", "b5~1", "b5~2")
	assertAheadBehind(t, m3, "b5", 3, 0)
	assertAheadBehind(t, m3, "b5~1", 2, 0)
	assertAheadBehind(t, m3, "b5~2", 1, 0)

	// A bare commit hash is a valid refspec, and its resolved hash echoes back.
	_, m4 := branchStatusMap(t, b, "testdb", "main", b5Hash)
	assertAheadBehind(t, m4, b5Hash, 3, 0)
	if m4[b5Hash].Hash != b5Hash {
		t.Errorf("hash entry resolved to %q, want %q", m4[b5Hash].Hash, b5Hash)
	}
}

// TestDumboDBBranchStatus_Merge ports dolt's merge scenario: ahead/behind before
// and after merging b1 into b2 (time flows left to right; anc is the shared
// baseline, branch heads are labeled in parentheses):
//
//	          * b1c1 --- * b1c2              (b1)
//	         /
//	* anc --- * m1 --- * m2                  (main)
//	         \
//	          * b2c1 --- * b2c2 --- * b2c3   (b2)
func TestDumboDBBranchStatus_Merge(t *testing.T) {
	b := initBaseline(t)

	bsBranch(t, b, "testdb", "main", "b1")
	bsBranch(t, b, "testdb", "main", "b2")

	emptyCommit(t, b, "testdb", "main", "m1")
	emptyCommit(t, b, "testdb", "main", "m2")

	emptyCommit(t, b, "testdb", "b1", "b1c1")
	emptyCommit(t, b, "testdb", "b1", "b1c2")

	emptyCommit(t, b, "testdb", "b2", "b2c1")
	emptyCommit(t, b, "testdb", "b2", "b2c2")
	emptyCommit(t, b, "testdb", "b2", "b2c3")

	_, m := branchStatusMap(t, b, "testdb", "main", "b1", "b2")
	assertAheadBehind(t, m, "b1", 2, 2)
	assertAheadBehind(t, m, "b2", 3, 2)

	_, m2 := branchStatusMap(t, b, "testdb", "b1", "b2")
	assertAheadBehind(t, m2, "b2", 3, 2)

	mergeBranch(t, b, "testdb", "b2", "b1") // merge b1 into b2

	_, m3 := branchStatusMap(t, b, "testdb", "main", "b1", "b2")
	assertAheadBehind(t, m3, "b1", 2, 2)
	assertAheadBehind(t, m3, "b2", 6, 2)

	_, m4 := branchStatusMap(t, b, "testdb", "b1", "b2")
	assertAheadBehind(t, m4, "b2", 4, 0)
}

// TestDumboDBBranchStatus_MergeCommitAsTarget verifies that a merge commit is
// counted as ahead by every commit reachable from it but not the base -- the
// merged-in branch commits plus the merge commit itself, not just the merge.
//
// Graph: merge b2 (one commit off anc) into a branch off main (one commit off
// anc). The merge is 2 ahead of main: the b2 commit and the merge commit.
//
//	          * b2
//	         /     \
//	* anc            \
//	         \         \
//	          * main --- * merge   (merge is 2 ahead of main)
func TestDumboDBBranchStatus_MergeCommitAsTarget(t *testing.T) {
	b := initBaseline(t)

	bsBranch(t, b, "testdb", "main", "b2") // b2 from anc
	emptyCommit(t, b, "testdb", "main", "main")
	emptyCommit(t, b, "testdb", "b2", "b2")

	bsBranch(t, b, "testdb", "main", "rel") // rel from main
	mergeBranch(t, b, "testdb", "rel", "b2")

	_, m := branchStatusMap(t, b, "testdb", "main", "rel")
	assertAheadBehind(t, m, "rel", 2, 0)
}

// TestDumboDBBranchStatus_MergeCommitDeeperGraph is the same idea over a longer
// main line and a two-commit feature branch. The merge is 3 ahead of main: the
// two feature commits plus the merge commit.
//
//	          * b1 --- * b2
//	         /            \
//	* anc                  \
//	         \               \
//	          * x1 --- * x2 --- * main --- * merge   (merge is 3 ahead of main)
func TestDumboDBBranchStatus_MergeCommitDeeperGraph(t *testing.T) {
	b := initBaseline(t)

	bsBranch(t, b, "testdb", "main", "feat") // feat from anc
	emptyCommit(t, b, "testdb", "feat", "b1")
	emptyCommit(t, b, "testdb", "feat", "b2")

	emptyCommit(t, b, "testdb", "main", "x1")
	emptyCommit(t, b, "testdb", "main", "x2")
	emptyCommit(t, b, "testdb", "main", "main")

	bsBranch(t, b, "testdb", "main", "rel") // rel from main
	mergeBranch(t, b, "testdb", "rel", "feat")

	_, m := branchStatusMap(t, b, "testdb", "main", "rel")
	assertAheadBehind(t, m, "rel", 3, 0)
}

// TestDumboDBBranchStatus_SelfAndEmpty verifies the boundary cases: a base-only
// call yields no entries, and comparing a ref against itself is (0, 0).
func TestDumboDBBranchStatus_SelfAndEmpty(t *testing.T) {
	b := initBaseline(t)
	emptyCommit(t, b, "testdb", "main", "c1")

	res, err := b.DumboDBBranchStatus(context.Background(), &backends.BranchStatusParams{
		DBName: "testdb",
		Base:   "main",
	})
	if err != nil {
		t.Fatalf("base-only call: %v", err)
	}
	if len(res.Entries) != 0 {
		t.Errorf("base-only call: got %d entries, want 0", len(res.Entries))
	}
	if res.BaseHash == "" {
		t.Errorf("base-only call: BaseHash should still be populated")
	}

	_, m := branchStatusMap(t, b, "testdb", "main", "main")
	assertAheadBehind(t, m, "main", 0, 0)
}

// TestDumboDBBranchStatus_Errors verifies refspec resolution failures surface as
// errors for both the base and the targets.
func TestDumboDBBranchStatus_Errors(t *testing.T) {
	b := initBaseline(t)

	if _, err := b.DumboDBBranchStatus(context.Background(), &backends.BranchStatusParams{
		DBName: "testdb", Base: "main", Targets: []string{"nope"},
	}); err == nil {
		t.Error("expected error for unknown target, got nil")
	}

	if _, err := b.DumboDBBranchStatus(context.Background(), &backends.BranchStatusParams{
		DBName: "testdb", Base: "main", Targets: []string{""},
	}); err == nil {
		t.Error("expected error for empty-string target, got nil")
	}

	if _, err := b.DumboDBBranchStatus(context.Background(), &backends.BranchStatusParams{
		DBName: "testdb", Base: "does-not-exist", Targets: []string{"main"},
	}); err == nil {
		t.Error("expected error for unknown base, got nil")
	}
}
