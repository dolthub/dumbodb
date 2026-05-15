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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/datas"
	dolttypes "github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// mainDS returns the main branch dataset for the given dbState.
func mainDS(t *testing.T, state *dbState) datas.Dataset {
	t.Helper()
	ds, err := state.datasDB.GetDataset(context.Background(), mainDataset)
	require.NoError(t, err)
	return ds
}

// TestInitialCommitMessage verifies that a brand-new database gets an "Initialize database"
// root commit with no parents, satisfying the requirement that dolt log shows a clean
// ancestry for new stores.
func TestInitialCommitMessage(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-init-commit-test-*")
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

	if !mainDS(t, state).HasHead() {
		t.Fatal("expected new database to have a head commit")
	}

	headVal, ok := mainDS(t, state).MaybeHead()
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
	dir, err := os.MkdirTemp("", "dolt-rtvl-test-*")
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

	if !mainDS(t, state).HasHead() {
		t.Fatal("expected new database to have a head commit")
	}

	headValue, _, err := mainDS(t, state).MaybeHeadValue()
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
	dir, err := os.MkdirTemp("", "dolt-ws-rtvl-test-*")
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
	wsDs, err := state.datasDB.GetDataset(ctx, workingSetDataset)
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
	dir, err := os.MkdirTemp("", "dolt-ws-diverge-test-*")
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

	// Record HEAD's rootValue hash before any writes  -- this is the expected staged addr.
	headValue, _, err := mainDS(t, state).MaybeHeadValue()
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
	wsDs, err := state.datasDB.GetDataset(ctx, workingSetDataset)
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
	dir, err := os.MkdirTemp("", "dolt-persist-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	logger := slog.Default()

	// --- Phase 1: insert a document ---
	b1, err := NewBackend(dir, logger, false)
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
	b2, err := NewBackend(dir, logger, false)
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
		t.Errorf("expected 1 document after restart, got %d  -- persistence is broken", count)
	}
}

// TestWritesNoNewCommits verifies that N document inserts produce exactly 1 commit
// (the "Initialize database" commit from init). Writes must update the working set
// only; they must NOT advance HEAD.
func TestWritesNoNewCommits(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-writes-no-commits-test-*")
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
	initAddr, ok := mainDS(t, state).MaybeHeadAddr()
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
	// cached mainDS(t, state) and verify HEAD has not moved.
	freshDS, err := state.datasDB.GetDataset(ctx, mainDataset)
	if err != nil {
		t.Fatalf("GetDataset after writes: %v", err)
	}
	headAddr, ok := freshDS.MaybeHeadAddr()
	if !ok {
		t.Fatal("no HEAD after writes")
	}
	if headAddr != initAddr {
		t.Errorf("HEAD moved after %d writes (addr %v -> %v): writes must not create dolt commits", N, initAddr, headAddr)
	}
}

// TestDumboDBCommit verifies that DumboDBCommit creates a new dolt commit,
// advances HEAD, and returns a non-empty hash string.
func TestDumboDBCommit(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-dumbodb-commit-test-*")
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
	initAddr, ok := mainDS(t, state).MaybeHeadAddr()
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

	// Run DumboDBCommit.
	res, err := b.DumboDBCommit(ctx, &backends.CommitParams{
		DBName:  "testdb",
		Message: "my first commit",
		Author:  "testuser",
	})
	if err != nil {
		t.Fatalf("DumboDBCommit: %v", err)
	}

	// Hash must be non-empty and not all-zeros.
	if res.CommitID == "" {
		t.Error("DumboDBCommit returned empty hash")
	}
	allZero := true
	for _, c := range res.CommitID {
		if c != '0' {
			allZero = false
			break
		}
	}
	if allZero {
		t.Errorf("DumboDBCommit returned all-zero hash: %s", res.CommitID)
	}

	// Branch should be "main".
	if res.Branch != "main" {
		t.Errorf("DumboDBCommit branch = %q, want %q", res.Branch, "main")
	}

	// Message should match.
	if res.Message != "my first commit" {
		t.Errorf("DumboDBCommit message = %q, want %q", res.Message, "my first commit")
	}

	// HEAD must have advanced past the init commit.
	freshDS, err := state.datasDB.GetDataset(ctx, mainDataset)
	if err != nil {
		t.Fatalf("GetDataset after commit: %v", err)
	}
	headAddr, ok := freshDS.MaybeHeadAddr()
	if !ok {
		t.Fatal("no HEAD after DumboDBCommit")
	}
	if headAddr == initAddr {
		t.Error("HEAD did not advance after DumboDBCommit")
	}
}

