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

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
)

// Publishing a reconciled working set must fail when the branch moved after the
// reconcile read it. That is the whole point of the optimistic lock: the merge
// was computed against one state, so it may only replace that state.
//
// publishWorkingSet takes that fork point from the caller. It used to resolve
// the CURRENT on-disk working set as its own prevHash, which asserts that the
// working set is whatever it is right now -- always true, so the lock never
// failed and a writer that reconciled against a stale branch overwrote whoever
// published in between, both having already been told n:1.
func TestPublishWorkingSet_RefusesAStalePublish(t *testing.T) {
	ctx := context.Background()
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	state, err := be.getOrOpenDB(ctx, "raceprobe", true)
	require.NoError(t, err)
	require.NotNil(t, state)

	wsRef := doltref.NewWorkingSetRef("heads/" + defaultBranch)
	// What a reconcile does: read the branch, then build a result from it.
	// Both writers read the same starting point.
	theirs, err := state.doltDB.ResolveWorkingSet(ctx, wsRef)
	require.NoError(t, err)
	theirsHash, err := theirs.HashOf()
	require.NoError(t, err)

	// Writer A publishes first and moves the branch, the way any write does.
	db, err := be.Database("raceprobe")
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "movedByA"}))

	afterA, err := state.doltDB.ResolveWorkingSet(ctx, wsRef)
	require.NoError(t, err)
	afterAHash, err := afterA.HashOf()
	require.NoError(t, err)
	require.NotEqual(t, theirsHash, afterAHash, "writer A must have moved the branch")

	// Writer B reconciled against `theirs`, which is now stale: its result does
	// not contain A's collection. Publishing it must be refused, and B has to
	// reconcile again against what A left.
	rootB := theirs.WorkingRoot()
	wsB := theirs.WithWorkingRoot(rootB).WithStagedRoot(rootB)
	err = publishWorkingSet(ctx, state.doltDB, wsB, defaultBranch, theirsHash)
	require.Error(t, err,
		"a publish reconciled against %s must not land on top of %s", theirsHash, afterAHash)
	require.ErrorIs(t, err, backends.ErrWriteRaced,
		"a raced publish must be answerable by replaying, not reported as a failure")
}

// The precondition the publish should be using is available at the call site:
// the reconcile already resolved the working set it merged against.
func TestPublishWorkingSet_ForkPointIsAvailableAtTheRead(t *testing.T) {
	ctx := context.Background()
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	state, err := be.getOrOpenDB(ctx, "raceprobe2", true)
	require.NoError(t, err)

	wsRef := doltref.NewWorkingSetRef("heads/" + defaultBranch)
	cur, err := state.doltDB.ResolveWorkingSet(ctx, wsRef)
	require.NoError(t, err)
	_, err = cur.HashOf()
	require.NoError(t, err,
		"a working set resolved from disk can be hashed, so the reconcile can hand its hash to the publish")

	var _ *doltdb.WorkingSet = cur
}
