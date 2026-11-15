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
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/clientconn/cursor"
	"github.com/dolthub/dumbodb/internal/sqlctx"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// Dropping a database while other connections write to it, commit, read it and
// create it must neither stop the server nor close a store under a command
// still using it.
//
// A writer holds db.mu and the provider holds dbCacheMu while each reaches back
// into b.mu, so a drop that waits for either while holding b.mu freezes every
// request. A commit and most reads use the store without db.mu, so a drop that
// closes the store without waiting for them crashes the server.
//
// Each step runs as a command on its session, as the wire protocol runs it:
// the drop waits for commands in flight, and only a command marks a session
// as busy.
func TestDropDatabaseRacingWritesDoesNotDeadlockOrCrash(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.NoError(t, err)

	const (
		dbName    = "victim"
		writers   = 4
		churners  = 3
		runFor    = 3 * time.Second
		stallTime = 10 * time.Second
	)

	var progress atomic.Int64
	var seq atomic.Int64
	stop := make(chan struct{})

	inCommand := func(fn func(ctx context.Context)) {
		ctx, ci := ctxWithSessionAs(t, be, fmt.Sprintf("drop-race-%d", seq.Add(1)), "")
		shadow, _ := ci.CachedShadow()
		_ = shadow.Use(time.Now(), func(*dsess.DoltSession) error {
			fn(ctx)
			return nil
		})
	}

	insertAndCommit := func(ctx context.Context, name string) {
		sess := sessionFromContext(ctx)
		if _, err := sess.StartTransaction(sqlctx.Wrap(ctx, sess), sql.ReadWrite); err != nil {
			return
		}
		owner := conninfo.Get(ctx).Owner()
		if db, err := be.Database(name); err == nil {
			if coll, err := db.Collection("items"); err == nil {
				doc, _ := types.NewDocument("_id", fmt.Sprintf("doc-%d", seq.Add(1)))
				_, _ = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}})
			}
		}
		if be.OnTransactionCommit(ctx, owner) != nil {
			be.OnTransactionAbort(owner)
		}
	}

	var wg sync.WaitGroup
	loop := func(step func(ctx context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				inCommand(step)
				progress.Add(1)
			}
		}()
	}

	loop(func(ctx context.Context) {
		insertAndCommit(ctx, dbName)
		_ = be.DropDatabase(ctx, &backends.DropDatabaseParams{Name: dbName})
	})
	for range writers {
		loop(func(ctx context.Context) { insertAndCommit(ctx, dbName) })
	}
	loop(func(ctx context.Context) {
		if db, err := be.Database(dbName); err == nil {
			_ = db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"})
		}
		_, _ = be.ListDatabases(ctx, &backends.ListDatabasesParams{})
		_, _ = be.Status(ctx, &backends.StatusParams{})
	})
	// Churners drop databases nobody else writes to, so their drops race
	// session lookups that hold dbCacheMu rather than writers holding db.mu.
	for i := range churners {
		churnName := fmt.Sprintf("churn%d", i)
		loop(func(ctx context.Context) {
			if db, err := be.Database(churnName); err == nil {
				_ = db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"})
			}
			insertAndCommit(ctx, churnName)
			_ = be.DropDatabase(ctx, &backends.DropDatabaseParams{Name: churnName})
		})
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	deadline := time.After(runFor)
	lastProgress, lastChange := progress.Load(), time.Now()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			close(stop)
			deadline = nil
		case <-done:
			require.Greater(t, progress.Load(), int64(writers), "workers made too little progress to exercise the race")
			be.Close()
			return
		case <-ticker.C:
			if cur := progress.Load(); cur != lastProgress {
				lastProgress, lastChange = cur, time.Now()
				continue
			}
			if time.Since(lastChange) > stallTime {
				// Panic rather than t.Fatal: the stuck goroutines hold b.mu, so
				// cleanup would hang the test binary until the global timeout.
				buf := make([]byte, 1<<22)
				buf = buf[:runtime.Stack(buf, true)]
				panic(fmt.Sprintf("no progress for %s after %d operations; server is deadlocked\n\n%s",
					stallTime, lastProgress, buf))
			}
		}
	}
}

// A drop must not close a store while a command that already holds it is
// still running, because commits and reads use the store without db.mu.
func TestDropDatabaseWaitsForCommandUsingTheStore(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	const dbName = "inuse"
	readerCtx, readerCI := ctxWithSessionAs(t, be, "drop-wait-reader", "")
	dropperCtx, dropperCI := ctxWithSessionAs(t, be, "drop-wait-dropper", "")
	readerShadow, _ := readerCI.CachedShadow()
	dropperShadow, _ := dropperCI.CachedShadow()

	dropDone := make(chan error, 1)
	err = readerShadow.Use(time.Now(), func(*dsess.DoltSession) error {
		state, err := be.getOrOpenDB(readerCtx, dbName, true)
		require.NoError(t, err)
		root, err := state.cs.Root(readerCtx)
		require.NoError(t, err)

		go func() {
			dropDone <- dropperShadow.Use(time.Now(), func(*dsess.DoltSession) error {
				return be.DropDatabase(dropperCtx, &backends.DropDatabaseParams{Name: dbName})
			})
		}()

		select {
		case err := <-dropDone:
			t.Fatalf("drop finished while a command still held the store: %v", err)
		case <-time.After(200 * time.Millisecond):
		}

		readRoot := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("reading the store panicked: %v", r)
				}
			}()
			_, err = state.cs.Get(readerCtx, root)
			return err
		}
		return readRoot()
	})
	require.NoError(t, err, "the store must stay open until the command using it ends")

	select {
	case err := <-dropDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("drop did not finish after the command using the store ended")
	}
	state, err := be.getOrOpenDB(context.Background(), dbName, false)
	require.NoError(t, err)
	require.Nil(t, state, "the dropped database must be gone")
}

