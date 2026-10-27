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
	"io"
	"log/slog"
	"testing"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
	"github.com/dolthub/dumbodb/internal/types"
)

// A client write that publishes while dumboCommit is between reading the
// working set and writing it back must survive the commit
// (dolthub/dumbodb#167). It used to be overwritten when the publish had
// already refreshed the cached working set, and dumboCommit failed with an
// optimistic lock error when it had not.
func TestDumboCommitKeepsWritesPublishedDuringIt(t *testing.T) {
	for _, branch := range []string{defaultBranch, "feature"} {
		for _, staleCache := range []bool{false, true} {
			name := branch + "/cache refreshed"
			if staleCache {
				name = branch + "/cache stale"
			}
			t.Run(name, func(t *testing.T) {
				be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
				require.NoError(t, err)
				t.Cleanup(be.Close)

				const dbName = "commitrace"
				writerCtx, _ := ctxWithSessionAs(t, be, "writer-lsid", "")
				committerCtx, _ := ctxWithSessionAs(t, be, "committer-lsid", "")
				db, err := be.Database(dbName)
				require.NoError(t, err)
				require.NoError(t, db.CreateCollection(writerCtx, &backends.CreateCollectionParams{Name: "items"}))
				target := dbName
				if branch != defaultBranch {
					_, err := be.DumboDBBranch(writerCtx, &backends.BranchParams{DBName: dbName, Action: "add", From: defaultBranch, Name: branch})
					require.NoError(t, err)
					target = dbName + "@" + branch
				}
				insertInTransaction(t, be, writerCtx, target, "before-commit")
				commitTransaction(t, be, writerCtx)

				// The writer's insert completes before dumboCommit takes the
				// database lock; only its transaction commit, which publishes
				// lock-free, overlaps the commit.
				insertInTransaction(t, be, writerCtx, target, "during-commit")
				state, err := be.getOrOpenDB(context.Background(), dbName, false)
				require.NoError(t, err)
				hook := func() {
					entry := state.branchEntry(branch)
					entry.mu.RLock()
					cachedWS, cachedHash := entry.ws, entry.wsHash
					entry.mu.RUnlock()
					commitTransaction(t, be, writerCtx)
					if staleCache {
						entry.mu.Lock()
						entry.ws, entry.wsHash = cachedWS, cachedHash
						entry.mu.Unlock()
					}
				}
				_, err = be.DumboDBCommit(context.WithValue(committerCtx, commitPersistHookKey{}, hook), &backends.CommitParams{
					DBName: dbName, Branch: branch, Message: "c", Author: "a", AllowEmpty: true,
				})
				require.NoError(t, err)

				readerCtx, _ := ctxWithSessionAs(t, be, "reader-lsid", "")
				require.Equal(t, 2, countDocsCtx(t, readerCtx, be, target, "items"),
					"an insert acknowledged while dumboCommit ran was lost")

				// The commit recorded the working set it read; the racing
				// insert stays uncommitted, so the next commit is not empty.
				_, err = be.DumboDBCommit(committerCtx, &backends.CommitParams{
					DBName: dbName, Branch: branch, Message: "after", Author: "a",
				})
				require.NoError(t, err, "the insert published during the first commit must remain to be committed")
			})
		}
	}
}

// insertInTransaction starts a transaction and inserts a document with _id id
// in it; commitTransaction then publishes it, acknowledging the write.
func insertInTransaction(t *testing.T, be *Backend, ctx context.Context, dbName, id string) {
	t.Helper()
	sess := sessionFromContext(ctx)
	require.NotNil(t, sess)
	_, err := sess.StartTransaction(sqlctx.Wrap(ctx, sess), sql.ReadWrite)
	require.NoError(t, err)
	db, err := be.Database(dbName)
	require.NoError(t, err)
	coll, err := db.Collection("items")
	require.NoError(t, err)
	doc, err := types.NewDocument("_id", id)
	require.NoError(t, err)
	_, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}})
	require.NoError(t, err)
}

func commitTransaction(t *testing.T, be *Backend, ctx context.Context) {
	t.Helper()
	require.NoError(t, be.OnTransactionCommit(ctx, conninfo.Get(ctx).Owner()))
}
