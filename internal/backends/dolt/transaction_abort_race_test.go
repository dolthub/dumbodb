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

func TestTransactionPublishRaceAbortClearsSessionOverlay(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	const dbName = "abortpublishrace"
	ctx, _ := ctxWithSessionAs(t, be, "abort-publish-race-lsid", "")
	sess := sessionFromContext(ctx)
	require.NotNil(t, sess)
	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items"}))

	sqlCtx := sqlctx.Wrap(ctx, sess)
	_, err = sess.StartTransaction(sqlCtx, sql.ReadWrite)
	require.NoError(t, err)
	coll, err := db.Collection("items")
	require.NoError(t, err)
	doc, err := types.NewDocument("_id", "must-be-aborted")
	require.NoError(t, err)
	_, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}})
	require.NoError(t, err)

	racingCtx := context.WithValue(ctx, sessionPublishHookKey{}, func() {
		require.NoError(t, db.CreateCollection(context.Background(), &backends.CreateCollectionParams{Name: "race-winner"}))
	})
	owner := conninfo.Get(ctx).Owner()
	err = be.OnTransactionCommit(racingCtx, owner)
	require.ErrorIs(t, err, backends.ErrWriteRaced)
	require.NotNil(t, sess.GetTransaction(), "failed commit must leave the transaction available for rollback")

	be.OnTransactionAbort(owner)
	require.Nil(t, sess.GetTransaction())
	require.Equal(t, 0, countDocsCtx(t, ctx, be, dbName, "items"))

	_, err = sess.StartTransaction(sqlctx.Wrap(ctx, sess), sql.ReadWrite)
	require.NoError(t, err)
	require.Equal(t, 0, countDocsCtx(t, ctx, be, dbName, "items"))
}
