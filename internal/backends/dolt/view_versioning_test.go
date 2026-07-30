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
	"os"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func matchOnPipeline(t *testing.T, field, value string) *types.Array {
	t.Helper()
	match, err := types.NewDocument(field, value)
	if err != nil {
		t.Fatalf("NewDocument(match): %v", err)
	}
	stage, err := types.NewDocument("$match", match)
	if err != nil {
		t.Fatalf("NewDocument(stage): %v", err)
	}
	pipe := types.MakeArray(1)
	pipe.Append(stage)
	return pipe
}

// TestViewStatusAndDiff verifies that view lifecycle (add, redefine, drop)
// surfaces in DumboDBStatus (name lists) and DumboDBDiff (full definitions),
// alongside collection changes (workspace-z0i.7). No MongoDB oracle exists for
// version-control commands, so this is a dumbodb-repo test.
func TestViewStatusAndDiff(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()

	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, "vdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	insertDocForTest(t, ctx, b, "vdb", 1) // base collection "col"

	db, err := b.Database("vdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	status := func() *backends.VersioningStatusResult {
		st, serr := b.DumboDBStatus(ctx, &backends.VersioningStatusParams{DBName: "vdb", Branch: "main"})
		if serr != nil {
			t.Fatalf("DumboDBStatus: %v", serr)
		}
		return st
	}
	diffViews := func() []backends.ViewChange {
		df, derr := b.DumboDBDiff(ctx, &backends.DiffParams{DBName: "vdb", ConnRootish: "main"})
		if derr != nil {
			t.Fatalf("DumboDBDiff: %v", derr)
		}
		return df.Views
	}
	commit := func(msg string) {
		if _, cerr := b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "vdb", Message: msg, Author: "t"}); cerr != nil {
			t.Fatalf("DumboDBCommit(%q): %v", msg, cerr)
		}
	}

	commit("seed base collection")

	// --- add a view ---
	if err := db.CreateCollection(ctx, &backends.CreateCollectionParams{
		Name: "v1", ViewOn: "col", ViewPipeline: matchOnPipeline(t, "status", "active"),
	}); err != nil {
		t.Fatalf("CreateCollection(view): %v", err)
	}
	if st := status(); !viewHasStatus(st.Views, "v1", "added") {
		t.Fatalf("added view not in status.Views: %+v", st.Views)
	}
	if vs := diffViews(); len(vs) != 1 || vs[0].Name != "v1" || vs[0].Status != "added" || vs[0].To == nil || vs[0].To.ViewOn != "col" {
		t.Fatalf("added view diff wrong: %+v", vs)
	}
	commit("add view v1")

	// --- redefine the view ---
	if err := db.CollMod(ctx, &backends.CollModParams{
		Name: "v1", SetView: true, ViewOn: "col", ViewPipeline: matchOnPipeline(t, "status", "inactive"),
	}); err != nil {
		t.Fatalf("CollMod(view): %v", err)
	}
	if st := status(); !viewHasStatus(st.Views, "v1", "modified") {
		t.Fatalf("redefined view not in status.Views: %+v", st.Views)
	}
	if vs := diffViews(); len(vs) != 1 || vs[0].Status != "modified" || vs[0].From == nil || vs[0].To == nil {
		t.Fatalf("modified view diff wrong: %+v", vs)
	}
	commit("redefine view v1")

	// --- drop the view ---
	if err := db.DropCollection(ctx, &backends.DropCollectionParams{Name: "v1"}); err != nil {
		t.Fatalf("DropCollection(view): %v", err)
	}
	if st := status(); !viewHasStatus(st.Views, "v1", "deleted") {
		t.Fatalf("dropped view not in status.Views: %+v", st.Views)
	}
	if vs := diffViews(); len(vs) != 1 || vs[0].Status != "deleted" || vs[0].From == nil {
		t.Fatalf("deleted view diff wrong: %+v", vs)
	}
}

