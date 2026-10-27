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
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Writes made while a merge is paused on a conflict must leave the paused
// merge on disk intact: the working set still records the operation, and its
// root still carries the conflict markers a restart rebuilds the conflicts
// from (workspace-8am).
func TestWritesDuringAConflictPauseKeepItOnDisk(t *testing.T) {
	x := func(v int32) *types.Document { return must.NotFail(types.NewDocument("_id", "x", "v", v)) }
	for _, tc := range []struct {
		name string
		// beforePause runs before the merge pauses; write runs during the pause.
		beforePause func(f *raceFixture)
		write       func(f *raceFixture)
	}{
		{"write without a session", nil, func(f *raceFixture) {
			db := must.NotFail(f.be.Database(f.db))
			coll := must.NotFail(db.Collection("items"))
			_, err := coll.InsertAll(context.Background(), &backends.InsertAllParams{Docs: []*types.Document{must.NotFail(types.NewDocument("_id", "direct"))}})
			require.NoError(f.t, err)
		}},
		{"session write", nil, func(f *raceFixture) {
			insertInTransaction(f.t, f.be, f.writer, f.db, "session")
			commitTransaction(f.t, f.be, f.writer)
		}},
		{"session write merged with another", nil, func(f *raceFixture) {
			insertInTransaction(f.t, f.be, f.writer, f.db, "first")
			other, _ := ctxWithSessionAs(f.t, f.be, "other-lsid", "")
			insertInTransaction(f.t, f.be, other, f.db, "second")
			commitTransaction(f.t, f.be, other)
			commitTransaction(f.t, f.be, f.writer)
		}},
		{"session write begun before the pause", func(f *raceFixture) {
			insertInTransaction(f.t, f.be, f.writer, f.db, "early")
		}, func(f *raceFixture) {
			commitTransaction(f.t, f.be, f.writer)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRaceFixture(t)
			f.commitDoc("feature", "feat", x(1))
			f.commitDoc(defaultBranch, "main", x(2))
			if tc.beforePause != nil {
				tc.beforePause(f)
			}
			var conflict *backends.MergeConflictError
			require.ErrorAs(t, f.mergeFeature(f.op), &conflict)
			require.Equal(t, 1, pausedConflictsOnDisk(t, f))

			tc.write(f)
			require.Equal(t, 1, pausedConflictsOnDisk(t, f), "the write dropped the paused merge's conflicts from disk")
		})
	}
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
