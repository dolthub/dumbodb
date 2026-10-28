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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// pauseWithDeferredCollection pauses a merge of feature into main on a
// document conflict in items and a merge-mode conflict on gov, which defers
// gov's documents until the mode is settled.
func pauseWithDeferredCollection(t *testing.T) *raceFixture {
	f := newRaceFixture(t)
	db := must.NotFail(f.be.Database(f.db))
	require.NoError(t, db.CreateCollection(f.writer, &backends.CreateCollectionParams{Name: "gov"}))
	_, err := f.be.DumboDBCommit(f.op, &backends.CommitParams{DBName: f.db, Message: "gov", Author: "a"})
	require.NoError(t, err)
	_, err = f.be.DumboDBBranch(f.op, &backends.BranchParams{DBName: f.db, Action: "add", From: defaultBranch, Name: "feature2"})
	require.NoError(t, err)
	for branch, mode := range map[string]string{"feature2": backends.MergeModeFieldDivergent, defaultBranch: backends.MergeModeDocumentDivergent} {
		bdb := must.NotFail(f.be.Database(f.target(branch)))
		require.NoError(t, bdb.CollMod(f.op, &backends.CollModParams{Name: "gov", MergeMode: mode}))
		v := int32(1)
		if branch == defaultBranch {
			v = 2
		}
		f.commitDoc(branch, branch, must.NotFail(types.NewDocument("_id", "x", "v", v)))
	}
	_, err = f.be.DumboDBMerge(f.op, &backends.MergeParams{DBName: f.db, Into: defaultBranch, From: "feature2", Author: "a"})
	var conflict *backends.MergeConflictError
	require.ErrorAs(t, err, &conflict)
	state := must.NotFail(f.be.getOrOpenDB(context.Background(), f.db, false))
	require.NotEmpty(t, state.mergeState.deferredCollections, "the fixture must defer a collection")
	return f
}

func (f *raceFixture) resolveAll(resolution string) {
	f.t.Helper()
	res, err := f.be.DumboDBConflicts(f.op, &backends.ConflictsParams{DBName: f.db, Branch: defaultBranch})
	require.NoError(f.t, err)
	for _, mc := range res.Metadata {
		_, err := f.be.DumboDBResolveConflict(f.op, &backends.ResolveConflictParams{
			DBName: f.db, Branch: defaultBranch, Collection: mc.Collection, ConflictID: mc.ConflictID, Resolution: resolution,
		})
		require.NoError(f.t, err, "resolving the metadata of %s", mc.Collection)
	}
	for _, cc := range res.Collections {
		for _, c := range cc.Conflicts {
			_, err := f.be.DumboDBResolveConflict(f.op, &backends.ResolveConflictParams{
				DBName: f.db, Branch: defaultBranch, Collection: cc.Collection, ConflictID: c.ConflictID, Resolution: resolution,
			})
			require.NoError(f.t, err, "resolving %s/%s", cc.Collection, c.ConflictID)
		}
	}
}

// A write published while a merge is paused must survive the continue that
// merges the collections the pause deferred.
func TestContinueWithDeferredCollectionsKeepsWritesFromThePause(t *testing.T) {
	f := pauseWithDeferredCollection(t)
	f.resolveAll("theirs")
	insertInTransaction(t, f.be, f.writer, f.db, "during-pause")
	commitTransaction(t, f.be, f.writer)

	_, err := f.be.DumboDBMerge(f.op, &backends.MergeParams{DBName: f.db, Into: defaultBranch, Continue: true, Author: "a"})
	require.NoError(t, err)
	require.True(t, f.ids()["during-pause"], "the continue overwrote a write published during the pause")
}

// The same holds for a write to the deferred collection itself: its merge
// must build on the collection as the pause left it, not as it was committed.
func TestContinueWithDeferredCollectionsKeepsWritesToThem(t *testing.T) {
	f := pauseWithDeferredCollection(t)
	f.resolveAll("theirs")
	gov := must.NotFail(must.NotFail(f.be.Database(f.db)).Collection("gov"))
	_, err := gov.InsertAll(context.Background(), &backends.InsertAllParams{Docs: []*types.Document{must.NotFail(types.NewDocument("_id", "into-gov"))}})
	require.NoError(t, err)

	_, err = f.be.DumboDBMerge(f.op, &backends.MergeParams{DBName: f.db, Into: defaultBranch, Continue: true, Author: "a"})
	require.NoError(t, err)
	readerCtx, _ := ctxWithSessionAs(t, f.be, "reader-lsid", "")
	require.True(t, docIDs(t, f.be, readerCtx, f.db, "gov")["into-gov"], "the continue overwrote a write to the deferred collection")
}

