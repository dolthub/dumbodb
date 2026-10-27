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

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// A write made without a session transaction reads the branch and writes it
// back. A session write published in between fails the write's
// compare-and-swap, so the write is refused with ErrWriteRaced rather than
// overwriting the published one (dolthub/dumbodb#167).
func TestDirectWriteFailsWhenAPublishLandsFirst(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	const dbName = "directrace"
	writerCtx, _ := ctxWithSessionAs(t, be, "writer-lsid", "")
	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(writerCtx, &backends.CreateCollectionParams{Name: "items"}))
	insertInTransaction(t, be, writerCtx, dbName, "before")
	commitTransaction(t, be, writerCtx)

	insertInTransaction(t, be, writerCtx, dbName, "published")
	hook := func() {
		require.NoError(t, be.OnTransactionCommit(writerCtx, conninfo.Get(writerCtx).Owner()))
	}
	coll, err := db.Collection("items")
	require.NoError(t, err)
	_, err = coll.InsertAll(context.WithValue(context.Background(), branchOpHookKey{}, hook),
		&backends.InsertAllParams{Docs: []*types.Document{must.NotFail(types.NewDocument("_id", "direct"))}})
	require.ErrorIs(t, err, backends.ErrWriteRaced)

	readerCtx, _ := ctxWithSessionAs(t, be, "reader-lsid", "")
	ids := docIDs(t, be, readerCtx, dbName, "items")
	require.True(t, ids["published"], "the published write was overwritten")
	require.False(t, ids["direct"], "a write refused as a race was applied")
}
