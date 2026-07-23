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

func acInsert(t *testing.T, ctx context.Context, b *Backend, dbName, coll string, id int64) {
	t.Helper()
	db, err := b.Database(dbName)
	require.NoError(t, err)
	c, err := db.Collection(coll)
	require.NoError(t, err)
	doc, err := types.NewDocument("_id", id)
	require.NoError(t, err)
	doc.SetRecordID(id)
	_, err = c.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}})
	require.NoError(t, err)
}

func drainDBs(ci *conninfo.ConnInfo) map[string]bool {
	out := map[string]bool{}
	for _, t := range ci.DrainAutoCommit() {
		out[t.DB] = true
	}
	return out
}

func TestAdminAutoCommit_DefaultMode(t *testing.T) {
	b, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(b.Close)

	ci := conninfo.New()
	ctx := conninfo.Ctx(context.Background(), ci)

	acInsert(t, ctx, b, "userdb", "col", 1)
	require.Empty(t, drainDBs(ci), "user-db write must not auto-commit when --auto-commit is off")

	acInsert(t, ctx, b, "admin", "system.users", 1)
	require.Equal(t, map[string]bool{"admin": true}, drainDBs(ci),
		"admin write must always record an auto-commit, independent of --auto-commit")
}

func TestAdminAutoCommit_SessionIsolationBypass(t *testing.T) {
	b, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, true, 0, 0)
	require.NoError(t, err)
	t.Cleanup(b.Close)

	ctx := ctxWithSession(t, b, "admin-autocommit-lsid")
	sess := sessionFromContext(ctx)
	require.NotNil(t, sess)
	sqlCtx := sqlctx.Wrap(ctx, sess)
	_, err = sess.StartTransaction(sqlCtx, sql.ReadWrite)
	require.NoError(t, err)

	ci := conninfo.GetIfPresent(ctx)
	require.NotNil(t, ci)

	acInsert(t, ctx, b, "userdb", "col", 1)
	acInsert(t, ctx, b, "admin", "system.users", 1)
	_ = ci.DrainAutoCommit()

	acInsert(t, ctx, b, "userdb", "col", 2)
	got := drainDBs(ci)
	require.NotContains(t, got, "userdb", "user-db write in session-isolation must defer, not auto-commit")

	acInsert(t, ctx, b, "admin", "system.users", 2)
	require.Equal(t, map[string]bool{"admin": true}, drainDBs(ci),
		"admin write must auto-commit even in session-isolation mode")
}