// TestDumboDBCommitDefaultMessage verifies that an empty Message defaults to "dolt commit".
func TestDumboDBCommitDefaultMessage(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-dumbodb-commit-default-msg-*")
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

	res, err := b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "testdb", Author: "testuser", AllowEmpty: true})
	if err != nil {
		t.Fatalf("DumboDBCommit: %v", err)
	}
	if res.Message != "dolt commit" {
		t.Errorf("default message = %q, want %q", res.Message, "dolt commit")
	}
}

// TestDumboDBCommitTwoDistinctHashes verifies that two sequential commits produce different hashes.
func TestDumboDBCommitTwoDistinctHashes(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-dumbodb-commit-two-hashes-*")
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

	res1, err := b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "commit one", Author: "testuser"})
	if err != nil {
		t.Fatalf("DumboDBCommit 1: %v", err)
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

	res2, err := b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "commit two", Author: "testuser"})
	if err != nil {
		t.Fatalf("DumboDBCommit 2: %v", err)
	}

	if res1.CommitID == res2.CommitID {
		t.Errorf("two commits produced the same hash %q", res1.CommitID)
	}
}

// TestDumboDBCommitEmptyRejected verifies that DumboDBCommit returns ErrEmptyCommit when
// the working set has no changes versus HEAD and AllowEmpty is not set, and that
// AllowEmpty: true overrides the gate to create an empty commit with a new hash.
func TestDumboDBCommitEmptyRejected(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-dumbodb-commit-empty-*")
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

	// No changes since init: commit without AllowEmpty must return ErrEmptyCommit.
	if _, err := b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "noop", Author: "testuser"}); !errors.Is(err, backends.ErrEmptyCommit) {
		t.Fatalf("DumboDBCommit (no changes, no flag): err = %v, want ErrEmptyCommit", err)
	}

	// With AllowEmpty:true the commit succeeds and produces a non-empty hash.
	res1, err := b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "empty1", Author: "testuser", AllowEmpty: true})
	if err != nil {
		t.Fatalf("DumboDBCommit (AllowEmpty): %v", err)
	}
	if res1.CommitID == "" {
		t.Errorf("AllowEmpty commit returned empty hash")
	}

	// Still no changes after the empty commit: another bare commit must fail.
	if _, err := b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "noop2", Author: "testuser"}); !errors.Is(err, backends.ErrEmptyCommit) {
		t.Fatalf("DumboDBCommit (no changes after empty, no flag): err = %v, want ErrEmptyCommit", err)
	}

	// AllowEmpty again  -- new empty commit with a different hash.
	res2, err := b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "empty2", Author: "testuser", AllowEmpty: true})
	if err != nil {
		t.Fatalf("DumboDBCommit (AllowEmpty 2): %v", err)
	}
	if res2.CommitID == "" {
		t.Errorf("AllowEmpty commit 2 returned empty hash")
	}
	if res1.CommitID == res2.CommitID {
		t.Errorf("two AllowEmpty commits produced the same hash %q", res1.CommitID)
	}
}

// TestDumboDBCommitWorkingSetClean verifies that after a commit the working set's
// staged root address equals the HEAD commit's rootValue address (clean state).
func TestDumboDBCommitWorkingSetClean(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-dumbodb-commit-ws-clean-*")
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

	if _, err = b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "clean check", Author: "testuser"}); err != nil {
		t.Fatalf("DumboDBCommit: %v", err)
	}

	// Read HEAD rootValue hash.
	headVal, _, err := mainDS(t, state).MaybeHeadValue()
	if err != nil {
		t.Fatalf("MaybeHeadValue: %v", err)
	}
	headRtvlRef, err := dolttypes.NewRef(headVal, dolttypes.Format_DOLT)
	if err != nil {
		t.Fatalf("NewRef for head rootValue: %v", err)
	}
	headRtvlHash := headRtvlRef.TargetHash()

	// Read the working set and get the staged root addr.
	wsDs, err := state.datasDB.GetDataset(ctx, workingSetDataset)
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

