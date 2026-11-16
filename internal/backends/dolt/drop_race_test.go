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
	"github.com/dolthub/dolt/go/store/hash"
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
		if _, err := sqlctx.EnsureTxn(sqlctx.Wrap(ctx, sess), sess); err != nil {
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

		// While the drop waits, the database is gone to lookups and refuses
		// creates the way mongod does, with DatabaseDropPending.
		bystanderCtx, _ := ctxWithSessionAs(t, be, "drop-wait-bystander", "")
		looked, err := be.getOrOpenDB(bystanderCtx, dbName, false)
		require.NoError(t, err)
		require.Nil(t, looked)
		db, err := be.Database(dbName)
		require.NoError(t, err)
		createErr := db.CreateCollection(bystanderCtx, &backends.CreateCollectionParams{Name: "late"})
		require.True(t, backends.ErrorCodeIs(createErr, backends.ErrorCodeDatabaseDropPending), "got %v", createErr)

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
		state, err := be.getOrOpenDB(readerCtx, dbName, false)
		require.NoError(t, err)
		require.NotNil(t, state)

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

// getOrBuildSqleDatabase looks a database up while holding dbCacheMu, and the
// drop forgets the cached database, which takes dbCacheMu. The drop must not
// hold b.mu while it waits there.
func TestDropDatabaseWaitingOnProviderCacheDoesNotBlockLookups(t *testing.T) {
	ctx := context.Background()
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	_, err = be.getOrOpenDB(ctx, "victim", true)
	require.NoError(t, err)
	_, err = be.getOrOpenDB(ctx, "bystander", true)
	require.NoError(t, err)

	be.provider.dbCacheMu.Lock()
	dropped := make(chan error, 1)
	go func() {
		dropped <- be.DropDatabase(ctx, &backends.DropDatabaseParams{Name: "victim"})
	}()
	// Give the drop time to reach dbCacheMu.
	time.Sleep(200 * time.Millisecond)

	looked := make(chan bool, 1)
	go func() {
		_, ok := be.provider.dbLookup(nil, "bystander")
		looked <- ok
	}()
	select {
	case ok := <-looked:
		require.True(t, ok)
	case <-time.After(10 * time.Second):
		buf := make([]byte, 1<<22)
		buf = buf[:runtime.Stack(buf, true)]
		panic(fmt.Sprintf("lookup under dbCacheMu blocked behind a drop; server is deadlocked\n\n%s", buf))
	}
	be.provider.dbCacheMu.Unlock()

	require.NoError(t, <-dropped)
	_, ok := be.provider.dbLookup(nil, "victim")
	require.False(t, ok)
}

// A drop waits only for commands using the database it drops. A command held
// open on another database, here a write parked on a document lock, must not
// delay it.
func TestDropDatabaseDoesNotWaitForCommandsOnOtherDatabases(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	busyCtx, busyCI := ctxWithSessionAs(t, be, "drop-other-busy", "")
	dropperCtx, dropperCI := ctxWithSessionAs(t, be, "drop-other-dropper", "")
	busyShadow, _ := busyCI.CachedShadow()
	dropperShadow, _ := dropperCI.CachedShadow()

	for _, name := range []string{"busy", "dropped"} {
		db, err := be.Database(name)
		require.NoError(t, err)
		require.NoError(t, db.CreateCollection(dropperCtx, &backends.CreateCollectionParams{Name: "items"}))
	}

	release := make(chan struct{})
	busyDone := make(chan error, 1)
	inCommand := make(chan struct{})
	go func() {
		busyDone <- busyShadow.Use(time.Now(), func(*dsess.DoltSession) error {
			if _, err := be.getOrOpenDB(busyCtx, "busy", false); err != nil {
				return err
			}
			close(inCommand)
			<-release
			return nil
		})
	}()
	<-inCommand

	dropDone := make(chan error, 1)
	go func() {
		dropDone <- dropperShadow.Use(time.Now(), func(*dsess.DoltSession) error {
			return be.DropDatabase(dropperCtx, &backends.DropDatabaseParams{Name: "dropped"})
		})
	}()
	select {
	case err := <-dropDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the drop waited for a command on another database")
	}

	close(release)
	require.NoError(t, <-busyDone)
}

// A write parked on a document lock of the database being dropped is
// interrupted rather than waited for, as mongod interrupts operations on a
// database it drops.
func TestDropDatabaseInterruptsParkedWrites(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	const dbName = "parked"
	parkedCtx, parkedCI := ctxWithSessionAs(t, be, "drop-parked-writer", "")
	dropperCtx, dropperCI := ctxWithSessionAs(t, be, "drop-parked-dropper", "")
	parkedShadow, _ := parkedCI.CachedShadow()
	dropperShadow, _ := dropperCI.CachedShadow()

	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(dropperCtx, &backends.CreateCollectionParams{Name: "items"}))

	locks := be.docLockManager(dbName, defaultBranch)
	id := hash.Of([]byte("doc"))
	require.NoError(t, locks.Acquire("holder", "items", []hash.Hash{id}, true))

	parked := make(chan struct{})
	parkedErr := make(chan error, 1)
	go func() {
		parkedErr <- parkedShadow.Use(time.Now(), func(*dsess.DoltSession) error {
			if _, err := be.getOrOpenDB(parkedCtx, dbName, false); err != nil {
				return err
			}
			close(parked)
			return locks.WaitUntilFree(parkedCtx, "items", []hash.Hash{id})
		})
	}()
	<-parked

	dropDone := make(chan error, 1)
	go func() {
		dropDone <- dropperShadow.Use(time.Now(), func(*dsess.DoltSession) error {
			return be.DropDatabase(dropperCtx, &backends.DropDatabaseParams{Name: dbName})
		})
	}()
	select {
	case err := <-dropDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the drop waited for a write parked on a document lock")
	}
	err = <-parkedErr
	require.True(t, backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseDropPending), "got %v", err)
}

