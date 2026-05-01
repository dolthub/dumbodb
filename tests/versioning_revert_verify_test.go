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

package tests

// TestRevertVerify is the automated analog of docs/verify/revert.md.
//
// Each top-level subtest corresponds to one scenario in that document.
// The setup:
//
//   - Commit C1 on main: items = [ {_id:1, v:1} ]
//   - Commit C2 on main: items = [ {_id:1, v:1}, {_id:2, v:2} ]  (adds _id:2)
//
// Reverting C2 should undo the addition of _id:2, leaving items = [ {_id:1, v:1} ].

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// revertVerifySetup creates the standard revert test scenario.
// Returns hashC1 (initial commit) and hashC2 (adds _id:2).
func revertVerifySetup(t *testing.T, env *dumboDBTestEnv, dbName string) (hashC1, hashC2 string) {
	t.Helper()

	ctx := context.Background()
	db := env.client.Database(dbName)

	require.NoError(t, db.Drop(ctx))

	// C1: initial commit on main.
	_, err := db.Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "v", Value: int32(1)},
	})
	require.NoError(t, err)
	hashC1 = dumboDBCommit(t, env, dbName, "initial", "alice <alice@dumbodb>")

	// C2: add _id:2 — this is the commit we will revert.
	_, err = db.Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "v", Value: int32(2)},
	})
	require.NoError(t, err)
	hashC2 = dumboDBCommit(t, env, dbName, "add-two", "bob <bob@dumbodb>")
	require.NotEmpty(t, hashC2, "C2 commit hash must not be empty")

	return hashC1, hashC2
}

func TestRevertVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("revertvrfy%d", rand.Int64N(1_000_000))

	hashC1, hashC2 := revertVerifySetup(t, env, dbName)
	_ = hashC1

	mainDB := env.client.Database(dbName + "@main")

	// -------------------------------------------------------------------------
	// Scenario 1: Clean revert — response shape and commit annotation
	// -------------------------------------------------------------------------
	t.Run("Scenario1_CleanRevert_ResponseShape", func(t *testing.T) {
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "doltRevert", Value: int32(1)},
			{Key: "commit", Value: hashC2},
		})

		assert.EqualValues(t, 1, raw["ok"], "ok must be 1")

		commitID, ok := raw["commitId"].(string)
		require.True(t, ok, "commitId must be a string, got %T", raw["commitId"])
		assert.NotEmpty(t, commitID, "commitId must not be empty")
		assert.NotEqual(t, hashC2, commitID, "revert creates a new commit distinct from the reverted source")

		msg, ok := raw["message"].(string)
		require.True(t, ok, "message must be a string")
		assert.Contains(t, msg, "add-two", "message must reference the reverted commit's original message")
		assert.Contains(t, msg, "This reverts commit", "message must contain the revert annotation")
		assert.Contains(t, msg, hashC2, "message must contain the reverted commit hash")

		// For reverts, committer == author.
		assert.NotNil(t, raw["committer"], "committer must be present in revert response")
		assert.NotNil(t, raw["committerTimestamp"], "committerTimestamp must be present in revert response")
	})

	// Verify the revert undid the changes (only _id:1 should remain).
	t.Run("Scenario1_CleanRevert_DataVisible", func(t *testing.T) {
		count, err := mainDB.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 1, count, "main must have 1 document after reverting the add-two commit")

		var doc bson.M
		err = mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc)
		require.NoError(t, err)
		assert.EqualValues(t, 1, doc["v"], "original document must still exist")
	})

	// Verify the revert commit is a single-parent commit (not a merge commit).
	t.Run("Scenario1_CleanRevert_SingleParent", func(t *testing.T) {
		var logResult bson.M
		err := mainDB.RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logResult)
		require.NoError(t, err)

		commits, ok := logResult["commits"].(bson.A)
		require.True(t, ok, "commits must be an array")
		require.NotEmpty(t, commits, "commits must not be empty")

		head := commits[0].(bson.M)
		_, hasParent2 := head["parent2"]
		assert.False(t, hasParent2, "revert commit must have no parent2 (single-parent)")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: Custom message and author override
	// -------------------------------------------------------------------------
	t.Run("Scenario2_CustomMessageAndAuthor", func(t *testing.T) {
		// Add another commit to revert.
		_, err := mainDB.Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)},
			{Key: "v", Value: int32(3)},
		})
		require.NoError(t, err)
		hashC3 := dumboDBCommit(t, env, dbName+"@main", "add-three", "carol <carol@dumbodb>")

		// Revert with custom message and author.
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "doltRevert", Value: int32(1)},
			{Key: "commit", Value: hashC3},
			{Key: "message", Value: "undo: add item three"},
			{Key: "author", Value: "alice <alice@dumbodb>"},
		})

		assert.EqualValues(t, 1, raw["ok"], "ok must be 1")
		assert.Equal(t, "undo: add item three", raw["message"], "custom message must be used verbatim")

		// No annotation when a custom message is provided.
		msg := raw["message"].(string)
		assert.False(t, strings.Contains(msg, "This reverts commit"), "custom message must NOT include the default annotation")

		// For reverts, committer equals author.
		assert.Equal(t, raw["author"], raw["committer"], "committer must equal author for revert")
		assert.NotNil(t, raw["committerTimestamp"], "committerTimestamp must be present in revert response")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: Conflict during revert — structured error response
	// -------------------------------------------------------------------------
	var hashConflictCommit string
	t.Run("Scenario3_ConflictResponse", func(t *testing.T) {
		// Create "feature" branch from current main HEAD.
		var branchResult bson.M
		err := mainDB.RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "conflict-feat"},
		}).Decode(&branchResult)
		require.NoError(t, err)
		assert.Equal(t, "conflict-feat", branchResult["branch"])

		// Add a document on feature that will be referenced in the commit to revert.
		featDB := env.client.Database(dbName + "@conflict-feat")
		_, err = featDB.Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(10)},
			{Key: "v", Value: int32(10)},
		})
		require.NoError(t, err)
		hashConflictCommit = dumboDBCommit(t, env, dbName+"@conflict-feat", "add-ten", "alice <alice@dumbodb>")

		// Advance main independently: also modify _id:1 to create a conflict scenario.
		// We now revert hashConflictCommit (which added _id:10) while also having
		// modified _id:10 on main — creating a conflict.
		// First merge feature into main so main has _id:10.
		var mergeResult bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "merge_in", Value: "conflict-feat"},
			{Key: "message", Value: "merge conflict-feat"},
			{Key: "author", Value: "alice <alice@dumbodb>"},
		}).Decode(&mergeResult)
		require.NoError(t, err, "merge must succeed cleanly")
		require.EqualValues(t, 1, mergeResult["ok"])

		// Now modify _id:10 on main so reverting hashConflictCommit causes a conflict:
		// reverting would delete _id:10 (it was added by hashConflictCommit) but we
		// modified it after the merge, so there's a conflict.
		_, err = mainDB.Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(10)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(99)}}}},
		)
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@main", "modify-ten", "alice <alice@dumbodb>")

		// Revert hashConflictCommit — expect conflict because _id:10 was modified after it was added.
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "doltRevert", Value: int32(1)},
			{Key: "commit", Value: hashConflictCommit},
		})

		assert.EqualValues(t, 0, raw["ok"], "ok must be 0 when conflicts exist")

		conflicts, ok := raw["conflicts"].(bson.A)
		require.True(t, ok, "conflicts must be an array, got %T", raw["conflicts"])
		require.NotEmpty(t, conflicts, "conflicts must be non-empty")

		first := conflicts[0].(bson.M)
		assert.Equal(t, "items", first["collection"], "conflict collection must be 'items'")
		count, _ := first["count"].(int32)
		assert.Greater(t, count, int32(0), "conflict count must be > 0")

		errmsg, _ := raw["errmsg"].(string)
		assert.Contains(t, errmsg, "doltRevert", "errmsg must mention the command")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: Inspect conflicts, resolve, and continue
	// -------------------------------------------------------------------------
	t.Run("Scenario4_ResolveAndContinue", func(t *testing.T) {
		// doltConflicts and doltResolveConflict are the same interface used for
		// merge/cherry-pick conflicts. The revert state is stored in the same
		// mergeState struct (with isRevert=true).

		// Step 1: Summary — list which collections have unresolved conflicts.
		var summaryRes bson.M
		err := mainDB.RunCommand(ctx, bson.D{
			{Key: "doltConflicts", Value: int32(1)},
		}).Decode(&summaryRes)
		require.NoError(t, err, "doltConflicts summary must succeed while revert in progress")
		assert.EqualValues(t, 1, summaryRes["ok"])

		colls, ok := summaryRes["collections"].(bson.A)
		require.True(t, ok, "collections must be an array, got %T", summaryRes["collections"])
		require.Len(t, colls, 1, "one collection must have conflicts")
		collEntry := colls[0].(bson.M)
		assert.Equal(t, "items", collEntry["name"])
		assert.EqualValues(t, 1, collEntry["count"])

		// Step 2: Per-collection detail — list individual document conflicts.
		var conflictsRes bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "doltConflicts", Value: int32(1)},
			{Key: "collection", Value: "items"},
		}).Decode(&conflictsRes)
		require.NoError(t, err)
		assert.EqualValues(t, 1, conflictsRes["ok"])

		conflicts, ok := conflictsRes["conflicts"].(bson.A)
		require.True(t, ok, "conflicts must be an array")
		require.Len(t, conflicts, 1, "must have exactly one conflict in 'items'")

		cf := conflicts[0].(bson.M)
		conflictID, ok := cf["conflictId"].(string)
		require.True(t, ok, "conflictId must be a string")
		require.NotEmpty(t, conflictID, "conflictId must not be empty")

		// Step 3: Resolve — accept "ours" (keep the modified main version).
		var resolveRes bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "items"},
			{Key: "conflictId", Value: conflictID},
			{Key: "resolution", Value: "ours"},
		}).Decode(&resolveRes)
		require.NoError(t, err)
		assert.EqualValues(t, 1, resolveRes["ok"])

		// Step 4: After resolution, doltConflicts summary must return an empty collections array.
		var postResolveRes bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "doltConflicts", Value: int32(1)},
		}).Decode(&postResolveRes)
		require.NoError(t, err)
		postColls, ok := postResolveRes["collections"].(bson.A)
		require.True(t, ok, "collections must be an array after resolution")
		assert.Len(t, postColls, 0, "no more conflicts after resolution")

		// Step 5: Continue the revert.
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "doltRevert", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		})

		assert.EqualValues(t, 1, raw["ok"], "ok must be 1 after continue")
		commitID, ok := raw["commitId"].(string)
		require.True(t, ok, "commitId must be a string")
		assert.NotEmpty(t, commitID, "commitId must not be empty")
		assert.NotNil(t, raw["committer"], "committer must be present in revert continue response")
		assert.NotNil(t, raw["committerTimestamp"], "committerTimestamp must be present in revert continue response")
	})

	// -------------------------------------------------------------------------
	// Scenario 5: Abort revert in progress
	// -------------------------------------------------------------------------
	t.Run("Scenario5_AbortRevert", func(t *testing.T) {
		// Add a new commit to revert that will conflict.
		_, err := mainDB.Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(20)},
			{Key: "v", Value: int32(20)},
		})
		require.NoError(t, err)
		hashNew := dumboDBCommit(t, env, dbName+"@main", "add-twenty", "alice <alice@dumbodb>")

		// Modify _id:20 to create a conflict when we revert the add.
		_, err = mainDB.Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(20)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(201)}}}},
		)
		require.NoError(t, err)
		mainHeadBeforeAbort := dumboDBCommit(t, env, dbName+"@main", "modify-twenty", "alice <alice@dumbodb>")

		// Start revert — expect conflict.
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "doltRevert", Value: int32(1)},
			{Key: "commit", Value: hashNew},
		})
		require.EqualValues(t, 0, raw["ok"], "revert must produce conflict")

		// Abort.
		var abortRes bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "doltRevert", Value: int32(1)},
			{Key: "abort", Value: int32(1)},
		}).Decode(&abortRes)
		require.NoError(t, err)
		assert.EqualValues(t, 1, abortRes["ok"], "abort must succeed")
		assert.Equal(t, "revert aborted", abortRes["message"])

		// After abort, main HEAD must be unchanged.
		var logRes bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logRes)
		require.NoError(t, err)
		commits := logRes["commits"].(bson.A)
		require.NotEmpty(t, commits)
		headAfterAbort := commits[0].(bson.M)["commitId"].(string)
		assert.Equal(t, mainHeadBeforeAbort, headAfterAbort, "main HEAD must be unchanged after abort")
	})

	// -------------------------------------------------------------------------
	// Scenario 6: Error cases
	// -------------------------------------------------------------------------
	t.Run("Scenario6_ErrorCases", func(t *testing.T) {
		// Missing commit parameter.
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "doltRevert", Value: int32(1)},
		})
		assert.EqualValues(t, 0, raw["ok"], "missing commit must return ok:0")

		// Abort when no revert in progress.
		rawAbort := runCommandRaw(t, mainDB, bson.D{
			{Key: "doltRevert", Value: int32(1)},
			{Key: "abort", Value: int32(1)},
		})
		assert.EqualValues(t, 0, rawAbort["ok"], "abort with no in-progress must return ok:0")
		errmsg, _ := rawAbort["errmsg"].(string)
		assert.Contains(t, errmsg, "no revert in progress", "abort error must mention no revert in progress")

		// Continue when no revert in progress.
		rawContinue := runCommandRaw(t, mainDB, bson.D{
			{Key: "doltRevert", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		})
		assert.EqualValues(t, 0, rawContinue["ok"], "continue with no in-progress must return ok:0")
		continueErrmsg, _ := rawContinue["errmsg"].(string)
		assert.Contains(t, continueErrmsg, "no revert in progress", "continue error must mention no revert in progress")
	})
}