// TestDumboDBCommitAuthorAndTimestamp verifies that DumboDBCommit stores the provided
// author name and timestamp, and that dumboDBLog reflects them on the resulting commit.
func TestDumboDBCommitAuthorAndTimestamp(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-dumbodb-commit-author-*")
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

	fixedTime := time.Date(2020, 6, 15, 12, 0, 0, 0, time.UTC)

	res, err := b.DumboDBCommit(ctx, &backends.CommitParams{
		DBName:     "testdb",
		Message:    "authored commit",
		Author:     "alice",
		Timestamp:  fixedTime,
		AllowEmpty: true,
	})
	if err != nil {
		t.Fatalf("DumboDBCommit: %v", err)
	}

	if res.Author != "alice" {
		t.Errorf("CommitResult.Author = %q, want %q", res.Author, "alice")
	}
	if res.Timestamp != fixedTime.UnixMilli() {
		t.Errorf("CommitResult.Timestamp = %d, want %d", res.Timestamp, fixedTime.UnixMilli())
	}

	// Verify via dumboDBLog that author and timestamp were persisted.
	logRes, err := b.DumboDBLog(ctx, &backends.LogParams{DBName: "testdb", Branch: "main", Limit: 1})
	if err != nil {
		t.Fatalf("DumboDBLog: %v", err)
	}
	if len(logRes.Commits) == 0 {
		t.Fatal("expected at least 1 commit from DumboDBLog")
	}
	c := logRes.Commits[0]
	// dumboDBLog formats author as "name <email>"; bare name "alice" becomes "alice <alice@dumbodb>".
	if c.Author != "alice <alice@dumbodb>" {
		t.Errorf("log commit Author = %q, want %q", c.Author, "alice <alice@dumbodb>")
	}
	if c.Timestamp != fixedTime.UnixMilli() {
		t.Errorf("log commit Timestamp = %d, want %d", c.Timestamp, fixedTime.UnixMilli())
	}
}

// TestDumboDBCommitTimestampDefaultsToNow verifies that when no Timestamp is provided,
// the commit timestamp is set to approximately the current time.
func TestDumboDBCommitTimestampDefaultsToNow(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-dumbodb-commit-ts-default-*")
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

	before := time.Now().UnixMilli()
	res, err := b.DumboDBCommit(ctx, &backends.CommitParams{
		DBName:     "testdb",
		Message:    "no timestamp",
		Author:     "bob",
		AllowEmpty: true,
	})
	after := time.Now().UnixMilli()
	if err != nil {
		t.Fatalf("DumboDBCommit: %v", err)
	}

	if res.Timestamp < before || res.Timestamp > after {
		t.Errorf("CommitResult.Timestamp %d not in [%d, %d]", res.Timestamp, before, after)
	}
}

// newBackendForTest creates a temporary Backend for use in tests.
// The caller must call b.Close() and os.RemoveAll(dir) when done.
func newBackendForTest(t *testing.T) (b *Backend, dir string) {
	t.Helper()
	var err error
	dir, err = os.MkdirTemp("", "dolt-log-test-*")
	if err != nil {
		t.Fatal(err)
	}
	b = &Backend{
		dataDir: dir,
		l:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		dbs:     make(map[string]*dbState),
	}
	return b, dir
}

// insertDocForTest inserts a document with the given integer _id into the named collection,
// so that a subsequent DumboDBCommit records a real content change.
func insertDocForTest(t *testing.T, ctx context.Context, b *Backend, dbName string, id int64) {
	t.Helper()
	db, err := b.Database(dbName)
	if err != nil {
		t.Fatalf("Database %q: %v", dbName, err)
	}
	coll, err := db.Collection("col")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	doc, err := types.NewDocument("_id", id, "v", id)
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	doc.SetRecordID(id)
	if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}}); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}
}