// A command that has used a database and then drops it does not wait on
// itself.
func TestDropDatabaseByACommandThatUsedIt(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	const dbName = "selfused"
	ctx, ci := ctxWithSessionAs(t, be, "drop-self-user", "")
	shadow, _ := ci.CachedShadow()

	done := make(chan error, 1)
	go func() {
		done <- shadow.Use(time.Now(), func(*dsess.DoltSession) error {
			db, err := be.Database(dbName)
			if err != nil {
				return err
			}
			if err := db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"}); err != nil {
				return err
			}
			return be.DropDatabase(ctx, &backends.DropDatabaseParams{Name: dbName})
		})
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a drop waited on its own command")
	}
}

// A drop that runs out of time keeps the database, and its open transactions
// keep their document locks: a write to a locked document still waits.
func TestDropDatabaseTimedOutKeepsDocumentLocks(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	const dbName = "locked"
	userCtx, userCI := ctxWithSessionAs(t, be, "drop-timeout-user", "")
	dropperCtx, dropperCI := ctxWithSessionAs(t, be, "drop-timeout-dropper", "")
	userShadow, _ := userCI.CachedShadow()
	dropperShadow, _ := dropperCI.CachedShadow()

	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(dropperCtx, &backends.CreateCollectionParams{Name: "items"}))
	locks := be.docLockManager(dbName, defaultBranch)
	id := hash.Of([]byte("doc"))
	require.NoError(t, locks.Acquire("txn-holder", "items", []hash.Hash{id}, true))

	require.NoError(t, userShadow.Use(time.Now(), func(*dsess.DoltSession) error {
		_, err := be.getOrOpenDB(userCtx, dbName, false)
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(dropperCtx, 100*time.Millisecond)
		defer cancel()
		return dropperShadow.Use(time.Now(), func(*dsess.DoltSession) error {
			require.ErrorIs(t, be.DropDatabase(ctx, &backends.DropDatabaseParams{Name: dbName}), context.DeadlineExceeded)
			return nil
		})
	}))

	require.Same(t, locks, be.docLockManager(dbName, defaultBranch), "the lock manager must survive")
	waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, locks.WaitUntilFree(waitCtx, "items", []hash.Hash{id}), context.DeadlineExceeded,
		"a write to the locked document must still wait for the transaction")
}
