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

// TestCherryPickVerify is the automated analog of docs/verify/cherry-pick.md.
//
// Each top-level subtest corresponds to one scenario in that document.
// The setup reproduces the manual setup block exactly:
//
//   - Commit C1 on main: items = [ {_id:1, v:1} ]
//   - Branch "feature" pointing at C1
//   - Commit C2 on feature: items = [ {_id:1, v:1}, {_id:2, v:2} ]
//
// Subtests run sequentially (no t.Parallel inside) so they share a single
// database and the side effects of one scenario carry into the next.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// cherryPickVerifySetup mirrors the Setup section of docs/verify/cherry-pick.md.
// Returns hashC1 (main HEAD, initial commit) and hashC2 (feature HEAD, adds _id:2).
func cherryPickVerifySetup(t *testing.T, env *docudoltTestEnv, dbName string) (hashC1, hashC2 string) {
	t.Helper()

	ctx := context.Background()
	db := env.client.Database(dbName)

	require.NoError(t, db.Drop(ctx))

	// Baseline: one document on main.
	_, err := db.Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "v", Value: int32(1)},
	})
	require.NoError(t, err)
	hashC1 = docudoltCommit(t, env, dbName, "initial")

	// Create "feature" branch from main HEAD.
	var branchResult bson.M
	err = env.client.Database(dbName+"__d_main").RunCommand(ctx, bson.D{
		{Key: "docudoltBranch", Value: int32(1)},
		{Key: "branch", Value: "feature"},
	}).Decode(&branchResult)
	require.NoError(t, err, "docudoltBranch to create 'feature'")
	assert.Equal(t, "feature", branchResult["branch"])

	// Advance feature with a commit that adds _id:2.
	_, err = env.client.Database(dbName+"__d_feature").Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "v", Value: int32(2)},
	})
	require.NoError(t, err)

	hashC2 = docudoltCommit(t, env, dbName+"__d_feature", "add-two", "bob <bob@docudolt>")
	require.NotEmpty(t, hashC2, "feature commit hash must not be empty")

	return hashC1, hashC2
}

