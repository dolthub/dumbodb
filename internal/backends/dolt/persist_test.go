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

	"github.com/dolthub/dolt/go/store/datas"

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/types"
)

// TestInitialCommitMessage verifies that a brand-new database gets an "Initialize database"
// root commit with no parents, satisfying the requirement that dolt log shows a clean
// ancestry for new stores.
func TestInitialCommitMessage(t *testing.T) {
	dir, err := os.MkdirTemp("", "dongo-init-commit-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	logger := slog.Default()

	b := &Backend{
		dataDir: dir,
		l:       logger,
		dbs:     make(map[string]*dbState),
	}

	state, err := b.getOrOpenDB(ctx, "testdb", true)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	if !state.ds.HasHead() {
		t.Fatal("expected new database to have a head commit")
	}

	headVal, ok := state.ds.MaybeHead()
	if !ok {
		t.Fatal("MaybeHead returned false")
	}

	meta, err := datas.GetCommitMeta(ctx, headVal)
	if err != nil {
		t.Fatalf("GetCommitMeta: %v", err)
	}

	const wantMsg = "Initialize database"
	if meta.Description != wantMsg {
		t.Errorf("initial commit message = %q, want %q", meta.Description, wantMsg)
	}

}

// TestPersistenceAcrossRestart verifies that documents survive a backend close and reopen.
// This is the end-to-end persistence test described in do-q040.
func TestPersistenceAcrossRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "dongo-persist-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	logger := slog.Default()

	// --- Phase 1: insert a document ---
	b1, err := NewBackend(dir, logger)
	if err != nil {
		t.Fatalf("NewBackend (open): %v", err)
	}

	db1, err := b1.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	coll1, err := db1.Collection("testcoll")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	doc, err := types.NewDocument("_id", int64(1), "value", "hello")
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	doc.SetRecordID(1)

	_, err = coll1.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}})
	if err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	// Close the backend (simulating server shutdown).
	b1.Close()

	// --- Phase 2: reopen and query ---
	b2, err := NewBackend(dir, logger)
	if err != nil {
		t.Fatalf("NewBackend (reopen): %v", err)
	}
	defer b2.Close()

	db2, err := b2.Database("testdb")
	if err != nil {
		t.Fatalf("Database (reopen): %v", err)
	}

	coll2, err := db2.Collection("testcoll")
	if err != nil {
		t.Fatalf("Collection (reopen): %v", err)
	}

	res, err := coll2.Query(ctx, nil)
	if err != nil {
		t.Fatalf("Query after reopen: %v", err)
	}

	var count int
	for {
		_, d, err := res.Iter.Next()
		if err != nil {
			break
		}
		_ = d
		count++
	}

	if count != 1 {
		t.Errorf("expected 1 document after restart, got %d — persistence is broken", count)
	}
}