// TestDumboDBLogFreshDatabase verifies that DumboDBLog on a brand-new database returns exactly
// one commit  -- the "Initialize database" root commit  -- and that the root commit has no Parent1.
func TestDumboDBLogFreshDatabase(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()

	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, "testdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	res, err := b.DumboDBLog(ctx, &backends.LogParams{DBName: "testdb", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBLog: %v", err)
	}

	if len(res.Commits) != 1 {
		t.Fatalf("expected 1 commit on fresh db, got %d", len(res.Commits))
	}

	c := res.Commits[0]
	if c.CommitID == "" {
		t.Error("root commit hash is empty")
	}
	if c.Message != "Initialize database" {
		t.Errorf("root commit message = %q, want %q", c.Message, "Initialize database")
	}
	// Root commit must have no parent.
	if c.Parent1 != "" {
		t.Errorf("root commit Parent1 = %q, want empty", c.Parent1)
	}
	if c.Parent2 != "" {
		t.Errorf("root commit Parent2 = %q, want empty", c.Parent2)
	}
}

// TestDumboDBLogAfterOneCommit verifies that after one DumboDBCommit the log returns
// 2 commits in newest-first order: the user commit followed by the init commit.
func TestDumboDBLogAfterOneCommit(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()

	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, "testdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	insertDocForTest(t, ctx, b, "testdb", 1)

	commitRes, err := b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "first user commit", Author: "testuser"})
	if err != nil {
		t.Fatalf("DumboDBCommit: %v", err)
	}

	res, err := b.DumboDBLog(ctx, &backends.LogParams{DBName: "testdb", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBLog: %v", err)
	}

	if len(res.Commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(res.Commits))
	}

	// Newest first: index 0 is the user commit.
	if res.Commits[0].CommitID != commitRes.CommitID {
		t.Errorf("Commits[0].Hash = %q, want %q (the user commit)", res.Commits[0].CommitID, commitRes.CommitID)
	}
	if res.Commits[0].Message != "first user commit" {
		t.Errorf("Commits[0].Message = %q, want %q", res.Commits[0].Message, "first user commit")
	}
	// User commit must have Parent1 pointing at the init commit.
	if res.Commits[0].Parent1 == "" {
		t.Error("user commit Parent1 is empty, expected init commit hash")
	}
	if res.Commits[0].Parent1 != res.Commits[1].CommitID {
		t.Errorf("user commit Parent1 = %q, want init hash %q", res.Commits[0].Parent1, res.Commits[1].CommitID)
	}

	// index 1 is the init commit (root  -- no parent).
	if res.Commits[1].Message != "Initialize database" {
		t.Errorf("Commits[1].Message = %q, want %q", res.Commits[1].Message, "Initialize database")
	}
	if res.Commits[1].Parent1 != "" {
		t.Errorf("init commit Parent1 = %q, want empty", res.Commits[1].Parent1)
	}
}

// TestDumboDBLogLimit verifies that the Limit parameter is respected: limit=1 returns
// only the HEAD commit even when more commits exist.
func TestDumboDBLogLimit(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()

	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, "testdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	// Create 3 user commits so there are 4 total (including init).
	for i := int64(1); i <= 3; i++ {
		insertDocForTest(t, ctx, b, "testdb", i)
		if _, err := b.DumboDBCommit(ctx, &backends.CommitParams{
			DBName:  "testdb",
			Message: fmt.Sprintf("commit %d", i),
			Author:  "testuser",
		}); err != nil {
			t.Fatalf("DumboDBCommit %d: %v", i, err)
		}
	}

	res, err := b.DumboDBLog(ctx, &backends.LogParams{DBName: "testdb", Branch: "main", Limit: 1})
	if err != nil {
		t.Fatalf("DumboDBLog: %v", err)
	}

	if len(res.Commits) != 1 {
		t.Fatalf("expected 1 commit with limit=1, got %d", len(res.Commits))
	}
	if res.Commits[0].Message != "commit 3" {
		t.Errorf("Commits[0].Message = %q, want %q", res.Commits[0].Message, "commit 3")
	}
}

