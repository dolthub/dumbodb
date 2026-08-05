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
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

func matchStagePipeline(t *testing.T) *types.Array {
	t.Helper()
	match, err := types.NewDocument("status", "active")
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

func findView(t *testing.T, db backends.Database, name string) backends.CollectionInfo {
	t.Helper()
	res, err := db.ListCollections(context.Background(), &backends.ListCollectionsParams{Name: name})
	if err != nil {
		t.Fatalf("ListCollections(%q): %v", name, err)
	}
	if len(res.Collections) != 1 {
		t.Fatalf("ListCollections(%q): got %d entries, want 1", name, len(res.Collections))
	}
	return res.Collections[0]
}

// A view definition survives close/reopen -- stored in the collections
// AddressMap as a blob, not an in-memory map (workspace-z0i.1).
func TestViewDurableAcrossRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-view-durable-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	logger := slog.Default()

	b1, err := NewBackend(dir, logger, false, false, 0, 0)
	if err != nil {
		t.Fatalf("NewBackend (open): %v", err)
	}
	db1, err := b1.Database("viewdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	if err := db1.CreateCollection(ctx, &backends.CreateCollectionParams{
		Name:         "myview",
		ViewOn:       "src",
		ViewPipeline: matchStagePipeline(t),
	}); err != nil {
		t.Fatalf("CreateCollection(view): %v", err)
	}
	b1.Close()

	b2, err := NewBackend(dir, logger, false, false, 0, 0)
	if err != nil {
		t.Fatalf("NewBackend (reopen): %v", err)
	}
	defer b2.Close()
	db2, err := b2.Database("viewdb")
	if err != nil {
		t.Fatalf("Database (reopen): %v", err)
	}

	ci := findView(t, db2, "myview")
	if !ci.IsView {
		t.Fatal("reopened entry is not reported as a view")
	}
	if ci.ViewOn != "src" {
		t.Errorf("ViewOn = %q, want %q", ci.ViewOn, "src")
	}
	if ci.ViewPipeline == nil || ci.ViewPipeline.Len() != 1 {
		t.Fatalf("ViewPipeline not round-tripped: %+v", ci.ViewPipeline)
	}
}

func TestViewDropDurable(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-view-drop-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	logger := slog.Default()

	b1, err := NewBackend(dir, logger, false, false, 0, 0)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	db1, err := b1.Database("viewdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	if err := db1.CreateCollection(ctx, &backends.CreateCollectionParams{
		Name: "v", ViewOn: "src", ViewPipeline: matchStagePipeline(t),
	}); err != nil {
		t.Fatalf("CreateCollection(view): %v", err)
	}
	if err := db1.DropCollection(ctx, &backends.DropCollectionParams{Name: "v"}); err != nil {
		t.Fatalf("DropCollection(view): %v", err)
	}
	b1.Close()

	b2, err := NewBackend(dir, logger, false, false, 0, 0)
	if err != nil {
		t.Fatalf("NewBackend (reopen): %v", err)
	}
	defer b2.Close()
	db2, err := b2.Database("viewdb")
	if err != nil {
		t.Fatalf("Database (reopen): %v", err)
	}
	res, err := db2.ListCollections(ctx, &backends.ListCollectionsParams{Name: "v"})
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if len(res.Collections) != 0 {
		t.Fatalf("dropped view reappeared after reopen: %+v", res.Collections)
	}
}

// Version-control walks must skip a view blob sharing the collections AddressMap
// rather than open it as a document map (workspace-z0i.7).
func TestVersionControlIgnoresViews(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-view-vc-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	b := &Backend{
		dataDir: dir,
		l:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		dbs:     make(map[string]*dbState),
	}
	defer b.Close()

	if _, err := b.getOrOpenDB(ctx, "viewdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	insertDocForTest(t, ctx, b, "viewdb", 1)

	db, err := b.Database("viewdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	if err := db.CreateCollection(ctx, &backends.CreateCollectionParams{
		Name: "myview", ViewOn: "col", ViewPipeline: matchStagePipeline(t),
	}); err != nil {
		t.Fatalf("CreateCollection(view): %v", err)
	}

	status, err := b.DumboDBStatus(ctx, &backends.VersioningStatusParams{DBName: "viewdb", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBStatus with a view present: %v", err)
	}
	for _, ts := range status.Tables {
		if ts.Name == "myview" {
			t.Errorf("view surfaced in status (not yet supported): %+v", ts)
		}
	}

	diff, err := b.DumboDBDiff(ctx, &backends.DiffParams{DBName: "viewdb", ConnRootish: "main"})
	if err != nil {
		t.Fatalf("DumboDBDiff with a view present: %v", err)
	}
	for _, cd := range diff.Collections {
		if cd.Name == "myview" {
			t.Errorf("view surfaced in diff (not yet supported): %+v", cd)
		}
	}
}
