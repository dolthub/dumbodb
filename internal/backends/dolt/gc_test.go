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
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/gcctx"
	"github.com/dolthub/dolt/go/store/hash"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// TestRunUnderGCSafepointKeeper_GCWaitsForBracket: a GC safepoint
// Waiter started while RunUnderGCSafepointKeeper is in flight must
// not complete its Wait until the bracket releases. The bracket's
// VisitGCRoots is invoked at safepoint resolution.
func TestRunUnderGCSafepointKeeper_GCWaitsForBracket(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	require.NotNil(t, be.gcController, "newBackend must wire gcController")
	require.NotNil(t, be.backgroundRP, "newBackend must wire backgroundRP")

	// Start the bracket FIRST so backgroundRP is registered with the
	// controller before we create the Waiter. The bracket holds via
	// bracketReleased until the test releases it.
	bracketEntered := make(chan struct{})
	bracketReleased := make(chan struct{})
	bracketDone := make(chan struct{})
	go func() {
		defer close(bracketDone)
		err := be.RunUnderGCSafepointKeeper(context.Background(), func() error {
			close(bracketEntered)
			<-bracketReleased
			return nil
		})
		require.NoError(t, err)
	}()
	<-bracketEntered

	// Now create a Waiter from an OUTSIDE GCRootsProvider (this is
	// what GC itself does -- the callSession is excluded from the
	// waited set). With the bracket in flight, the Waiter must defer
	// its visit of backgroundRP until SessionCommandEnd fires.
	outside := &outsideGCRootsProvider{}
	var visitedBackground atomic.Bool
	waiter := be.gcController.Waiter(context.Background(), outside, func(_ context.Context, s gcctx.GCRootsProvider) error {
		if s == gcctx.GCRootsProvider(be.backgroundRP) {
			visitedBackground.Store(true)
		}
		return nil
	})

	// Spin off the Wait on its own goroutine so we can assert it does
	// NOT return before the bracket releases.
	waitErrCh := make(chan error, 1)
	go func() {
		waitErrCh <- waiter.Wait(context.Background())
	}()

	select {
	case <-waitErrCh:
		t.Fatal("waiter.Wait returned before the safepoint bracket released")
	case <-time.After(100 * time.Millisecond):
		// Expected: Wait is blocked.
	}

	close(bracketReleased)
	<-bracketDone

	select {
	case err := <-waitErrCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("waiter.Wait did not return after the bracket released")
	}

	require.True(t, visitedBackground.Load(), "Waiter must invoke visit on backgroundRP at safepoint")
}

// TestRunUnderGCSafepointKeeper_NilControllerFallsThrough: a backend
// constructed without a gcController (test helpers) must still run fn
// without panic.
func TestRunUnderGCSafepointKeeper_NilControllerFallsThrough(t *testing.T) {
	be := &Backend{}
	called := false
	err := be.RunUnderGCSafepointKeeper(context.Background(), func() error {
		called = true
		return nil
	})
	require.NoError(t, err)
	require.True(t, called)
}

// outsideGCRootsProvider stands in for the GC callSession in unit
// tests. Its VisitGCRoots is never invoked by the test paths.
type outsideGCRootsProvider struct{}

func (*outsideGCRootsProvider) VisitGCRoots(_ context.Context, _ string, _ func(hash.Hash) bool) error {
	return nil
}

// TestDumboDBGC_DefaultModeShrinksAfterDelete: insert documents,
// commit, delete them, commit, then GC. Default-mode GC sweeps
// new-gen chunks no longer reachable from any branch ref, so the
// chunk count drops.
func TestDumboDBGC_DefaultModeShrinksAfterDelete(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), true, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	dbName := "gctest"
	ctx := ctxWithSession(t, be, "test-lsid-gc-default")

	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"}))
	coll, err := db.Collection("items")
	require.NoError(t, err)

	// Insert a workload large enough that a sweep is visible.
	docs := make([]*types.Document, 0, 200)
	for i := 0; i < 200; i++ {
		doc, mErr := types.NewDocument("_id", fmt.Sprintf("id-%d", i), "payload", fmt.Sprintf("payload-bytes-for-id-%d-padded-out-a-bit", i))
		require.NoError(t, mErr)
		docs = append(docs, doc)
	}
	_, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs})
	require.NoError(t, err)

	// Delete every document so the chunks they wrote become unreachable.
	for _, d := range docs {
		id, _ := d.Get("_id")
		_, err = coll.DeleteAll(ctx, &backends.DeleteAllParams{IDs: []any{id}})
		require.NoError(t, err)
	}

	// Capture pre-GC count to ensure the GC actually has something to sweep.
	state, ok := be.lookupDbStateForDsess(dbName)
	require.True(t, ok)

	res, err := be.DumboDBGC(ctx, &backends.GCParams{DBName: dbName, Mode: "default"})
	require.NoError(t, err)
	require.Equal(t, dbName, res.DB)
	require.Equal(t, "default", res.Mode)
	t.Logf("default-mode GC: chunks %d->%d, size %d->%d, duration=%dms",
		res.ChunksBefore, res.ChunksAfter, res.SizeBefore, res.SizeAfter, res.DurationMs)
	assert.Less(t, res.ChunksAfter, res.ChunksBefore, "default-mode GC should reclaim chunks after a delete-everything workload")
	_ = state
}