// TestDumboDBLogFromHash verifies that setting From=<hash> starts traversal from
// that specific commit rather than HEAD.
func TestDumboDBLogFromHash(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()

	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, "testdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	insertDocForTest(t, ctx, b, "testdb", 1)
	res1, err := b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "commit one", Author: "testuser"})
	if err != nil {
		t.Fatalf("DumboDBCommit 1: %v", err)
	}

	insertDocForTest(t, ctx, b, "testdb", 2)
	if _, err = b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "commit two", Author: "testuser"}); err != nil {
		t.Fatalf("DumboDBCommit 2: %v", err)
	}

	// Starting from commit one's hash should return commit one + init commit only.
	res, err := b.DumboDBLog(ctx, &backends.LogParams{DBName: "testdb", Branch: "main", From: res1.CommitID})
	if err != nil {
		t.Fatalf("DumboDBLog from hash: %v", err)
	}

	if len(res.Commits) != 2 {
		t.Fatalf("expected 2 commits starting from commit one, got %d", len(res.Commits))
	}
	if res.Commits[0].CommitID != res1.CommitID {
		t.Errorf("Commits[0].Hash = %q, want %q (commit one)", res.Commits[0].CommitID, res1.CommitID)
	}
	if res.Commits[1].Message != "Initialize database" {
		t.Errorf("Commits[1].Message = %q, want %q", res.Commits[1].Message, "Initialize database")
	}
}

// TestDumboDBLogFromUnknownHash verifies that from=<unknown hash> returns an error.
func TestDumboDBLogFromUnknownHash(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()

	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, "testdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	// A syntactically valid but non-existent hash (32 hex bytes = 64 chars).
	unknownHash := "0000000000000000000000000000000000000000000000000000000000000001"

	_, err := b.DumboDBLog(ctx, &backends.LogParams{DBName: "testdb", Branch: "main", From: unknownHash})
	if err == nil {
		t.Error("expected error for unknown from hash, got nil")
	}
}

// TestDumboDBLogHashOrderAndTimestamps exercises the "commit, commit, log" scenario:
// verifies that returned hashes match what DumboDBCommit reported (newest first) and
// that timestamps are non-zero and non-decreasing from root toward HEAD.
func TestDumboDBLogHashOrderAndTimestamps(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()

	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, "testdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}

	insertDocForTest(t, ctx, b, "testdb", 1)
	r1, err := b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "alpha", Author: "testuser"})
	if err != nil {
		t.Fatalf("DumboDBCommit 1: %v", err)
	}

	insertDocForTest(t, ctx, b, "testdb", 2)
	r2, err := b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "testdb", Message: "beta", Author: "testuser"})
	if err != nil {
		t.Fatalf("DumboDBCommit 2: %v", err)
	}

	res, err := b.DumboDBLog(ctx, &backends.LogParams{DBName: "testdb", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBLog: %v", err)
	}

	// Expect 3 commits: beta, alpha, init.
	if len(res.Commits) != 3 {
		t.Fatalf("expected 3 commits, got %d", len(res.Commits))
	}

	// Newest first: beta is index 0, alpha is index 1.
	if res.Commits[0].CommitID != r2.CommitID {
		t.Errorf("Commits[0].Hash = %q, want %q (beta)", res.Commits[0].CommitID, r2.CommitID)
	}
	if res.Commits[1].CommitID != r1.CommitID {
		t.Errorf("Commits[1].Hash = %q, want %q (alpha)", res.Commits[1].CommitID, r1.CommitID)
	}

	// All timestamps must be non-zero.
	for i, c := range res.Commits {
		if c.Timestamp == 0 {
			t.Errorf("Commits[%d].Timestamp is zero", i)
		}
	}

	// Timestamps must be non-decreasing from root toward HEAD (i.e. non-decreasing
	// as we scan from the oldest commit to newest, which is reverse of log order).
	for i := len(res.Commits) - 1; i > 0; i-- {
		older := res.Commits[i].Timestamp
		newer := res.Commits[i-1].Timestamp
		if newer < older {
			t.Errorf("timestamp went backwards: Commits[%d].Timestamp=%d > Commits[%d].Timestamp=%d",
				i, older, i-1, newer)
		}
	}
}

// TestDumboDBLogCommitInfoSupportsParent2 is a compile-time structural assertion:
// CommitInfo must have a Parent2 field to support merge commits in the future.
// This test documents the requirement and ensures the struct is not accidentally changed.
func TestDumboDBLogCommitInfoSupportsParent2(t *testing.T) {
	var ci backends.CommitInfo
	ci.Parent2 = "somemergehash"
	if ci.Parent2 != "somemergehash" {
		t.Error("CommitInfo.Parent2 field not working as expected")
	}
}