func TestCherryPickVerify(t *testing.T) {
	env := startDocudolt(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("pickvrfy%d", rand.Int64N(1_000_000))

	hashC1, hashC2 := cherryPickVerifySetup(t, env, dbName)
	_ = hashC1

	mainDB := env.client.Database(dbName + "__d_main")

	// -------------------------------------------------------------------------
	// Scenario 1: Clean cherry-pick — response shape and commit annotation
	// -------------------------------------------------------------------------
	t.Run("Scenario1_CleanCherryPick_ResponseShape", func(t *testing.T) {
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "docudoltCherryPick", Value: int32(1)},
			{Key: "commit", Value: hashC2},
		})

		assert.EqualValues(t, 1, raw["ok"], "ok must be 1")

		commitID, ok := raw["commitId"].(string)
		require.True(t, ok, "commitId must be a string, got %T", raw["commitId"])
		assert.NotEmpty(t, commitID, "commitId must not be empty")
		assert.NotEqual(t, hashC2, commitID, "cherry-pick creates a new commit distinct from the source")

		msg, ok := raw["message"].(string)
		require.True(t, ok, "message must be a string")
		assert.Contains(t, msg, "add-two", "message must include original commit message")
		assert.Contains(t, msg, "cherry picked from commit", "message must contain cherry-pick annotation")
		assert.Contains(t, msg, hashC2, "message must contain the source commit hash")
	})

	// Verify main now has both documents after the cherry-pick.
	t.Run("Scenario1_CleanCherryPick_DataVisible", func(t *testing.T) {
		count, err := mainDB.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 2, count, "main must have 2 documents after cherry-pick")
	})

	// Verify the cherry-pick commit is a single-parent commit (not a merge commit).
	t.Run("Scenario1_CleanCherryPick_SingleParent", func(t *testing.T) {
		var logResult bson.M
		err := mainDB.RunCommand(ctx, bson.D{
			{Key: "docudoltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logResult)
		require.NoError(t, err)

		commits, ok := logResult["commits"].(bson.A)
		require.True(t, ok, "commits must be an array")
		require.NotEmpty(t, commits, "commits must not be empty")

		head := commits[0].(bson.M)
		_, hasParent2 := head["parent2"]
		assert.False(t, hasParent2, "cherry-pick commit must have no parent2 (single-parent)")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: Custom message and author override
	// -------------------------------------------------------------------------
	t.Run("Scenario2_CustomMessageAndAuthor", func(t *testing.T) {
		// Add a new commit on feature to cherry-pick.
		_, err := env.client.Database(dbName+"__d_feature").Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)},
			{Key: "v", Value: int32(3)},
		})
		require.NoError(t, err)

		hashC3feat := docudoltCommit(t, env, dbName+"__d_feature", "add-three")

		// Cherry-pick with custom message and author.
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "docudoltCherryPick", Value: int32(1)},
			{Key: "commit", Value: hashC3feat},
			{Key: "message", Value: "port: add item three"},
			{Key: "author", Value: "alice <alice@docudolt>"},
		})

		assert.EqualValues(t, 1, raw["ok"], "ok must be 1")
		assert.Equal(t, "port: add item three", raw["message"], "custom message must be used verbatim")

		// No annotation when a custom message is provided.
		msg := raw["message"].(string)
		assert.False(t, strings.Contains(msg, "cherry picked from"), "custom message must NOT include the default annotation")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: Conflict during cherry-pick — structured error response
	// -------------------------------------------------------------------------
	var hashConflictFeat string
	t.Run("Scenario3_ConflictResponse", func(t *testing.T) {
		// Modify _id:1 on feature (will conflict with main's version).
		_, err := env.client.Database(dbName+"__d_feature").Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(99)}}}},
		)
		require.NoError(t, err)
		hashConflictFeat = docudoltCommit(t, env, dbName+"__d_feature", "conflict-source")

		// Modify _id:1 on main too (independent change to create conflict).
		_, err = mainDB.Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(100)}}}},
		)
		require.NoError(t, err)
		docudoltCommit(t, env, dbName+"__d_main", "conflict-target")

		// Cherry-pick — expect conflict error.
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "docudoltCherryPick", Value: int32(1)},
			{Key: "commit", Value: hashConflictFeat},
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
		assert.Contains(t, errmsg, "docudoltCherryPick", "errmsg must mention the command")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: Inspect conflicts, resolve, and continue
	// -------------------------------------------------------------------------
	t.Run("Scenario4_ResolveAndContinue", func(t *testing.T) {
		// Inspect per-collection conflict list.
		var conflictsRes bson.M
		err := mainDB.RunCommand(ctx, bson.D{
			{Key: "docudoltConflicts", Value: int32(1)},
			{Key: "collection", Value: "items"},
		}).Decode(&conflictsRes)
		require.NoError(t, err, "docudoltConflicts must succeed while cherry-pick in progress")

		conflicts, ok := conflictsRes["conflicts"].(bson.A)
		require.True(t, ok, "conflicts must be an array")
		require.NotEmpty(t, conflicts, "must have at least one conflict")

		conflictID := conflicts[0].(bson.M)["conflictId"].(string)
		require.NotEmpty(t, conflictID, "conflictId must not be empty")

		// Resolve: accept "theirs" (the cherry-picked value v:99).
		var resolveRes bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "docudoltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "items"},
			{Key: "conflictId", Value: conflictID},
			{Key: "resolution", Value: "theirs"},
		}).Decode(&resolveRes)
		require.NoError(t, err)
		assert.EqualValues(t, 1, resolveRes["ok"])

		// Continue the cherry-pick.
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "docudoltCherryPick", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		})

		assert.EqualValues(t, 1, raw["ok"], "ok must be 1 after continue")
		commitID, ok := raw["commitId"].(string)
		require.True(t, ok, "commitId must be a string")
		assert.NotEmpty(t, commitID, "commitId must not be empty")

		// Verify the resolved value is visible.
		var doc bson.M
		err = mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc)
		require.NoError(t, err)
		assert.EqualValues(t, 99, doc["v"], "resolved value should be 99 (theirs)")
	})

	// -------------------------------------------------------------------------
	// Scenario 5: Abort cherry-pick in progress
	// -------------------------------------------------------------------------
	t.Run("Scenario5_AbortCherryPick", func(t *testing.T) {
		// Create another conflicting commit on feature.
		_, err := env.client.Database(dbName+"__d_feature").Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(200)}}}},
		)
		require.NoError(t, err)
		hashConflict2 := docudoltCommit(t, env, dbName+"__d_feature", "another-conflict")

		// Create conflicting change on main.
		_, err = mainDB.Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(201)}}}},
		)
		require.NoError(t, err)
		mainHeadBeforeAbort := docudoltCommit(t, env, dbName+"__d_main", "another-conflict-target")

		// Start cherry-pick — expect conflict.
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "docudoltCherryPick", Value: int32(1)},
			{Key: "commit", Value: hashConflict2},
		})
		require.EqualValues(t, 0, raw["ok"], "cherry-pick must produce conflict")

		// Abort.
		var abortRes bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "docudoltCherryPick", Value: int32(1)},
			{Key: "abort", Value: int32(1)},
		}).Decode(&abortRes)
		require.NoError(t, err)
		assert.EqualValues(t, 1, abortRes["ok"], "abort must succeed")
		assert.Equal(t, "cherry-pick aborted", abortRes["message"])

		// After abort, main HEAD must be unchanged.
		var logRes bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "docudoltLog", Value: int32(1)},
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
			{Key: "docudoltCherryPick", Value: int32(1)},
		})
		assert.EqualValues(t, 0, raw["ok"], "missing commit must return ok:0")

		// Abort when no cherry-pick in progress.
		rawAbort := runCommandRaw(t, mainDB, bson.D{
			{Key: "docudoltCherryPick", Value: int32(1)},
			{Key: "abort", Value: int32(1)},
		})
		assert.EqualValues(t, 0, rawAbort["ok"], "abort with no in-progress must return ok:0")
		errmsg, _ := rawAbort["errmsg"].(string)
		assert.Contains(t, errmsg, "no cherry-pick in progress", "abort error must mention no cherry-pick in progress")

		// Continue when no cherry-pick in progress.
		rawContinue := runCommandRaw(t, mainDB, bson.D{
			{Key: "docudoltCherryPick", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		})
		assert.EqualValues(t, 0, rawContinue["ok"], "continue with no in-progress must return ok:0")
		continueErrmsg, _ := rawContinue["errmsg"].(string)
		assert.Contains(t, continueErrmsg, "no cherry-pick in progress", "continue error must mention no cherry-pick in progress")
	})
}