// TestViewMergeClean verifies a view created on a branch merges cleanly into a
// branch that did not touch it (workspace-z0i.6, non-conflicting case).
func TestViewMergeClean(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()

	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, "vmdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	insertDocForTest(t, ctx, b, "vmdb", 1)
	commitBranch(t, b, "vmdb", "main", "seed base")
	branchFrom(t, b, "vmdb", "main", "feature")

	featDB, err := b.Database("vmdb@feature")
	if err != nil {
		t.Fatalf("Database(feature): %v", err)
	}
	if err := featDB.CreateCollection(ctx, &backends.CreateCollectionParams{
		Name: "fv", ViewOn: "col", ViewPipeline: matchOnPipeline(t, "status", "active"),
	}); err != nil {
		t.Fatalf("CreateCollection(view on feature): %v", err)
	}
	commitBranch(t, b, "vmdb", "feature", "add view fv")

	if _, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: "vmdb", Into: "main", From: "feature",
		Message: "merge feature", Author: "t <t@e>",
	}); err != nil {
		t.Fatalf("DumboDBMerge: %v", err)
	}

	mainDB, err := b.Database("vmdb")
	if err != nil {
		t.Fatalf("Database(main): %v", err)
	}
	res, err := mainDB.ListCollections(ctx, &backends.ListCollectionsParams{Name: "fv"})
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if len(res.Collections) != 1 || !res.Collections[0].IsView || res.Collections[0].ViewOn != "col" {
		t.Fatalf("merged view not present on main: %+v", res.Collections)
	}
}

// viewMatchValue returns the {$match:{status:<v>}} value of the named view's
// single-stage pipeline, for asserting which definition a merge resolved to.
func viewMatchValue(t *testing.T, db backends.Database, name string) string {
	t.Helper()
	res, err := db.ListCollections(context.Background(), &backends.ListCollectionsParams{Name: name})
	if err != nil {
		t.Fatalf("ListCollections(%q): %v", name, err)
	}
	if len(res.Collections) != 1 || res.Collections[0].ViewPipeline == nil {
		t.Fatalf("view %q not found or has no pipeline: %+v", name, res.Collections)
	}
	stage0, _ := res.Collections[0].ViewPipeline.Get(0)
	match, _ := stage0.(*types.Document).Get("$match")
	v, _ := match.(*types.Document).Get("status")
	s, _ := v.(string)
	return s
}

// setupViewConflict builds a db where view "cv" is redefined divergently on
// main (status=pending) and feature (status=inactive), then merges feature into
// main and returns the resulting conflict (the merge is left paused).
func setupViewConflict(t *testing.T, b *Backend, dbName string) backends.Database {
	t.Helper()
	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, dbName, true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	insertDocForTest(t, ctx, b, dbName, 1)

	mainDB, err := b.Database(dbName)
	if err != nil {
		t.Fatalf("Database(main): %v", err)
	}
	if err := mainDB.CreateCollection(ctx, &backends.CreateCollectionParams{
		Name: "cv", ViewOn: "col", ViewPipeline: matchOnPipeline(t, "status", "active"),
	}); err != nil {
		t.Fatalf("CreateCollection(view): %v", err)
	}
	commitBranch(t, b, dbName, "main", "seed base + view cv")
	branchFrom(t, b, dbName, "main", "feature")

	featDB, err := b.Database(dbName + "@feature")
	if err != nil {
		t.Fatalf("Database(feature): %v", err)
	}
	if err := featDB.CollMod(ctx, &backends.CollModParams{
		Name: "cv", SetView: true, ViewOn: "col", ViewPipeline: matchOnPipeline(t, "status", "inactive"),
	}); err != nil {
		t.Fatalf("CollMod(feature): %v", err)
	}
	commitBranch(t, b, dbName, "feature", "redefine cv on feature")

	if err := mainDB.CollMod(ctx, &backends.CollModParams{
		Name: "cv", SetView: true, ViewOn: "col", ViewPipeline: matchOnPipeline(t, "status", "pending"),
	}); err != nil {
		t.Fatalf("CollMod(main): %v", err)
	}
	commitBranch(t, b, dbName, "main", "redefine cv on main")

	if _, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: dbName, Into: "main", From: "feature", Message: "merge", Author: "t <t@e>",
	}); err == nil {
		t.Fatal("DumboDBMerge: expected a view merge conflict, got nil")
	}
	return mainDB
}

