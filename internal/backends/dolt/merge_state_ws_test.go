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
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// conflictedMerge seeds dbName with a document both branches edit differently,
// then starts the merge that conflicts on it.
func conflictedMerge(t *testing.T, b *Backend, dbName string) {
	t.Helper()
	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, dbName, true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	insertDocForTest(t, ctx, b, dbName, 1)
	commitBranch(t, b, dbName, "main", "seed")
	branchFrom(t, b, dbName, "main", "feature")

	setValue(t, b, dbName, "feature", 200)
	commitBranch(t, b, dbName, "feature", "theirs")
	setValue(t, b, dbName, defaultBranch, 100)
	commitBranch(t, b, dbName, "main", "ours")

	if _, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: dbName, Into: "main", From: "feature", Message: "merge", Author: "t <t@e>",
	}); err == nil {
		t.Fatal("divergent edits to one document must surface a conflict")
	}
}

func setValue(t *testing.T, b *Backend, dbName, branch string, v int64) {
	t.Helper()
	coll := collAt(t, b, dbName, branch, "col")
	doc, err := types.NewDocument("_id", int64(1), "v", v)
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	doc.SetRecordID(1)
	if _, err := coll.UpdateAll(context.Background(), &backends.UpdateAllParams{
		Docs: []*types.Document{doc},
	}); err != nil {
		t.Fatalf("UpdateAll(%s@%s): %v", dbName, branch, err)
	}
}

// branchWorkingSet resolves branch's working set straight from disk, so the
// test sees what a restart would see rather than any in-memory cache.
func branchWorkingSet(t *testing.T, b *Backend, dbName, branch string) (*dbState, *doltdb.WorkingSet) {
	t.Helper()
	db, err := b.getOrOpenDB(context.Background(), dbName, false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	ws, err := db.doltDB.ResolveWorkingSet(context.Background(), doltref.NewWorkingSetRef("heads/"+branch))
	if err != nil {
		t.Fatalf("ResolveWorkingSet(%q): %v", branch, err)
	}
	return db, ws
}

// artifactCount reports how many conflict artifacts the working root of branch
// still carries for collName.
func artifactCount(t *testing.T, b *Backend, dbName, branch, collName string) uint64 {
	t.Helper()
	ctx := context.Background()
	db, ws := branchWorkingSet(t, b, dbName, branch)
	am, err := amFromWorkingRoot(ctx, ws.WorkingRoot(), db.ns)
	if err != nil {
		t.Fatalf("amFromWorkingRoot: %v", err)
	}
	artMap, err := openCollectionArtifacts(ctx, db, am, collName)
	if err != nil {
		t.Fatalf("openCollectionArtifacts: %v", err)
	}
	n, err := artMap.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	return uint64(n)
}

// A paused merge must describe itself in the branch working set, not beside it.
func TestPausedMergeLivesInWorkingSet(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()

	conflictedMerge(t, b, "wsmerge")

	entries, err := os.ReadDir(filepath.Join(dir, "wsmerge"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == ".dumbodb_merge_state.json" {
			t.Fatal("merge state must not be written to a sidecar file")
		}
	}

	_, ws := branchWorkingSet(t, b, "wsmerge", "main")
	if !ws.MergeActive() {
		t.Fatal("the branch working set must record the paused merge")
	}
	if ws.MergeState().IsCherryPick() || ws.MergeState().IsRevert() {
		t.Fatal("a plain merge must not be recorded as a cherry-pick or revert")
	}
	// The slot carries the rendered label, not the bare refspec, so the wording
	// of a paused conflict does not depend on the ref still existing.
	if spec := ws.MergeState().CommitSpecStr(); spec != "branch 'feature'" {
		t.Fatalf("merge source label = %q, want \"branch 'feature'\"", spec)
	}
	if ws.MergeState().PreMergeWorkingRoot() == nil {
		t.Fatal("the pre-merge root must be reachable from the working set")
	}
	if n := artifactCount(t, b, "wsmerge", "main", "col"); n != 1 {
		t.Fatalf("working root carries %d conflict artifacts, want 1", n)
	}
}

// Completing a merge must leave neither operation metadata nor the conflict
// artifacts it was paused on, whichever side the user resolved to.
func TestContinueClearsArtifactsAndMergeState(t *testing.T) {
	for _, resolution := range []string{"ours", "theirs"} {
		t.Run(resolution, func(t *testing.T) {
			b, dir := newBackendForTest(t)
			defer os.RemoveAll(dir)
			defer b.Close()
			ctx := context.Background()

			dbName := "wscont" + resolution
			conflictedMerge(t, b, dbName)

			confl, err := b.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: dbName, Branch: "main"})
			if err != nil {
				t.Fatalf("DumboDBConflicts: %v", err)
			}
			if len(confl.Collections) != 1 || len(confl.Collections[0].Conflicts) != 1 {
				t.Fatalf("expected one conflict in one collection, got %+v", confl.Collections)
			}
			if _, err := b.DumboDBResolveConflict(ctx, &backends.ResolveConflictParams{
				DBName: dbName, Branch: "main", Collection: "col",
				ConflictID: confl.Collections[0].Conflicts[0].ConflictID, Resolution: resolution,
			}); err != nil {
				t.Fatalf("DumboDBResolveConflict(%s): %v", resolution, err)
			}
			if _, err := b.DumboDBMerge(ctx, &backends.MergeParams{
				DBName: dbName, Into: "main", Continue: true, Message: "merge", Author: "t <t@e>",
			}); err != nil {
				t.Fatalf("DumboDBMerge continue: %v", err)
			}

			_, ws := branchWorkingSet(t, b, dbName, "main")
			if ws.MergeActive() || ws.RebaseActive() {
				t.Fatal("a completed merge must leave no operation metadata behind")
			}
			if n := artifactCount(t, b, dbName, "main", "col"); n != 0 {
				t.Fatalf("working root still carries %d conflict artifacts after continue", n)
			}
		})
	}
}