// A hard reset that loses the race to a client write changes nothing: HEAD
// stays where it was and the write survives.
func TestHardResetFailsWhenAWriteLandsWhileItRuns(t *testing.T) {
	f := newRaceFixture(t)
	f.commitDoc(defaultBranch, "main", must.NotFail(types.NewDocument("_id", "main")))
	headBefore := f.head()
	_, err := f.be.DumboDBReset(f.racing("during-reset"), &backends.ResetParams{DBName: f.db, Branch: defaultBranch, CommitID: f.commitID["base"], Hard: true})
	require.ErrorIs(t, err, backends.ErrWriteRaced)
	require.Equal(t, headBefore, f.head())
	require.True(t, f.ids()["during-reset"])
}

// A reset under a paused operation would move HEAD out from under it, and the
// continue would then commit on the commit the reset left. It is refused.
func TestResetRefusedWhileAnOperationIsPaused(t *testing.T) {
	f := newRaceFixture(t)
	f.commitDoc("feature", "feat", must.NotFail(types.NewDocument("_id", "x", "v", int32(1))))
	f.commitDoc(defaultBranch, "main", must.NotFail(types.NewDocument("_id", "x", "v", int32(2))))
	require.Error(t, f.mergeFeature(f.op))
	for _, hard := range []bool{false, true} {
		_, err := f.be.DumboDBReset(f.op, &backends.ResetParams{DBName: f.db, Branch: defaultBranch, CommitID: f.commitID["base"], Hard: hard})
		require.ErrorContains(t, err, "paused")
	}
	require.Equal(t, f.commitID["main"], f.head())
}

// A rebase abort that loses the race leaves the rebase paused exactly as it
// was, on disk too: reloading it, as a restart does, finds the same tip and
// the abort then succeeds.
func TestRaceLostRebaseAbortLeavesTheRebasePaused(t *testing.T) {
	f := newRaceFixture(t)
	f.commitDoc("feature", "feat", must.NotFail(types.NewDocument("_id", "x", "v", int32(1))))
	f.commitDoc(defaultBranch, "m1", must.NotFail(types.NewDocument("_id", "m1")))
	f.commitDoc(defaultBranch, "m2", must.NotFail(types.NewDocument("_id", "x", "v", int32(2))))
	require.Error(t, f.rebaseOntoFeature(f.op))
	pausedHead := f.head()
	require.NotEqual(t, f.commitID["m2"], pausedHead, "the fixture must pause after replaying a pick")

	abort := func(ctx context.Context) error {
		_, err := f.be.DumboDBRebase(ctx, &backends.RebaseParams{DBName: f.db, Branch: defaultBranch, Abort: true})
		return err
	}
	require.ErrorIs(t, abort(f.racing("during-abort")), backends.ErrWriteRaced)
	require.Equal(t, pausedHead, f.head(), "an abort that lost the race moved HEAD")

	state := must.NotFail(f.be.getOrOpenDB(context.Background(), f.db, false))
	reloaded, err := loadMergeState(context.Background(), state)
	require.NoError(t, err)
	require.NotNil(t, reloaded)
	require.Equal(t, state.mergeState.intoHash, reloaded.intoHash, "a reload must find the rebase where memory has it")

	require.NoError(t, abort(f.op))
	require.Equal(t, f.commitID["m2"], f.head())
	require.Empty(t, f.mergeOp())
}

// requireOperationStateMatchesDisk fails unless the paused operation held in
// memory is the one a restart would rebuild from the branch.
func requireOperationStateMatchesDisk(t *testing.T, f *raceFixture) {
	t.Helper()
	state := must.NotFail(f.be.getOrOpenDB(context.Background(), f.db, false))
	onDisk, err := loadMergeState(context.Background(), state)
	require.NoError(t, err)
	if onDisk == nil {
		require.Nil(t, state.mergeState, "memory holds an operation the branch does not")
		return
	}
	require.NotNil(t, state.mergeState, "the branch holds an operation memory does not")
	require.Equal(t, onDisk.intoHash, state.mergeState.intoHash)
	require.Equal(t, onDisk.rebaseCurrentPick, state.mergeState.rebaseCurrentPick)
	require.Equal(t, onDisk.rebaseRemainingHashes, state.mergeState.rebaseRemainingHashes)
	require.Equal(t, onDisk.rebaseCommitsReplayed, state.mergeState.rebaseCommitsReplayed)
}

