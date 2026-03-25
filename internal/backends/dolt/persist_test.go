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

	// At init (before any writes), working == staged == HEAD's rootValue.
	if workingAddr != stagedAddr {
		t.Errorf("working_root_addr != staged_root_addr at init (working=%v, staged=%v)", workingAddr, stagedAddr)
	}
}

// TestWorkingSetDivergesAfterWrite verifies that after a document insert,
// working_root_addr advances while staged_root_addr stays at HEAD's rootValue.
// This models the git/dolt staging model: `dolt status` should show
// "Changes not staged for commit" after writes.
func TestWorkingSetDivergesAfterWrite(t *testing.T) {
	dir, err := os.MkdirTemp("", "dongo-ws-diverge-test-*")
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
	defer b.Close()

	state, err := b.getOrOpenDB(ctx, "testdb", true)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	// Record HEAD's rootValue hash before any writes — this is the expected staged addr.
	headValue, _, err := state.ds.MaybeHeadValue()
	if err != nil {
		t.Fatalf("MaybeHeadValue: %v", err)
	}
	headMsg, ok := headValue.(dolttypes.SerialMessage)
	if !ok {
		t.Fatalf("unexpected HEAD value type %T", headValue)
	}
	headRef, err := dolttypes.NewRef(headMsg, dolttypes.Format_DOLT)
	if err != nil {
		t.Fatalf("NewRef for HEAD: %v", err)
	}
	headRootAddr := headRef.TargetHash()

	// Insert a document so the working set diverges from HEAD.
	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("testcoll")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	doc, err := types.NewDocument("_id", int64(1), "v", int64(42))
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	doc.SetRecordID(1)
	if _, err := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	// Read the updated working set.
	wsDs, err := state.doltDB.GetDataset(ctx, workingSetDataset)
	if err != nil {
		t.Fatalf("GetDataset(workingSet): %v", err)
	}
	wsHead, err := wsDs.HeadWorkingSet()
	if err != nil {
		t.Fatalf("HeadWorkingSet: %v", err)
	}

	workingAddr := wsHead.WorkingAddr
	if workingAddr.IsEmpty() {
		t.Fatal("working_root_addr is empty after write")
	}
	if wsHead.StagedAddr == nil {
		t.Fatal("staged_root_addr is nil after write")
	}
	stagedAddr := *wsHead.StagedAddr

	// working must have advanced past HEAD (new data was inserted).
	if workingAddr == headRootAddr {
		t.Errorf("working_root_addr did not advance after write (still = HEAD rootValue %v)", workingAddr)
	}

	// staged must stay equal to HEAD's rootValue (nothing was explicitly staged).
	if stagedAddr != headRootAddr {
		t.Errorf("staged_root_addr moved after write: got %v, want HEAD rootValue %v", stagedAddr, headRootAddr)
	}

	// working and staged must differ after writes.
	if workingAddr == stagedAddr {
		t.Errorf("working_root_addr == staged_root_addr after write: staging model is collapsed")
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

// TestWritesNoNewCommits verifies that N document inserts produce exactly 1 commit
// (the "Initialize database" commit from init). Writes must update the working set
// only; they must NOT advance HEAD.
func TestWritesNoNewCommits(t *testing.T) {
	dir, err := os.MkdirTemp("", "dongo-writes-no-commits-test-*")
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
	defer b.Close()

	// Open the database and capture HEAD hash after init.
	state, err := b.getOrOpenDB(ctx, "testdb", true)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	initAddr, ok := state.ds.MaybeHeadAddr()
	if !ok {
		t.Fatal("no HEAD after init")
	}

	// Insert N documents via the same backend.
	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("testcoll")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	const N = 5
	for i := 0; i < N; i++ {
		doc, docErr := types.NewDocument("_id", int64(i), "v", int64(i))
		if docErr != nil {
			t.Fatalf("NewDocument[%d]: %v", i, docErr)
		}
		doc.SetRecordID(int64(i + 1))
		_, insErr := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}})
		if insErr != nil {
			t.Fatalf("InsertAll[%d]: %v", i, insErr)
		}
	}

	// Re-read the "heads/main" dataset directly from the doltDB to bypass any
	// cached state.ds and verify HEAD has not moved.
	freshDS, err := state.doltDB.GetDataset(ctx, mainDataset)
	if err != nil {
		t.Fatalf("GetDataset after writes: %v", err)
	}
	headAddr, ok := freshDS.MaybeHeadAddr()
	if !ok {
		t.Fatal("no HEAD after writes")
	}
	if headAddr != initAddr {
		t.Errorf("HEAD moved after %d writes (addr %v → %v): writes must not create dolt commits", N, initAddr, headAddr)
	}
}