// Aborting must roll the working set back to the pre-merge root and drop the
// operation metadata, so nothing survives to be picked up on the next open.
func TestAbortClearsWorkingSetMergeState(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()
	ctx := context.Background()

	conflictedMerge(t, b, "wsabort")

	if _, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: "wsabort", Into: "main", Abort: true,
	}); err != nil {
		t.Fatalf("DumboDBMerge abort: %v", err)
	}

	_, ws := branchWorkingSet(t, b, "wsabort", "main")
	if ws.MergeActive() || ws.RebaseActive() {
		t.Fatal("an aborted merge must leave no operation metadata behind")
	}
	if n := artifactCount(t, b, "wsabort", "main", "col"); n != 0 {
		t.Fatalf("working root still carries %d conflict artifacts after abort", n)
	}
}

// Reopening the database has to recover the paused operation from the tree.
func TestPausedMergeReloadsOnOpen(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	ctx := context.Background()

	conflictedMerge(t, b, "wsreload")
	before, err := b.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: "wsreload", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBConflicts: %v", err)
	}
	b.Close()

	reopened := &Backend{
		dataDir: dir,
		l:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		dbs:     make(map[string]*dbState),
	}
	defer reopened.Close()

	after, err := reopened.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: "wsreload", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBConflicts after reopen: %v", err)
	}
	if len(after.Collections) != 1 || len(after.Collections[0].Conflicts) != 1 {
		t.Fatalf("reopen lost the paused conflict: %+v", after.Collections)
	}
	wantID := before.Collections[0].Conflicts[0].ConflictID
	gotID := after.Collections[0].Conflicts[0].ConflictID
	if gotID != wantID {
		t.Fatalf("conflict id changed across reopen: %q -> %q", wantID, gotID)
	}
	wantMsg := before.Collections[0].Conflicts[0].Reason.Message
	gotMsg := after.Collections[0].Conflicts[0].Reason.Message
	if gotMsg != wantMsg {
		t.Fatalf("conflict reason changed across reopen:\n before %q\n after  %q", wantMsg, gotMsg)
	}
}