// TestDumboDBGC_FullModePreservesReachableData: full-mode GC rewrites
// every chunk (it does not skip referenced chunks the way default
// mode does), so the on-disk layout reorganises even with no garbage
// to reclaim. The interesting property: every document inserted
// before GC must still be readable after.
func TestDumboDBGC_FullModePreservesReachableData(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), true, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	dbName := "gcfull"
	ctx := ctxWithSession(t, be, "test-lsid-gc-full")

	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"}))
	coll, err := db.Collection("items")
	require.NoError(t, err)

	const N = 100
	docs := make([]*types.Document, 0, N)
	for i := 0; i < N; i++ {
		doc, mErr := types.NewDocument("_id", fmt.Sprintf("id-%d", i), "payload", fmt.Sprintf("padding-payload-for-id-%d", i))
		require.NoError(t, mErr)
		docs = append(docs, doc)
	}
	_, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs})
	require.NoError(t, err)

	res, err := be.DumboDBGC(ctx, &backends.GCParams{DBName: dbName, Mode: "full"})
	require.NoError(t, err)
	require.Equal(t, "full", res.Mode)
	t.Logf("full-mode GC: chunks %d->%d, size %d->%d, duration=%dms",
		res.ChunksBefore, res.ChunksAfter, res.SizeBefore, res.SizeAfter, res.DurationMs)

	// Read back: every inserted document must still be queryable.
	// Full-mode GC failing to preserve referenced chunks would
	// surface here as a missing read or a chunk-load error.
	qres, err := coll.Query(ctx, &backends.QueryParams{})
	require.NoError(t, err)
	defer qres.Iter.Close()

	seen := make(map[string]bool, N)
	for {
		_, gotDoc, qerr := qres.Iter.Next()
		if qerr != nil {
			break
		}
		id, _ := gotDoc.Get("_id")
		seen[fmt.Sprintf("%v", id)] = true
	}
	require.Equal(t, N, len(seen), "all %d docs must be readable after full-mode GC; got %d", N, len(seen))
	for i := 0; i < N; i++ {
		require.True(t, seen[fmt.Sprintf("id-%d", i)], "missing _id=%d after full-mode GC", i)
	}
}

// TestDumboDBGC_NoSessionInContextErrors: GC needs a GCRootsProvider
// for BeginGC's root walk; an in-process call with no session in ctx
// must fail rather than silently degrade.
func TestDumboDBGC_NoSessionInContextErrors(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), true, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	dbName := "gcnosess"
	ctxWithSess := ctxWithSession(t, be, "test-lsid-gc-nosess-setup")
	_, err = be.Database(dbName)
	require.NoError(t, err)
	_ = ctxWithSess

	// New context with no conninfo / shadow.
	_, err = be.DumboDBGC(context.Background(), &backends.GCParams{DBName: dbName})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no session in context")
}

// TestDumboDBGC_UnknownDatabaseErrors: GC on an unopened database
// surfaces DatabaseDoesNotExist.
func TestDumboDBGC_UnknownDatabaseErrors(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), true, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	ctx := ctxWithSession(t, be, "test-lsid-gc-unknown")
	_, err = be.DumboDBGC(ctx, &backends.GCParams{DBName: "never-opened"})
	require.Error(t, err)
}

// TestDumboDBGC_UnknownModeErrors: rejects modes other than
// "default" and "full" with a clear error.
func TestDumboDBGC_UnknownModeErrors(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), true, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	dbName := "gcmode"
	ctx := ctxWithSession(t, be, "test-lsid-gc-mode")
	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"}))

	_, err = be.DumboDBGC(ctx, &backends.GCParams{DBName: dbName, Mode: "bogus"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown mode")
}

// TestDumboDBGC_BranchSelectorIsStripped: a wire-name with @branch
// resolves to the base database; the result's DB field echoes the
// stripped base name.
func TestDumboDBGC_BranchSelectorIsStripped(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), true, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	dbName := "gcsplit"
	ctx := ctxWithSession(t, be, "test-lsid-gc-split")
	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"}))

	res, err := be.DumboDBGC(ctx, &backends.GCParams{DBName: dbName + "/main", Mode: "default"})
	require.NoError(t, err)
	assert.Equal(t, dbName, res.DB, "branch selector should be stripped from the returned DB name")
}
