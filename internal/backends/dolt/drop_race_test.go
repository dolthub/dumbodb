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

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
	"github.com/dolthub/dumbodb/internal/types"
)

// Dropping a database while other connections write to it and create it must
// never stop the server. A writer holds db.mu and the provider holds dbCacheMu
// while each reaches back into b.mu, so a drop that waits for either while
// holding b.mu freezes every request.
//
// Churn workers each create, fill, commit and drop their own database, so
// drops also race session lookups that hold the provider's dbCacheMu.
//
// Only a goroutine dropping a database commits to it. A commit reads the store without db.mu,
// so a drop closing the store under a concurrent commit is a separate hazard
// this test does not cover.
func TestDropDatabaseRacingWritesDoesNotDeadlock(t *testing.T) {
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
	var lsidSeq atomic.Int64
	stop := make(chan struct{})

	insertInTxn := func(ctx context.Context, dbName string, commit bool) {
		sess := sessionFromContext(ctx)
		if _, err := sess.StartTransaction(sqlctx.Wrap(ctx, sess), sql.ReadWrite); err != nil {
			return
		}
		owner := conninfo.Get(ctx).Owner()
		db, err := be.Database(dbName)
		if err == nil {
			if coll, err := db.Collection("items"); err == nil {
				doc, _ := types.NewDocument("_id", fmt.Sprintf("doc-%d", lsidSeq.Add(1)))
				_, _ = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}})
			}
		}
		if !commit || be.OnTransactionCommit(ctx, owner) != nil {
			be.OnTransactionAbort(owner)
		}
	}

	newSessionCtx := func() context.Context {
		ctx, _ := ctxWithSessionAs(t, be, fmt.Sprintf("drop-race-%d", lsidSeq.Add(1)), "")
		return ctx
	}

	var wg sync.WaitGroup
	loop := func(step func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				step()
				progress.Add(1)
			}
		}()
	}

	loop(func() {
		ctx := newSessionCtx()
		insertInTxn(ctx, dbName, true)
		_ = be.DropDatabase(ctx, &backends.DropDatabaseParams{Name: dbName})
	})
	for range writers {
		loop(func() { insertInTxn(newSessionCtx(), dbName, false) })
	}
	for i := range churners {
		churnName := fmt.Sprintf("churn%d", i)
		loop(func() {
			ctx := newSessionCtx()
			if db, err := be.Database(churnName); err == nil {
				_ = db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"})
			}
			insertInTxn(ctx, churnName, true)
			_ = be.DropDatabase(ctx, &backends.DropDatabaseParams{Name: churnName})
		})
	}
	loop(func() {
		ctx := newSessionCtx()
		db, err := be.Database(dbName)
		if err == nil {
			_ = db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"})
		}
	})

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
		_, ok := be.provider.dbLookup("bystander")
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
	_, ok := be.provider.dbLookup("victim")
	require.False(t, ok)
}
