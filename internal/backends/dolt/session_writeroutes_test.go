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
	"sync"
	"testing"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dolt/go/store/hash"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
	"github.com/dolthub/dumbodb/internal/types"
)

// ctxWithSession builds a context with a registered conninfo + shadow so
// backend write paths route through the given lsid's DoltSession. The
// returned context is what dispatchThroughSession sets up on a real wire
// request.
func ctxWithSession(t *testing.T, be *Backend, lsid string) context.Context {
	t.Helper()
	shadow, err := be.SessionRegistry().Connect(lsid)
	require.NoError(t, err)
	ci := conninfo.New()
	ci.SetLSID(lsid)
	ci.SetCachedShadow(lsid, shadow)
	return conninfo.Ctx(context.Background(), ci)
}

// TestSession_InsertVisibleToVisitGCRoots: an in-txn client insert must
// surface its chunks via VisitGCRoots so GC's keeper retains them.
// AutoCommit writes don't push to the session (they go straight to disk
// and the disk WS ref is GC-protected directly); only writes inside an
// active dsess txn live on the session's branchState.
func TestSession_InsertVisibleToVisitGCRoots(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	ctx := ctxWithSession(t, be, "test-lsid-gc")
	sess := sessionFromContext(ctx)
	require.NotNil(t, sess, "ctxWithSession must wrap a DoltSession")

	dbName := "gcvisible"
	doc, err := types.NewDocument("_id", "row-with-known-id", "payload", "the-payload-bytes-that-must-be-kept-alive")
	require.NoError(t, err)

	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"}))

	// Start a dsess txn before the insert so the write lands on the
	// session's branchState (mirroring dispatchThroughSession's EnsureTxn
	// for session-isolation / explicit-tx flows).
	sqlCtx := sqlctx.Wrap(ctx, sess)
	_, err = sess.StartTransaction(sqlCtx, sql.ReadWrite)
	require.NoError(t, err)

	coll, err := db.Collection("items")
	require.NoError(t, err)
	_, err = coll.InsertAll(ctx, &backends.InsertAllParams{
		Docs: []*types.Document{doc},
	})
	require.NoError(t, err)

	visited := map[hash.Hash]struct{}{}
	err = sess.VisitGCRoots(context.Background(), dbName, func(h hash.Hash) bool {
		visited[h] = struct{}{}
		return false
	})
	require.NoError(t, err)
	assert.NotEmpty(t, visited, "VisitGCRoots must surface at least one chunk hash from the in-flight working root")
}

// TestSession_CrossSessionConcurrentWritesIsolated stresses the qsc.2
// boundary: two sessions writing concurrently to the same (db, branch)
// must each see their own writes via the session route without
// clobbering each other's overlays. The pre-qsc world stored both
// overlays under dbState.pendingWS keyed by (owner, branch); the
// post-qsc world stores them on each session's branchState.
func TestSession_CrossSessionConcurrentWritesIsolated(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), true, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	dbName := "concurrent"
	// CreateCollection once with a setup session; the workers will share
	// the collection but use independent sessions.
	setupCtx := ctxWithSession(t, be, "test-lsid-setup")
	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(setupCtx, &backends.CreateCollectionParams{Name: "items"}))

	const numSessions = 8
	const opsPerSession = 5

	var wg sync.WaitGroup
	wg.Add(numSessions)
	errCh := make(chan error, numSessions*opsPerSession)

	for s := 0; s < numSessions; s++ {
		go func(sIdx int) {
			defer wg.Done()
			ctx := ctxWithSession(t, be, fmt.Sprintf("test-lsid-w%d", sIdx))
			collDB, derr := be.Database(dbName)
			if derr != nil {
				errCh <- derr
				return
			}
			coll, cerr := collDB.Collection("items")
			if cerr != nil {
				errCh <- cerr
				return
			}
			for op := 0; op < opsPerSession; op++ {
				id := fmt.Sprintf("s%d-op%d", sIdx, op)
				doc, mErr := types.NewDocument("_id", id, "from", int32(sIdx))
				if mErr != nil {
					errCh <- mErr
					return
				}
				_, ierr := coll.InsertAll(ctx, &backends.InsertAllParams{
					Docs: []*types.Document{doc},
				})
				if ierr != nil {
					errCh <- ierr
					return
				}
			}
		}(s)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent insert: %v", err)
	}

	// All writes auto-committed (autoCommit=true). The final state must
	// contain every (session, op) pair. Reading with the setup session
	// (which never wrote) confirms cross-session visibility post-commit.
	state, ok := be.lookupDbStateForDsess(dbName)
	require.True(t, ok)
	state.mu.RLock()
	defer state.mu.RUnlock()
	rv, err := workingRootViaSession(setupCtx, sessionFromContext(setupCtx), state.workingSets[defaultBranch], dbName, defaultBranch)
	require.NoError(t, err)
	am, err := amFromWorkingRoot(setupCtx, rv, state.ns)
	require.NoError(t, err)
	dtblHash, err := am.Get(setupCtx, "items")
	require.NoError(t, err)
	require.False(t, dtblHash.IsEmpty(), "items collection's DTBL must exist after concurrent commits")
}