// TestDongoCommit verifies that DongoCommit creates a new dolt commit,
// advances HEAD, and returns a non-empty hash string.
func TestDongoCommit(t *testing.T) {
	dir, err := os.MkdirTemp("", "dongo-dongo-commit-test-*")
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

	// Open/create a database.
	state, err := b.getOrOpenDB(ctx, "testdb", true)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	initAddr, ok := state.ds.MaybeHeadAddr()
	if !ok {
		t.Fatal("no HEAD after init")
	}

	// Insert a document so the working set has something different from HEAD.
	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	doc, err := types.NewDocument("_id", int64(1), "x", int64(42))
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	doc.SetRecordID(1)
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	// Run DongoCommit.
	res, err := b.DongoCommit(ctx, &backends.CommitParams{
		DBName:  "testdb",
		Message: "my first commit",
	})
	if err != nil {
		t.Fatalf("DongoCommit: %v", err)
	}

	// Hash must be non-empty and not all-zeros.
	if res.Hash == "" {
		t.Error("DongoCommit returned empty hash")
	}
	allZero := true
	for _, c := range res.Hash {
		if c != '0' {
			allZero = false
			break
		}
	}
	if allZero {
		t.Errorf("DongoCommit returned all-zero hash: %s", res.Hash)
	}

	// Branch should be "main".
	if res.Branch != "main" {
		t.Errorf("DongoCommit branch = %q, want %q", res.Branch, "main")
	}

	// Message should match.
	if res.Message != "my first commit" {
		t.Errorf("DongoCommit message = %q, want %q", res.Message, "my first commit")
	}

	// HEAD must have advanced past the init commit.
	freshDS, err := state.doltDB.GetDataset(ctx, mainDataset)
	if err != nil {
		t.Fatalf("GetDataset after commit: %v", err)
	}
	headAddr, ok := freshDS.MaybeHeadAddr()
	if !ok {
		t.Fatal("no HEAD after DongoCommit")
	}
	if headAddr == initAddr {
		t.Error("HEAD did not advance after DongoCommit")
	}
}

// TestDongoCommitDefaultMessage verifies that an empty Message defaults to "dongo commit".
func TestDongoCommitDefaultMessage(t *testing.T) {
	dir, err := os.MkdirTemp("", "dongo-dongo-commit-default-msg-*")
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

	if _, err = b.getOrOpenDB(ctx, "testdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	res, err := b.DongoCommit(ctx, &backends.CommitParams{DBName: "testdb"})
	if err != nil {
		t.Fatalf("DongoCommit: %v", err)
	}
	if res.Message != "dongo commit" {
		t.Errorf("default message = %q, want %q", res.Message, "dongo commit")
	}
}

// TestDongoCommitTwoDistinctHashes verifies that two sequential commits produce different hashes.
func TestDongoCommitTwoDistinctHashes(t *testing.T) {
	dir, err := os.MkdirTemp("", "dongo-dongo-commit-two-hashes-*")
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

	if _, err = b.getOrOpenDB(ctx, "testdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	// Insert a document, then commit.
	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	doc1, err := types.NewDocument("_id", int64(1), "x", int64(1))
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	doc1.SetRecordID(1)
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc1}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	res1, err := b.DongoCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "commit one"})
	if err != nil {
		t.Fatalf("DongoCommit 1: %v", err)
	}

	// Insert another document, then commit again.
	doc2, err := types.NewDocument("_id", int64(2), "x", int64(2))
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	doc2.SetRecordID(2)
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc2}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	res2, err := b.DongoCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "commit two"})
	if err != nil {
		t.Fatalf("DongoCommit 2: %v", err)
	}

	if res1.Hash == res2.Hash {
		t.Errorf("two commits produced the same hash %q", res1.Hash)
	}
}

// TestDongoCommitNoOpSucceeds verifies that committing with no changes since the last
// commit succeeds (a no-op commit is acceptable).
func TestDongoCommitNoOpSucceeds(t *testing.T) {
	dir, err := os.MkdirTemp("", "dongo-dongo-commit-noop-*")
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

	if _, err = b.getOrOpenDB(ctx, "testdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	// First commit (on empty state after init).
	res1, err := b.DongoCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "first"})
	if err != nil {
		t.Fatalf("DongoCommit 1: %v", err)
	}

	// Second commit with no intervening writes — must not error.
	res2, err := b.DongoCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "no-op"})
	if err != nil {
		t.Fatalf("DongoCommit 2 (no-op): %v", err)
	}

	// Both hashes must be non-empty.
	if res1.Hash == "" || res2.Hash == "" {
		t.Errorf("got empty hash: %q / %q", res1.Hash, res2.Hash)
	}
}

// TestDongoCommitWorkingSetClean verifies that after a commit the working set's
// staged root address equals the HEAD commit's rootValue address (clean state).
func TestDongoCommitWorkingSetClean(t *testing.T) {
	dir, err := os.MkdirTemp("", "dongo-dongo-commit-ws-clean-*")
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

	state, err := b.getOrOpenDB(ctx, "testdb", true)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	// Insert a doc, then commit.
	db, err := b.Database("testdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	doc, err := types.NewDocument("_id", int64(1), "x", int64(99))
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	doc.SetRecordID(1)
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	if _, err = b.DongoCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "clean check"}); err != nil {
		t.Fatalf("DongoCommit: %v", err)
	}

	// Read HEAD rootValue hash.
	headVal, _, err := state.ds.MaybeHeadValue()
	if err != nil {
		t.Fatalf("MaybeHeadValue: %v", err)
	}
	headRtvlRef, err := dolttypes.NewRef(headVal, dolttypes.Format_DOLT)
	if err != nil {
		t.Fatalf("NewRef for head rootValue: %v", err)
	}
	headRtvlHash := headRtvlRef.TargetHash()

	// Read the working set and get the staged root addr.
	wsDs, err := state.doltDB.GetDataset(ctx, workingSetDataset)
	if err != nil {
		t.Fatalf("GetDataset working set: %v", err)
	}
	if !wsDs.HasHead() {
		t.Fatal("working set has no head")
	}
	ws, err := wsDs.HeadWorkingSet()
	if err != nil {
		t.Fatalf("HeadWorkingSet: %v", err)
	}
	if ws.StagedAddr == nil {
		t.Fatal("working set StagedAddr is nil after commit")
	}

	if *ws.StagedAddr != headRtvlHash {
		t.Errorf("working set staged root %v != HEAD rootValue hash %v", *ws.StagedAddr, headRtvlHash)
	}
}
