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
	"testing"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// countQuery runs filter on items, through the hinted index or a collection
// scan, and counts the documents it returns, duplicates included.
func countQuery(t *testing.T, be *Backend, ctx context.Context, dbName string, filter *types.Document, hint any) int {
	t.Helper()
	db, err := be.Database(dbName)
	require.NoError(t, err)
	coll, err := db.Collection("items")
	require.NoError(t, err)
	res, err := coll.Query(ctx, &backends.QueryParams{Filter: filter, Hint: hint})
	require.NoError(t, err)
	defer res.Iter.Close()
	n := 0
	for {
		_, d, err := res.Iter.Next()
		if err != nil || d == nil {
			return n
		}
		n++
	}
}

// Two ordinary writes that forked from the same snapshot each change a
// different document. Their merges at the write boundary must leave exactly
// one index entry per remaining document, whichever side of the merge each
// change lands on (dolthub/dumbodb#361).
func TestDisjointWritesMergeSecondaryIndexEntries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantDocs  int
		writeEach func(t *testing.T, be *Backend, ctx context.Context, dbName, id string)
	}{
		{"update", 2, func(t *testing.T, be *Backend, ctx context.Context, dbName, id string) {
			writeOrdinary(t, be, ctx, dbName, must.NotFail(types.NewDocument("_id", id, "v", int32(1))), false)
		}},
		{"delete", 0, func(t *testing.T, be *Backend, ctx context.Context, dbName, id string) {
			deleteOrdinary(t, be, ctx, dbName, id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
			require.NoError(t, err)
			t.Cleanup(be.Close)

			const dbName = "idxmerge"
			seeder, _ := ctxWithSessionAs(t, be, "seeder", "")
			db, err := be.Database(dbName)
			require.NoError(t, err)
			require.NoError(t, db.CreateCollection(seeder, &backends.CreateCollectionParams{Name: "items"}))
			coll, err := db.Collection("items")
			require.NoError(t, err)
			_, err = coll.CreateIndexes(seeder, &backends.CreateIndexesParams{
				Indexes: []backends.IndexInfo{{Name: "v_1", Key: []backends.IndexKeyPair{{Field: "v"}}}},
			})
			require.NoError(t, err)
			for _, id := range []string{"a", "b"} {
				writeOrdinary(t, be, seeder, dbName, must.NotFail(types.NewDocument("_id", id, "v", int32(0))), true)
				reconcileOrdinary(t, be, seeder)
			}

			first, _ := ctxWithSessionAs(t, be, "first", "")
			second, _ := ctxWithSessionAs(t, be, "second", "")
			tc.writeEach(t, be, first, dbName, "a")
			tc.writeEach(t, be, second, dbName, "b")
			reconcileOrdinary(t, be, first)
			reconcileOrdinary(t, be, second)

			reader, _ := ctxWithSessionAs(t, be, "reader", "")
			all := must.NotFail(types.NewDocument("v", must.NotFail(types.NewDocument("$gte", int32(0)))))
			natural := must.NotFail(types.NewDocument("$natural", int32(1)))
			scanned := countQuery(t, be, reader, dbName, all, natural)
			indexed := countQuery(t, be, reader, dbName, all, "v_1")
			require.Equal(t, tc.wantDocs, scanned)
			require.Equal(t, scanned, indexed, fmt.Sprintf("v_1 returned %d documents for %d stored", indexed, scanned))
			require.Equal(t, scanned, indexEntryCount(t, reader, be, dbName, defaultBranch, "items", "v_1"),
				"v_1 must hold one entry per stored document")
		})
	}
}

// deleteOrdinary deletes id the way a delete command outside a client
// transaction does: into the session overlay, pinned to its base.
func deleteOrdinary(t *testing.T, be *Backend, ctx context.Context, dbName, id string) {
	t.Helper()
	ci := conninfo.Get(ctx)
	ci.SetWriting(true)
	t.Cleanup(func() { ci.SetWriting(false) })
	sess := sessionFromContext(ctx)
	require.NotNil(t, sess)
	sqlCtx := sqlctx.Wrap(ctx, sess)
	if sqlCtx.GetTransaction() == nil {
		_, err := sess.StartTransaction(sqlCtx, sql.ReadWrite)
		require.NoError(t, err)
	}
	db, err := be.Database(dbName)
	require.NoError(t, err)
	coll, err := db.Collection("items")
	require.NoError(t, err)
	_, err = coll.DeleteAll(ctx, &backends.DeleteAllParams{IDs: []any{id}})
	require.NoError(t, err)
}