// TestViewMergeConflictResolveTheirs verifies the doltConflicts /
// doltResolveConflict("theirs") / continue flow for a view-definition conflict
// (workspace-z0i.6).
func TestViewMergeConflictResolveTheirs(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()
	ctx := context.Background()

	mainDB := setupViewConflict(t, b, "vcdb")

	confl, err := b.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: "vcdb", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBConflicts: %v", err)
	}
	if len(confl.Views) != 1 || confl.Views[0].Name != "cv" || confl.Views[0].ConflictID == "" {
		t.Fatalf("expected one view conflict for cv with a hash id, got %+v", confl.Views)
	}
	if confl.Views[0].Ours == nil || confl.Views[0].Theirs == nil {
		t.Fatalf("view conflict missing ours/theirs: %+v", confl.Views[0])
	}

	if _, err := b.DumboDBResolveConflict(ctx, &backends.ResolveConflictParams{
		DBName: "vcdb", Branch: "main", Collection: "cv", ConflictID: confl.Views[0].ConflictID, Resolution: "theirs",
	}); err != nil {
		t.Fatalf("DumboDBResolveConflict(theirs): %v", err)
	}
	if _, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: "vcdb", Into: "main", Continue: true, Message: "merge", Author: "t <t@e>",
	}); err != nil {
		t.Fatalf("DumboDBMerge continue: %v", err)
	}
	if got := viewMatchValue(t, mainDB, "cv"); got != "inactive" {
		t.Fatalf("after resolving theirs, view cv match = %q, want %q", got, "inactive")
	}
}

// TestViewMergeConflictResolveCustom verifies a custom resolution replaces the
// view definition with a supplied one.
func TestViewMergeConflictResolveCustom(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()
	ctx := context.Background()

	mainDB := setupViewConflict(t, b, "vcdb")

	confl, err := b.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: "vcdb", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBConflicts: %v", err)
	}
	if len(confl.Views) != 1 {
		t.Fatalf("expected one view conflict, got %+v", confl.Views)
	}

	custom := must.NotFail(types.NewDocument(
		"viewOn", "col",
		"pipeline", matchOnPipeline(t, "status", "custom"),
	))
	if _, err := b.DumboDBResolveConflict(ctx, &backends.ResolveConflictParams{
		DBName: "vcdb", Branch: "main", Collection: "cv", ConflictID: confl.Views[0].ConflictID,
		Resolution: "custom", Value: custom,
	}); err != nil {
		t.Fatalf("DumboDBResolveConflict(custom): %v", err)
	}
	if _, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: "vcdb", Into: "main", Continue: true, Message: "merge", Author: "t <t@e>",
	}); err != nil {
		t.Fatalf("DumboDBMerge continue: %v", err)
	}
	if got := viewMatchValue(t, mainDB, "cv"); got != "custom" {
		t.Fatalf("after custom resolution, view cv match = %q, want %q", got, "custom")
	}
}

// viewHasStatus reports whether views contains an entry for name with the given
// status.
func viewHasStatus(views []backends.ViewStatus, name, status string) bool {
	for _, v := range views {
		if v.Name == name && v.Status == status {
			return true
		}
	}
	return false
}

// TestViewLogStatAndPatch verifies dumboLog surfaces view changes: stat as a
// {name, status} summary (ViewStat) and patch as a full definition diff
// (ViewDiff), parallel to the collection stat/diff (workspace-z0i.7).
func TestViewLogStatAndPatch(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()
	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, "vldb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	insertDocForTest(t, ctx, b, "vldb", 1) // base collection "col"
	commitBranch(t, b, "vldb", "main", "seed")

	db, err := b.Database("vldb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	if err := db.CreateCollection(ctx, &backends.CreateCollectionParams{
		Name: "v1", ViewOn: "col", ViewPipeline: matchOnPipeline(t, "status", "active"),
	}); err != nil {
		t.Fatalf("CreateCollection(view): %v", err)
	}
	commitBranch(t, b, "vldb", "main", "add view v1")

	statRes, err := b.DumboDBLog(ctx, &backends.LogParams{DBName: "vldb", Branch: "main", Stat: true, Limit: 1})
	if err != nil {
		t.Fatalf("DumboDBLog(stat): %v", err)
	}
	if len(statRes.Commits) != 1 || !viewHasStatus(statRes.Commits[0].ViewStat, "v1", "added") {
		t.Fatalf("view add not in log ViewStat: %+v", statRes.Commits)
	}

	patchRes, err := b.DumboDBLog(ctx, &backends.LogParams{DBName: "vldb", Branch: "main", Patch: true, Limit: 1})
	if err != nil {
		t.Fatalf("DumboDBLog(patch): %v", err)
	}
	vd := patchRes.Commits[0].ViewDiff
	if len(vd) != 1 || vd[0].Name != "v1" || vd[0].Status != "added" || vd[0].To == nil || vd[0].To.ViewOn != "col" {
		t.Fatalf("view add not in log ViewDiff: %+v", vd)
	}
}
