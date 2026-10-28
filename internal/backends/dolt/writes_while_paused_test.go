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
	"testing"

	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// While a merge is paused on conflicts the branch accepts no writes, from a
// session or not, and a transaction begun before the pause cannot commit onto
// it either. The paused merge on disk is left exactly as it was.
func TestWritesRefusedWhileAnOperationIsPaused(t *testing.T) {
	f := newRaceFixture(t)
	f.commitDoc("feature", "feat", must.NotFail(types.NewDocument("_id", "x", "v", int32(1))))
	f.commitDoc(defaultBranch, "main", must.NotFail(types.NewDocument("_id", "x", "v", int32(2))))
	insertInTransaction(t, f.be, f.writer, f.db, "begun-before-the-pause")
	require.Error(t, f.mergeFeature(f.op))
	require.Equal(t, 1, pausedConflictsOnDisk(t, f))

	items := must.NotFail(must.NotFail(f.be.Database(f.db)).Collection("items"))
	insert := func(ctx context.Context, id string) error {
		_, err := items.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{must.NotFail(types.NewDocument("_id", id))}})
		return err
	}
	other, _ := ctxWithSessionAs(t, f.be, "other-lsid", "")
	sess := sessionFromContext(other)
	_, err := sess.StartTransaction(sqlctx.Wrap(other, sess), sql.ReadWrite)
	require.NoError(t, err)

	for name, err := range map[string]error{
		"a write without a session":                       insert(context.Background(), "direct"),
		"a session write":                                 insert(other, "session"),
		"creating a collection":                           must.NotFail(f.be.Database(f.db)).CreateCollection(other, &backends.CreateCollectionParams{Name: "another"}),
		"committing a transaction begun before the pause": f.be.OnTransactionCommit(f.writer, conninfo.Get(f.writer).Owner()),
	} {
		require.True(t, backends.ErrorCodeIs(err, backends.ErrorCodeBranchInConflict), "%s: %v", name, err)
	}
	require.Equal(t, 1, pausedConflictsOnDisk(t, f), "a refused write changed the paused merge")
	require.False(t, f.ids()["begun-before-the-pause"])
}

// pausedConflictsOnDisk counts the document conflicts of the operation paused
// on main, rebuilt from the working set on disk as a restart rebuilds them.
func pausedConflictsOnDisk(t *testing.T, f *raceFixture) int {
	t.Helper()
	ctx := context.Background()
	state, err := f.be.getOrOpenDB(ctx, f.db, false)
	require.NoError(t, err)
	ws, err := state.doltDB.ResolveWorkingSet(ctx, doltref.NewWorkingSetRef("heads/"+defaultBranch))
	require.NoError(t, err)
	require.True(t, ws.MergeActive(), "the working set no longer records the paused merge")
	ms, err := mergeStateFromWS(ctx, state, defaultBranch, ws)
	require.NoError(t, err)
	n := 0
	for _, entries := range ms.conflicts {
		n += len(entries)
	}
	return n
}
