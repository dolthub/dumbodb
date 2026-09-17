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

package verify

// In-progress merge, cherry-pick, revert, and rebase state lives in the branch
// working set inside the Dolt tree. These tests kill the server mid-operation
// and require the paused operation to come back intact, and they assert that no
// merge-state sidecar file is ever written beside the database.

import (
	"context"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// mergeStateSidecar is the file this state used to be written to.
const mergeStateSidecar = ".dumbodb_merge_state.json"

func assertNoMergeStateSidecar(t *testing.T, env *dumboDBTestEnv) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(env.DataDir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == mergeStateSidecar {
			t.Errorf("merge state must live in the Dolt tree, found sidecar at %s", path)
		}
		return nil
	}))
}

// conflictingBranches builds a database whose main and feature branches have
// each changed _id:1 of collection "items" to a different value, and returns
// the database name.
func conflictingBranches(t *testing.T, env *dumboDBTestEnv, prefix string, oursValue, theirsValue int32) string {
	t.Helper()
	ctx := context.Background()
	dbName := fmt.Sprintf("%s%d", prefix, rand.Int64N(1_000_000))

	mainCol := env.Client.Database(dbName + "@main").Collection("items")
	_, err := mainCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "C1", "alice <alice@acme.com>")

	require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "action", Value: "add"},
		{Key: "branch", Value: "feature"},
	}).Err())

	_, err = mainCol.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: oursValue}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "ours", "alice <alice@acme.com>")

	_, err = env.Client.Database(dbName+"@feature").Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: theirsValue}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "theirs", "bob <bob@widgets.io>")

	return dbName
}

// soleConflict returns the single reported conflict on db.
func soleConflict(t *testing.T, db *mongo.Database) bson.M {
	t.Helper()
	var res bson.M
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "doltConflicts", Value: int32(1)},
	}).Decode(&res))
	conflicts, ok := res["conflicts"].(bson.A)
	require.True(t, ok, "conflicts must be an array, got %T", res["conflicts"])
	require.Len(t, conflicts, 1, "expected exactly one conflict")
	return conflicts[0].(bson.M)
}

func TestMergeConflictSurvivesRestart(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := conflictingBranches(t, env, "mergerestart", 100, 200)
	mainDB := env.Client.Database(dbName + "@main")

	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "mergeIn", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"], "conflicting merge must return ok:0")

	before := soleConflict(t, mainDB)
	assertNoMergeStateSidecar(t, env)

	env.Restart(t)
	mainDB = env.Client.Database(dbName + "@main")

	var status bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&status))
	assert.Equal(t, "merge", status["mergeState"], "merge must still be in progress after restart")

	after := soleConflict(t, mainDB)
	assert.Equal(t, before["conflictId"], after["conflictId"], "conflict id must be stable across restart")
	assert.Equal(t, before["collection"], after["collection"])
	assert.Equal(t, before["reason"], after["reason"], "conflict reason must survive restart")
	assert.Equal(t, before["ours"], after["ours"])
	assert.Equal(t, before["theirs"], after["theirs"])

	var resolveRes bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "collection", Value: "items"},
		{Key: "conflictId", Value: after["conflictId"]},
		{Key: "resolution", Value: "theirs"},
	}).Decode(&resolveRes))
	require.EqualValues(t, 1, resolveRes["ok"])

	contRaw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "continue", Value: int32(1)},
		{Key: "author", Value: "alice <alice@acme.com>"},
	})
	assert.EqualValues(t, 1, contRaw["ok"], "continue must succeed after restart")

	var doc bson.M
	require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
	assert.EqualValues(t, 200, doc["v"], "resolution must have taken theirs")

	var clean bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&clean))
	assert.Nil(t, clean["mergeState"], "merge state must be gone after continue")
	assert.Equal(t, false, clean["dirty"], "workspace must be clean after continue")
	assertNoMergeStateSidecar(t, env)
}

// A completed merge must not come back: restarting after continue has to find a
// clean branch, not the conflicted root the operation was paused on.
func TestCompletedMergeStaysCompletedAcrossRestart(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := conflictingBranches(t, env, "mergesettled", 100, 200)
	mainDB := env.Client.Database(dbName + "@main")

	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "mergeIn", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"])
	assertNoMergeStateSidecar(t, env)

	cf := soleConflict(t, mainDB)
	var resolveRes bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "collection", Value: "items"},
		{Key: "conflictId", Value: cf["conflictId"]},
		{Key: "resolution", Value: "ours"},
	}).Decode(&resolveRes))
	require.EqualValues(t, 1, resolveRes["ok"])

	contRaw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "continue", Value: int32(1)},
		{Key: "author", Value: "alice <alice@acme.com>"},
	})
	require.EqualValues(t, 1, contRaw["ok"])

	env.Restart(t)
	mainDB = env.Client.Database(dbName + "@main")

	var status bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&status))
	assert.Nil(t, status["mergeState"], "a finished merge must not reappear after restart")
	assert.Nil(t, status["conflicts"], "a finished merge must leave no conflicts behind")
	assert.Equal(t, false, status["dirty"], "workspace must be clean after restart")

	var doc bson.M
	require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
	assert.EqualValues(t, 100, doc["v"], "merge result must persist across restart")
	assertNoMergeStateSidecar(t, env)
}

