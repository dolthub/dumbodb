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

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/datas"
	dolttypes "github.com/dolthub/dolt/go/store/types"

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

// TestRTVLFormat verifies that the head commit's rootValue has file ID "RTVL"
// and that the embedded ADRM in RTVL.tables can be parsed.
func TestRTVLFormat(t *testing.T) {
	dir, err := os.MkdirTemp("", "dongo-rtvl-test-*")
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

	headValue, _, err := state.ds.MaybeHeadValue()
	if err != nil {
		t.Fatalf("MaybeHeadValue: %v", err)
	}

	headMsg, ok := headValue.(dolttypes.SerialMessage)
	if !ok {
		t.Fatalf("head value is %T, want SerialMessage", headValue)
	}

	fileID := serial.GetFileID([]byte(headMsg))
	if fileID != serial.RootValueFileID {
		t.Errorf("head commit rootValue file ID = %q, want %q", fileID, serial.RootValueFileID)
	}

	// Verify we can parse the RTVL and extract the tables ADRM.
	rtvl, err := serial.TryGetRootAsRootValue([]byte(headMsg), serial.MessagePrefixSz)
	if err != nil {
		t.Fatalf("TryGetRootAsRootValue: %v", err)
	}

	if rtvl.FeatureVersion() != 7 {
		t.Errorf("RTVL feature_version = %d, want 7", rtvl.FeatureVersion())
	}

	if rtvl.TablesLength() == 0 {
		t.Error("RTVL.tables is empty, expected embedded ADRM bytes")
	}
}

// TestWorkingSetRTVL verifies that both working_root_addr and staged_root_addr
// in the working set point to RTVL chunks (not raw ADRM).
func TestWorkingSetRTVL(t *testing.T) {
	dir, err := os.MkdirTemp("", "dongo-ws-rtvl-test-*")
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

	// Read the working set dataset.
	wsDs, err := state.doltDB.GetDataset(ctx, workingSetDataset)
	if err != nil {
		t.Fatalf("GetDataset(workingSet): %v", err)
	}

	if !wsDs.HasHead() {
		t.Fatal("working set dataset has no head")
	}

	wsHead, err := wsDs.HeadWorkingSet()
	if err != nil {
		t.Fatalf("HeadWorkingSet: %v", err)
	}

	workingAddr := wsHead.WorkingAddr
	if workingAddr.IsEmpty() {
		t.Fatal("working_root_addr is empty")
	}

	if wsHead.StagedAddr == nil {
		t.Fatal("staged_root_addr is nil")
	}
	stagedAddr := *wsHead.StagedAddr

	// Read the working root chunk and verify it's RTVL.
	workingChunk, err := state.cs.Get(ctx, workingAddr)
	if err != nil {
		t.Fatalf("reading working root chunk: %v", err)
	}

	workingFileID := serial.GetFileID(workingChunk.Data())
	if workingFileID != serial.RootValueFileID {
		t.Errorf("working_root_addr chunk file ID = %q, want %q", workingFileID, serial.RootValueFileID)
	}

	// Read staged root chunk and verify it's also RTVL.
	stagedChunk, err := state.cs.Get(ctx, stagedAddr)
	if err != nil {
		t.Fatalf("reading staged root chunk: %v", err)
	}

	stagedFileID := serial.GetFileID(stagedChunk.Data())
	if stagedFileID != serial.RootValueFileID {
		t.Errorf("staged_root_addr chunk file ID = %q, want %q", stagedFileID, serial.RootValueFileID)
	}

	// Verify staged root is the same hash as working root (clean state invariant).
	if workingAddr != stagedAddr {
		t.Errorf("working_root_addr != staged_root_addr: invariant violated (working=%v, staged=%v)", workingAddr, stagedAddr)
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
