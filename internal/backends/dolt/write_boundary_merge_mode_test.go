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
	"errors"
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

// boundaryEdit is one writer's change to the seed document
// {_id: "d", v: 1, a: "seed", b: "seed"}.
type boundaryEdit struct {
	field string
	value any
}

// writeBoundaryShapes are the two edits the merge modes disagree on: both
// writers moving the same field to the same value (the compare-and-swap race),
// and each writer changing a different field of one document.
var writeBoundaryShapes = []struct {
	name          string
	first, second boundaryEdit
}{
	{"convergent", boundaryEdit{"v", int32(2)}, boundaryEdit{"v", int32(2)}},
	{"disjoint", boundaryEdit{"a", "first"}, boundaryEdit{"b", "second"}},
}

// writeBoundaryExpectations is whether each mode refuses the second writer,
// per shape. It is the merge mode scenario matrix's answer for the same edits.
var writeBoundaryExpectations = []struct {
	mode                     string
	onConvergent, onDisjoint bool
}{
	{backends.MergeModeDocumentTouched, true, true},
	{backends.MergeModeFieldTouched, true, false},
	{backends.MergeModeFieldDivergent, false, false},
	{backends.MergeModeDocumentDivergent, false, true},
}

// An ordinary write reconciles at the end of its command. Two writers that
// forked from the same base and both edit one document reach that boundary one
// after the other, and the collection's merge mode alone decides whether the
// second is accepted.
func TestWriteBoundaryAppliesTheCollectionsMergeMode(t *testing.T) {
	for _, exp := range writeBoundaryExpectations {
		for _, shape := range writeBoundaryShapes {
			wantConflict := exp.onConvergent
			if shape.name == "disjoint" {
				wantConflict = exp.onDisjoint
			}
			t.Run(exp.mode+"/"+shape.name, func(t *testing.T) {
				be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
				require.NoError(t, err)
				t.Cleanup(be.Close)

				const dbName = "boundarymode"
				seeder, _ := ctxWithSessionAs(t, be, "seeder-lsid", "")
				db, err := be.Database(dbName)
				require.NoError(t, err)
				require.NoError(t, db.CreateCollection(seeder, &backends.CreateCollectionParams{Name: "items", MergeMode: exp.mode}))
				writeOrdinary(t, be, seeder, dbName, boundaryEdited(t), true)
				reconcileOrdinary(t, be, seeder)

				first, _ := ctxWithSessionAs(t, be, "first-lsid", "")
				second, _ := ctxWithSessionAs(t, be, "second-lsid", "")
				writeOrdinary(t, be, first, dbName, boundaryEdited(t, shape.first), false)
				writeOrdinary(t, be, second, dbName, boundaryEdited(t, shape.second), false)
				reconcileOrdinary(t, be, first)

				err = be.OnTransactionCommit(second, conninfo.Get(second).Owner())
				reader, _ := ctxWithSessionAs(t, be, "reader-lsid", "")
				got := boundaryFields(t, boundaryDoc(t, be, reader, dbName))
				if wantConflict {
					var refusal *backends.MergeConflictError
					require.True(t, errors.As(err, &refusal),
						"mergeMode=%s must refuse a %s edit at the write boundary, got %v", exp.mode, shape.name, err)
					be.OnTransactionAbort(conninfo.Get(second).Owner())
					require.Equal(t, boundaryFields(t, boundaryEdited(t, shape.first)), got, "a refused write must not land")
					return
				}
				require.NoError(t, err, "mergeMode=%s must accept a %s edit at the write boundary", exp.mode, shape.name)
				require.Equal(t, boundaryFields(t, boundaryEdited(t, shape.first, shape.second)), got, "both writes must land")
			})
		}
	}
}

// boundaryEdited returns the seed document with edits applied.
func boundaryEdited(t *testing.T, edits ...boundaryEdit) *types.Document {
	t.Helper()
	d, err := types.NewDocument("_id", "d", "v", int32(1), "a", "seed", "b", "seed")
	require.NoError(t, err)
	for _, e := range edits {
		d.Set(e.field, e.value)
	}
	return d
}

// writeOrdinary writes d the way a write command outside a client transaction
// does: into the session overlay, pinned to the base its transaction started
// from, without a document lock.
func writeOrdinary(t *testing.T, be *Backend, ctx context.Context, dbName string, d *types.Document, insert bool) {
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
	if insert {
		_, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{d}})
	} else {
		_, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{d}})
	}
	require.NoError(t, err)
}

func reconcileOrdinary(t *testing.T, be *Backend, ctx context.Context) {
	t.Helper()
	require.NoError(t, be.OnTransactionCommit(ctx, conninfo.Get(ctx).Owner()))
	conninfo.Get(ctx).SetWriting(false)
}

func boundaryDoc(t *testing.T, be *Backend, ctx context.Context, dbName string) *types.Document {
	t.Helper()
	db, err := be.Database(dbName)
	require.NoError(t, err)
	coll, err := db.Collection("items")
	require.NoError(t, err)
	res, err := coll.Query(ctx, &backends.QueryParams{})
	require.NoError(t, err)
	defer res.Iter.Close()
	_, d, err := res.Iter.Next()
	require.NoError(t, err)
	return d
}

// boundaryFields is d's edited fields, independent of field order and record id.
func boundaryFields(t *testing.T, d *types.Document) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, k := range []string{"v", "a", "b"} {
		v, err := d.Get(k)
		require.NoError(t, err)
		out[k] = v
	}
	return out
}