// An operation's commit and the working set it settles must land together. If
// they were two updates, a crash in between would leave the commit on the
// branch while the working set still advertised the operation, and the branch
// could then be neither continued (its head has moved past the parents the
// retry wants) nor sensibly aborted.
func TestOperationCommitAndSettleAreOneUpdate(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	ctx := context.Background()

	conflictedMerge(t, b, "wsatomic")
	db, err := b.getOrOpenDB(ctx, "wsatomic", false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	ms := db.mergeState

	confl, err := b.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: "wsatomic", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBConflicts: %v", err)
	}
	if _, err := b.DumboDBResolveConflict(ctx, &backends.ResolveConflictParams{
		DBName: "wsatomic", Branch: "main", Collection: "col",
		ConflictID: confl.Collections[0].Conflicts[0].ConflictID, Resolution: "ours",
	}); err != nil {
		t.Fatalf("DumboDBResolveConflict: %v", err)
	}

	// Exactly the commit half of continue, with nothing after it. Whatever the
	// merge commit made durable, the working set has to have settled with it.
	db.mu.Lock()
	if err := clearConflictArtifacts(ctx, db, ms); err != nil {
		db.mu.Unlock()
		t.Fatalf("clearConflictArtifacts: %v", err)
	}
	res, err := b.commitMerge(ctx, db, ms.fromLabel, ms.intoBranch, ms.intoHash, ms.fromHash, ms.resolvedAM, "merged", "t <t@e>", "")
	if err != nil {
		db.mu.Unlock()
		t.Fatalf("commitMerge: %v", err)
	}
	db.mu.Unlock()

	b.Close()
	reopened := &Backend{
		dataDir: dir,
		l:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		dbs:     make(map[string]*dbState),
	}
	defer reopened.Close()

	db2, err := reopened.getOrOpenDB(ctx, "wsatomic", false)
	if err != nil {
		t.Fatalf("getOrOpenDB after reopen: %v", err)
	}
	if db2.mergeState != nil {
		t.Fatal("the merge commit landed, so reopening must not find the merge still in progress")
	}

	head, err := branchHeadHash(ctx, db2, "main")
	if err != nil {
		t.Fatalf("branchHeadHash: %v", err)
	}
	if head.String() != res.CommitID {
		t.Fatalf("HEAD = %s, want the merge commit %s", head.String(), res.CommitID)
	}
	if n := artifactCount(t, reopened, "wsatomic", "main", "col"); n != 0 {
		t.Fatalf("working root still carries %d conflict artifacts", n)
	}
}

// refLabel resolves against the live ref namespace, so the wording of a paused
// conflict has to be captured when the merge starts rather than re-derived on
// reload: a source branch removed mid-merge would otherwise re-word the reason
// from "branch 'feature'" to "commit 'feature'".
func TestConflictReasonSurvivesSourceBranchRemoval(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	ctx := context.Background()

	conflictedMerge(t, b, "wslabel")
	before, err := b.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: "wslabel", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBConflicts: %v", err)
	}
	want := before.Collections[0].Conflicts[0].Reason.Message

	if _, err := b.DumboDBBranch(ctx, &backends.BranchParams{
		DBName: "wslabel", Action: "remove", Name: "feature", Force: true,
	}); err != nil {
		t.Fatalf("removing the merge source branch: %v", err)
	}
	b.Close()

	reopened := &Backend{
		dataDir: dir,
		l:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		dbs:     make(map[string]*dbState),
	}
	defer reopened.Close()

	after, err := reopened.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: "wslabel", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBConflicts after reopen: %v", err)
	}
	got := after.Collections[0].Conflicts[0].Reason.Message
	if got != want {
		t.Fatalf("conflict reason changed when the source branch went away:\n before %q\n after  %q", want, got)
	}
}