func TestCherryPickAbortAfterRestart(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := conflictingBranches(t, env, "pickrestart", 100, 200)
	mainDB := env.Client.Database(dbName + "@main")

	var featureHead bson.M
	require.NoError(t, env.Client.Database(dbName+"@feature").
		RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&featureHead))
	pickHash, ok := featureHead["commitId"].(string)
	require.True(t, ok, "feature must have a HEAD commit")

	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "dumboCherryPick", Value: int32(1)},
		{Key: "commit", Value: pickHash},
	})
	require.EqualValues(t, 0, raw["ok"], "conflicting cherry-pick must return ok:0")
	assertNoMergeStateSidecar(t, env)

	env.Restart(t)
	mainDB = env.Client.Database(dbName + "@main")

	var status bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&status))
	assert.Equal(t, "cherry-pick", status["mergeState"], "cherry-pick must still be in progress after restart")
	assert.Equal(t, "items", soleConflict(t, mainDB)["collection"], "the paused conflict must be readable after restart")

	var abortRes bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "dumboCherryPick", Value: int32(1)},
		{Key: "abort", Value: int32(1)},
	}).Decode(&abortRes))
	assert.EqualValues(t, 1, abortRes["ok"], "abort must succeed after restart")

	var doc bson.M
	require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
	assert.EqualValues(t, 100, doc["v"], "abort must restore the pre-pick document")

	// The abort has to be durable too: a restart must not resurrect the
	// conflicted root it rolled back.
	env.Restart(t)
	mainDB = env.Client.Database(dbName + "@main")

	var afterAbort bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&afterAbort))
	assert.Nil(t, afterAbort["mergeState"], "aborted cherry-pick must not reappear after restart")
	assert.Equal(t, false, afterAbort["dirty"], "workspace must be clean after an aborted cherry-pick")
	assertNoMergeStateSidecar(t, env)
}

func TestRevertContinueAfterRestart(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("revrestart%d", rand.Int64N(1_000_000))
	mainDB := env.Client.Database(dbName + "@main")

	_, err := mainDB.Collection("records").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "C1", "alice <alice@acme.com>")

	_, err = mainDB.Collection("records").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(2)}}}})
	require.NoError(t, err)
	revertTarget := dumboDBCommit(t, env, dbName+"@main", "C2", "alice <alice@acme.com>")

	// A later edit to the same document makes reverting C2 conflict.
	_, err = mainDB.Collection("records").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(3)}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "C3", "alice <alice@acme.com>")

	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "dumboRevert", Value: int32(1)},
		{Key: "commit", Value: revertTarget},
	})
	require.EqualValues(t, 0, raw["ok"], "conflicting revert must return ok:0")
	assertNoMergeStateSidecar(t, env)

	env.Restart(t)
	mainDB = env.Client.Database(dbName + "@main")

	var status bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&status))
	assert.Equal(t, "revert", status["mergeState"], "revert must still be in progress after restart")

	cf := soleConflict(t, mainDB)
	var resolveRes bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "collection", Value: "records"},
		{Key: "conflictId", Value: cf["conflictId"]},
		{Key: "resolution", Value: "theirs"},
	}).Decode(&resolveRes))
	require.EqualValues(t, 1, resolveRes["ok"])

	contRaw := runCommandRaw(t, mainDB, bson.D{
		{Key: "dumboRevert", Value: int32(1)},
		{Key: "continue", Value: int32(1)},
	})
	assert.EqualValues(t, 1, contRaw["ok"], "revert continue must succeed after restart")

	var doc bson.M
	require.NoError(t, mainDB.Collection("records").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
	assert.EqualValues(t, 1, doc["v"], "revert must restore the pre-C2 value")
	assertNoMergeStateSidecar(t, env)
}

