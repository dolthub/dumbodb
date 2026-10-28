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

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
	"github.com/dolthub/dumbodb/internal/types"
)

// A client write that publishes while dumboCommit is between reading the
// working set and committing it must survive the commit (dolthub/dumbodb#167).
// It used to be overwritten, or dumboCommit failed with an optimistic lock
// error. The commit and its working set now land in one compare-and-swap, so
// the publish makes the swap fail with nothing committed, and dumboCommit
// commits again from the branch as the write left it: the write is in the
// commit, and nothing was committed twice.
func TestDumboCommitKeepsWritesPublishedDuringIt(t *testing.T) {
	for _, branch := range []string{defaultBranch, "feature"} {
		t.Run(branch, func(t *testing.T) {
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
			state, err := be.getOrOpenDB(context.Background(), dbName, false)
			require.NoError(t, err)
			headBefore, err := branchHeadHash(context.Background(), state, branch)
			require.NoError(t, err)

			insertInTransaction(t, be, writerCtx, target, "during-commit")
			fired := false
			hook := func() {
				if !fired {
					fired = true
					commitTransaction(t, be, writerCtx)
				}
			}
			res, err := be.DumboDBCommit(context.WithValue(committerCtx, branchOpHookKey{}, hook), &backends.CommitParams{
				DBName: dbName, Branch: branch, Message: "c", Author: "a",
			})
			require.NoError(t, err)

			readerCtx, _ := ctxWithSessionAs(t, be, "reader-lsid", "")
			require.Equal(t, 2, countDocsCtx(t, readerCtx, be, target, "items"),
				"an insert acknowledged while dumboCommit ran was lost")
			parent, err := firstParentHash(context.Background(), state, hash.Parse(res.CommitID))
			require.NoError(t, err)
			require.Equal(t, headBefore, parent, "the commit must sit directly on the HEAD it started from: one commit, not two")
			_, err = be.DumboDBCommit(committerCtx, &backends.CommitParams{DBName: dbName, Branch: branch, Message: "after", Author: "a"})
			require.ErrorIs(t, err, backends.ErrEmptyCommit, "the write published during the commit must be in it")
		})
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