// A rebase or continue that fails before its write lands leaves the paused
// operation in memory exactly as the branch records it: none at all for a new
// rebase, the pause it started from for a continue. The rebase can then still
// be finished.
func TestFailedRebaseKeepsOperationStateInStepWithTheBranch(t *testing.T) {
	f := newRaceFixture(t)
	f.commitDoc("feature", "feat", must.NotFail(types.NewDocument("_id", "x", "v", int32(1))))
	f.commitDoc(defaultBranch, "m1", must.NotFail(types.NewDocument("_id", "m1")))
	f.commitDoc(defaultBranch, "m2", must.NotFail(types.NewDocument("_id", "x", "v", int32(2))))
	f.commitDoc(defaultBranch, "m3", must.NotFail(types.NewDocument("_id", "m3")))
	headBefore := f.head()
	failing := context.WithValue(f.op, branchOpHookKey{}, func() error { return errors.New("injected failure") })

	require.ErrorContains(t, f.rebaseOntoFeature(failing), "injected failure")
	require.Equal(t, headBefore, f.head())
	require.Empty(t, f.mergeOp(), "a rebase that failed before publishing left an operation in progress")
	requireOperationStateMatchesDisk(t, f)

	require.Error(t, f.rebaseOntoFeature(f.op))
	require.Equal(t, "rebase", f.mergeOp())
	f.resolveAll("theirs")
	_, err := f.be.DumboDBRebase(failing, &backends.RebaseParams{DBName: f.db, Branch: defaultBranch, Continue: true})
	require.ErrorContains(t, err, "injected failure")
	require.Equal(t, "rebase", f.mergeOp())
	requireOperationStateMatchesDisk(t, f)

	_, err = f.be.DumboDBRebase(f.op, &backends.RebaseParams{DBName: f.db, Branch: defaultBranch, Continue: true})
	require.NoError(t, err)
	require.Empty(t, f.mergeOp())
	ids := f.ids()
	require.True(t, ids["m1"] && ids["m3"], "%v", ids)
}

// A command that fails after its write landed takes the paused operation from
// the branch rather than going back to where it started.
func TestCommandFailingAfterItPublishedReloadsTheOperation(t *testing.T) {
	f := newRaceFixture(t)
	f.commitDoc("feature", "feat", must.NotFail(types.NewDocument("_id", "x", "v", int32(1))))
	f.commitDoc(defaultBranch, "main", must.NotFail(types.NewDocument("_id", "x", "v", int32(2))))
	require.Error(t, f.mergeFeature(f.op))
	state := must.NotFail(f.be.getOrOpenDB(context.Background(), f.db, false))

	tip, err := state.readBranchTip(context.Background(), defaultBranch)
	require.NoError(t, err)
	tip.published = true
	failed := errors.New("failed after publishing")
	state.mergeState = nil
	state.keepOperationStateConsistent(nil, &tip, &failed)
	requireOperationStateMatchesDisk(t, f)
	require.NotNil(t, state.mergeState)
}

// dumboMerge abort and continue act only on a merge: a paused rebase or revert
// is left for its own command, which knows how to finish or undo it.
func TestMergeAbortRefusesOtherOperations(t *testing.T) {
	f := newRaceFixture(t)
	f.commitDoc("feature", "feat", must.NotFail(types.NewDocument("_id", "x", "v", int32(1))))
	f.commitDoc(defaultBranch, "m1", must.NotFail(types.NewDocument("_id", "m1")))
	f.commitDoc(defaultBranch, "m2", must.NotFail(types.NewDocument("_id", "x", "v", int32(2))))
	require.Error(t, f.rebaseOntoFeature(f.op))
	pausedHead := f.head()

	for _, params := range []*backends.MergeParams{
		{DBName: f.db, Into: defaultBranch, Abort: true},
		{DBName: f.db, Into: defaultBranch, Continue: true},
	} {
		_, err := f.be.DumboDBMerge(f.op, params)
		require.ErrorContains(t, err, "rebase in progress")
		require.Equal(t, "rebase", f.mergeOp())
		require.Equal(t, pausedHead, f.head())
	}
}