// The paused pick is the first of three, so continuing after a restart proves
// the remaining picks were recovered rather than remembered in process memory.
func TestRebaseContinueAfterRestart(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("rebrestart%d", rand.Int64N(1_000_000))
	mainCol := env.Client.Database(dbName + "@main").Collection("items")

	_, err := mainCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "C1", "alice <alice@acme.com>")

	require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "action", Value: "add"},
		{Key: "branch", Value: "feature"},
	}).Err())

	featCol := env.Client.Database(dbName + "@feature").Collection("items")

	// F1 conflicts with main's C2; F2 and F3 are clean.
	_, err = featCol.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(100)}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "F1-conflict", "bob <bob@widgets.io>")

	_, err = featCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(10)}, {Key: "v", Value: int32(10)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "F2-clean", "bob <bob@widgets.io>")

	_, err = featCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(11)}, {Key: "v", Value: int32(11)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "F3-clean", "bob <bob@widgets.io>")

	_, err = mainCol.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(200)}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "C2-conflict", "alice <alice@acme.com>")

	featDB := env.Client.Database(dbName + "@feature")
	raw := runCommandRaw(t, featDB, bson.D{
		{Key: "dumboRebase", Value: int32(1)},
		{Key: "onto", Value: "main"},
	})
	require.EqualValues(t, 0, raw["ok"], "rebase must pause on F1")
	assertNoMergeStateSidecar(t, env)

	env.Restart(t)
	featDB = env.Client.Database(dbName + "@feature")

	var status bson.M
	require.NoError(t, featDB.RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&status))
	assert.Equal(t, "rebase", status["mergeState"], "rebase must still be in progress after restart")

	cf := soleConflict(t, featDB)
	var resolveRes bson.M
	require.NoError(t, featDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "collection", Value: "items"},
		{Key: "conflictId", Value: cf["conflictId"]},
		{Key: "resolution", Value: "theirs"},
	}).Decode(&resolveRes))
	require.EqualValues(t, 1, resolveRes["ok"])

	contRaw := runCommandRaw(t, featDB, bson.D{
		{Key: "dumboRebase", Value: int32(1)},
		{Key: "continue", Value: int32(1)},
	})
	require.EqualValues(t, 1, contRaw["ok"], "rebase continue must succeed after restart")
	assert.EqualValues(t, 3, contRaw["commitsReplayed"],
		"the picks remaining after the paused one must be recovered from the working set")

	n, err := featDB.Collection("items").CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.EqualValues(t, 3, n, "all three feature commits must have landed")
	assertNoMergeStateSidecar(t, env)
}

func TestRebaseAbortAfterRestart(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := conflictingBranches(t, env, "rebabort", 200, 100)
	featDB := env.Client.Database(dbName + "@feature")

	var beforeStatus bson.M
	require.NoError(t, featDB.RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&beforeStatus))
	tipBeforeRebase, ok := beforeStatus["commitId"].(string)
	require.True(t, ok, "feature must have a HEAD before the rebase")

	raw := runCommandRaw(t, featDB, bson.D{
		{Key: "dumboRebase", Value: int32(1)},
		{Key: "onto", Value: "main"},
	})
	require.EqualValues(t, 0, raw["ok"], "rebase must conflict")
	assertNoMergeStateSidecar(t, env)

	env.Restart(t)
	featDB = env.Client.Database(dbName + "@feature")

	var abortRes bson.M
	require.NoError(t, featDB.RunCommand(ctx, bson.D{
		{Key: "dumboRebase", Value: int32(1)},
		{Key: "abort", Value: int32(1)},
	}).Decode(&abortRes))
	require.EqualValues(t, 1, abortRes["ok"], "abort must succeed after restart")
	assert.Equal(t, tipBeforeRebase, abortRes["newTip"], "abort must restore the pre-rebase tip")

	env.Restart(t)
	featDB = env.Client.Database(dbName + "@feature")

	var status bson.M
	require.NoError(t, featDB.RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&status))
	assert.Nil(t, status["mergeState"], "aborted rebase must not reappear after restart")
	assert.Equal(t, tipBeforeRebase, status["commitId"], "branch must sit on its pre-rebase tip")
	assert.Equal(t, false, status["dirty"], "workspace must be clean after an aborted rebase")

	var doc bson.M
	require.NoError(t, featDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
	assert.EqualValues(t, 100, doc["v"], "abort must restore the pre-rebase document")
	assertNoMergeStateSidecar(t, env)
}

// A GC sweep during a paused merge must not collect the pre-merge root the
// abort rolls back to, nor the commits the operation still refers to.
func TestPausedMergeSurvivesGC(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := conflictingBranches(t, env, "mergegc", 100, 200)
	mainDB := env.Client.Database(dbName + "@main")

	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "mergeIn", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"], "conflicting merge must return ok:0")

	var gcRes bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "dumboGC", Value: int32(1)},
		{Key: "mode", Value: "full"},
	}).Decode(&gcRes))
	require.EqualValues(t, 1, gcRes["ok"], "gc must succeed during a paused merge")

	env.Restart(t)
	mainDB = env.Client.Database(dbName + "@main")

	var status bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&status))
	require.Equal(t, "merge", status["mergeState"], "merge must still be in progress after gc and restart")
	assert.Equal(t, "items", soleConflict(t, mainDB)["collection"], "the conflict must still be readable")

	var abortRes bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "abort", Value: int32(1)},
	}).Decode(&abortRes))
	require.EqualValues(t, 1, abortRes["ok"], "abort must reach the pre-merge root gc left alone")

	var doc bson.M
	require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
	assert.EqualValues(t, 100, doc["v"], "abort must restore the pre-merge document")
	assertNoMergeStateSidecar(t, env)
}

