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
	"slices"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
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
	if st := status(); !slices.Contains(st.AddedViews, "v1") {
		t.Fatalf("added view not in status.AddedViews: %+v", st.AddedViews)
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
	if st := status(); !slices.Contains(st.ModifiedViews, "v1") {
		t.Fatalf("redefined view not in status.ModifiedViews: %+v", st.ModifiedViews)
	}
	if vs := diffViews(); len(vs) != 1 || vs[0].Status != "modified" || vs[0].From == nil || vs[0].To == nil {
		t.Fatalf("modified view diff wrong: %+v", vs)
	}
	commit("redefine view v1")

	// --- drop the view ---
	if err := db.DropCollection(ctx, &backends.DropCollectionParams{Name: "v1"}); err != nil {
		t.Fatalf("DropCollection(view): %v", err)
	}
	if st := status(); !slices.Contains(st.RemovedViews, "v1") {
		t.Fatalf("dropped view not in status.RemovedViews: %+v", st.RemovedViews)
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

// TestViewMergeConflict verifies that a view redefined divergently on both
// branches fails the merge loudly rather than silently picking a side
// (workspace-z0i.6; interactive resolution is a follow-up).
func TestViewMergeConflict(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()

	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, "vcdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	insertDocForTest(t, ctx, b, "vcdb", 1)

	mainDB, err := b.Database("vcdb")
	if err != nil {
		t.Fatalf("Database(main): %v", err)
	}
	if err := mainDB.CreateCollection(ctx, &backends.CreateCollectionParams{
		Name: "cv", ViewOn: "col", ViewPipeline: matchOnPipeline(t, "status", "active"),
	}); err != nil {
		t.Fatalf("CreateCollection(view): %v", err)
	}
	commitBranch(t, b, "vcdb", "main", "seed base + view cv")
	branchFrom(t, b, "vcdb", "main", "feature")

	// Redefine cv differently on each branch.
	featDB, err := b.Database("vcdb@feature")
	if err != nil {
		t.Fatalf("Database(feature): %v", err)
	}
	if err := featDB.CollMod(ctx, &backends.CollModParams{
		Name: "cv", SetView: true, ViewOn: "col", ViewPipeline: matchOnPipeline(t, "status", "inactive"),
	}); err != nil {
		t.Fatalf("CollMod(feature): %v", err)
	}
	commitBranch(t, b, "vcdb", "feature", "redefine cv on feature")

	if err := mainDB.CollMod(ctx, &backends.CollModParams{
		Name: "cv", SetView: true, ViewOn: "col", ViewPipeline: matchOnPipeline(t, "status", "pending"),
	}); err != nil {
		t.Fatalf("CollMod(main): %v", err)
	}
	commitBranch(t, b, "vcdb", "main", "redefine cv on main")

	_, err = b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: "vcdb", Into: "main", From: "feature",
		Message: "merge feature", Author: "t <t@e>",
	})
	if err == nil {
		t.Fatal("DumboDBMerge: expected a view merge conflict, got nil")
	}
}