// A drop whose context ends while it waits for commands in flight must fail
// and leave the database open, not half-dropped.
func TestDropDatabaseCanceledWhileWaitingKeepsTheDatabase(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	const dbName = "kept"
	readerCtx, readerCI := ctxWithSessionAs(t, be, "drop-cancel-reader", "")
	dropperCtx, dropperCI := ctxWithSessionAs(t, be, "drop-cancel-dropper", "")
	readerShadow, _ := readerCI.CachedShadow()
	dropperShadow, _ := dropperCI.CachedShadow()

	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(readerCtx, &backends.CreateCollectionParams{Name: "items"}))

	err = readerShadow.Use(time.Now(), func(*dsess.DoltSession) error {
		ctx, cancel := context.WithTimeout(dropperCtx, 100*time.Millisecond)
		defer cancel()
		return dropperShadow.Use(time.Now(), func(*dsess.DoltSession) error {
			dropErr := be.DropDatabase(ctx, &backends.DropDatabaseParams{Name: dbName})
			require.ErrorIs(t, dropErr, context.DeadlineExceeded)
			return nil
		})
	})
	require.NoError(t, err)

	state, err := be.getOrOpenDB(readerCtx, dbName, false)
	require.NoError(t, err)
	require.NotNil(t, state, "a failed drop must leave the database in place")
	require.NoError(t, db.CreateCollection(readerCtx, &backends.CreateCollectionParams{Name: "more"}))

	require.NoError(t, be.DropDatabase(dropperCtx, &backends.DropDatabaseParams{Name: dbName}))
}

// An iterator that outlives the command that opened it, as a cursor's does,
// is not covered by the drop's wait for commands in flight. The drop must have
// it closed while the store can still serve it, so a later getMore ends the
// cursor instead of reading a closed store.
func TestDropDatabaseClosesIteratorsBeforeTheStore(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	const dbName = "cursored"
	readerCtx, readerCI := ctxWithSessionAs(t, be, "drop-iter-reader", "")
	dropperCtx, dropperCI := ctxWithSessionAs(t, be, "drop-iter-dropper", "")
	readerShadow, _ := readerCI.CachedShadow()
	dropperShadow, _ := dropperCI.CachedShadow()

	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(readerCtx, &backends.CreateCollectionParams{Name: "items"}))
	coll, err := db.Collection("items")
	require.NoError(t, err)
	sess := sessionFromContext(readerCtx)
	_, err = sess.StartTransaction(sqlctx.Wrap(readerCtx, sess), sql.ReadWrite)
	require.NoError(t, err)
	docs := make([]*types.Document, 3)
	for i := range docs {
		docs[i], err = types.NewDocument("_id", int32(i))
		require.NoError(t, err)
	}
	_, err = coll.InsertAll(readerCtx, &backends.InsertAllParams{Docs: docs})
	require.NoError(t, err)
	require.NoError(t, be.OnTransactionCommit(readerCtx, conninfo.Get(readerCtx).Owner()))

	registry := cursor.NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(registry.Close)
	var cur *cursor.Cursor
	require.NoError(t, readerShadow.Use(time.Now(), func(*dsess.DoltSession) error {
		res, err := coll.Query(readerCtx, &backends.QueryParams{})
		require.NoError(t, err)
		cur = registry.NewCursor(context.Background(), res.Iter, &cursor.NewParams{
			Type: cursor.Normal, DB: dbName + "@main", Collection: "items",
		})
		_, doc, err := cur.Next()
		require.NoError(t, err)
		require.NotNil(t, doc)
		return nil
	}))

	state, err := be.getOrOpenDB(readerCtx, dbName, false)
	require.NoError(t, err)
	root, err := state.cs.Root(readerCtx)
	require.NoError(t, err)

	closedIterators := false
	require.NoError(t, dropperShadow.Use(time.Now(), func(*dsess.DoltSession) error {
		return be.DropDatabase(dropperCtx, &backends.DropDatabaseParams{
			Name: dbName,
			CloseIterators: func(name string) {
				require.Equal(t, dbName, name)
				_, err := state.cs.Get(readerCtx, root)
				require.NoError(t, err, "iterators must be closed while the store is still open")
				for _, c := range registry.All() {
					registry.CloseAndRemove(c)
				}
				closedIterators = true
			},
		})
	}))
	require.True(t, closedIterators, "the drop must close the iterators over the database")

	_, _, err = cur.Next()
	require.ErrorIs(t, err, iterator.ErrIteratorDone, "a getMore after the drop must end the cursor")
}