// A GC while a rebase is paused must not collect the picks still queued behind
// the paused one: the branch HEAD has already moved off them, so the working
// set's pre-rebase tip is the only thing keeping that run reachable.
func TestPausedRebaseSurvivesGC(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("rebasegc%d", rand.Int64N(1_000_000))
	mainCol := env.Client.Database(dbName + "@main").Collection("items")

	_, err := mainCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "C1", "alice <alice@acme.com>")

	require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "action", Value: "add"},
		{Key: "branch", Value: "feature"},
	}).Err())

	featCol := env.Client.Database(dbName + "@feature").Collection("items")

	// F1 conflicts with main's C2, so the replay pauses before F2 and F3 are
	// committed anywhere.
	_, err = featCol.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(100)}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "F1-conflict", "bob <bob@widgets.io>")

	_, err = featCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(10)}, {Key: "v", Value: int32(10)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "F2-clean", "bob <bob@widgets.io>")

	_, err = featCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(11)}, {Key: "v", Value: int32(11)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "F3-clean", "bob <bob@widgets.io>")

	_, err = mainCol.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(200)}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "C2-conflict", "alice <alice@acme.com>")

	featDB := env.Client.Database(dbName + "@feature")
	raw := runCommandRaw(t, featDB, bson.D{
		{Key: "dumboRebase", Value: int32(1)},
		{Key: "onto", Value: "main"},
	})
	require.EqualValues(t, 0, raw["ok"], "rebase must pause on F1")

	var gcRes bson.M
	require.NoError(t, featDB.RunCommand(ctx, bson.D{
		{Key: "dumboGC", Value: int32(1)},
		{Key: "mode", Value: "full"},
	}).Decode(&gcRes))
	require.EqualValues(t, 1, gcRes["ok"], "gc must succeed during a paused rebase")

	env.Restart(t)
	featDB = env.Client.Database(dbName + "@feature")

	cf := soleConflict(t, featDB)
	var resolveRes bson.M
	require.NoError(t, featDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "collection", Value: "items"},
		{Key: "conflictId", Value: cf["conflictId"]},
		{Key: "resolution", Value: "theirs"},
	}).Decode(&resolveRes))
	require.EqualValues(t, 1, resolveRes["ok"])

	contRaw := runCommandRaw(t, featDB, bson.D{
		{Key: "dumboRebase", Value: int32(1)},
		{Key: "continue", Value: int32(1)},
	})
	require.EqualValues(t, 1, contRaw["ok"], "continue must succeed after a gc")
	assert.EqualValues(t, 3, contRaw["commitsReplayed"], "gc must not have collected the queued picks")

	n, err := featDB.Collection("items").CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.EqualValues(t, 3, n, "all three feature commits must have landed")
	assertNoMergeStateSidecar(t, env)
}

// Resolving with "ours" changes no document, so the conflict artifact is the
// only record that the conflict existed. It has to be cleared, or a restart
// reports the resolved conflict as outstanding again.
func TestOursResolutionSurvivesRestart(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := conflictingBranches(t, env, "oursrestart", 100, 200)
	mainDB := env.Client.Database(dbName + "@main")

	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "mergeIn", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"], "conflicting merge must return ok:0")

	cf := soleConflict(t, mainDB)
	var resolveRes bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "collection", Value: "items"},
		{Key: "conflictId", Value: cf["conflictId"]},
		{Key: "resolution", Value: "ours"},
	}).Decode(&resolveRes))
	require.EqualValues(t, 1, resolveRes["ok"])

	env.Restart(t)
	mainDB = env.Client.Database(dbName + "@main")

	var status bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&status))
	require.Equal(t, "merge", status["mergeState"], "the merge itself is still in progress")

	var conflicts bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltConflicts", Value: int32(1)}}).Decode(&conflicts))
	remaining, _ := conflicts["conflicts"].(bson.A)
	assert.Empty(t, remaining, "a conflict resolved with ours must stay resolved across a restart")

	contRaw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "continue", Value: int32(1)},
		{Key: "author", Value: "alice <alice@acme.com>"},
	})
	assert.EqualValues(t, 1, contRaw["ok"], "continue must succeed without re-resolving")

	var doc bson.M
	require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
	assert.EqualValues(t, 100, doc["v"], "ours must have been kept")
	assertNoMergeStateSidecar(t, env)
}
