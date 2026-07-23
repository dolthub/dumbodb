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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
	"github.com/dolthub/dumbodb/internal/types"
)

func ctxWithSessionAs(t *testing.T, be *Backend, lsid, user string) (context.Context, *conninfo.ConnInfo) {
	t.Helper()

	ci := conninfo.New()
	ci.SetLSID(lsid)
	if user != "" {
		ci.SetAuth(user, "", nil, "admin")
	}

	shadow, err := be.SessionRegistry().Connect(ci.Owner())
	require.NoError(t, err)
	ci.SetCachedShadow(ci.Owner(), shadow)

	return conninfo.Ctx(context.Background(), ci), ci
}

func countDocsCtx(t *testing.T, ctx context.Context, b *Backend, dbName, collName string) int {
	t.Helper()

	db, err := b.Database(dbName)
	require.NoError(t, err)
	coll, err := db.Collection(collName)
	require.NoError(t, err)

	res, err := coll.Query(ctx, &backends.QueryParams{})
	require.NoError(t, err)
	defer res.Iter.Close()

	n := 0
	for {
		_, _, iterErr := res.Iter.Next()
		if iterErr != nil {
			break
		}
		n++
	}
	return n
}

func TestSession_CrossUserSameLsidIsolated(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, true, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	const dbName = "isouser"
	const lsid = "shared-lsid-L"

	ctxA, ciA := ctxWithSessionAs(t, be, lsid, "alice")
	sessA := sessionFromContext(ctxA)
	require.NotNil(t, sessA)

	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(ctxA, &backends.CreateCollectionParams{Name: "items"}))

	sqlCtxA := sqlctx.Wrap(ctxA, sessA)
	_, err = sessA.StartTransaction(sqlCtxA, sql.ReadWrite)
	require.NoError(t, err)

	collA, err := db.Collection("items")
	require.NoError(t, err)
	doc, err := types.NewDocument("_id", "a-secret")
	require.NoError(t, err)
	_, err = collA.InsertAll(ctxA, &backends.InsertAllParams{Docs: []*types.Document{doc}})
	require.NoError(t, err)

	ctxB, _ := ctxWithSessionAs(t, be, lsid, "bob")
	sessB := sessionFromContext(ctxB)
	require.NotNil(t, sessB)

	require.NotSame(t, sessA, sessB, "same lsid under a different user must resolve to a distinct session")
	shadowA, _ := ciA.CachedShadow()
	require.True(t, shadowA.Active(), "A's session must survive B connecting under the same lsid")

	assert.Equal(t, 1, countDocsCtx(t, ctxA, be, dbName, "items"), "A must see its own uncommitted write")
	assert.Equal(t, 0, countDocsCtx(t, ctxB, be, dbName, "items"), "B must not see A's uncommitted write")
}
