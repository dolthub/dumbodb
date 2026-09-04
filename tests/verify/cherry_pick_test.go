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

// Subtests run sequentially so they share a single database and side effects of
// one scenario carry into the next.

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

// cherryPickVerifySetup returns hashC1 (main HEAD, initial commit) and hashC2
// (feature HEAD, adds _id:2).
func cherryPickVerifySetup(t *testing.T, env *dumboDBTestEnv, dbName string) (hashC1, hashC2 string) {
	t.Helper()

	ctx := context.Background()
	db := env.Client.Database(dbName)

	require.NoError(t, db.Drop(ctx))

	_, err := db.Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "v", Value: int32(1)},
	})
	require.NoError(t, err)
	hashC1 = dumboDBCommit(t, env, dbName, "initial", "alice <alice@acme.com>")

	var branchResult bson.M
	err = env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "action", Value: "add"},
		{Key: "branch", Value: "feature"},
	}).Decode(&branchResult)
	require.NoError(t, err, "doltBranch to create 'feature'")
	assert.Equal(t, "feature", branchResult["branch"])

	_, err = env.Client.Database(dbName+"@feature").Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "v", Value: int32(2)},
	})
	require.NoError(t, err)

	hashC2 = dumboDBCommit(t, env, dbName+"@feature", "add-two", "bob <bob@widgets.io>")
	require.NotEmpty(t, hashC2, "feature commit hash must not be empty")

	return hashC1, hashC2
}

func TestCherryPickVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("pickvrfy%d", rand.Int64N(1_000_000))

	hashC1, hashC2 := cherryPickVerifySetup(t, env, dbName)
	_ = hashC1

	mainDB := env.Client.Database(dbName + "@main")

	t.Run("Scenario1_CleanCherryPick_ResponseShape", func(t *testing.T) {
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboCherryPick", Value: int32(1)},
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

		// Cherry-pick without explicit committer: committer equals the original author.
		assert.Equal(t, "bob <bob@widgets.io>", raw["author"], "cherry-pick must preserve original commit author")
		assert.Equal(t, raw["author"], raw["committer"],
			"without committer param, committer must equal original author")
		assert.NotNil(t, raw["committerTimestamp"], "committerTimestamp must be present")
	})

	t.Run("Scenario1_CleanCherryPick_DataVisible", func(t *testing.T) {
		count, err := mainDB.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 2, count, "main must have 2 documents after cherry-pick")
	})

	t.Run("Scenario1_CleanCherryPick_SingleParent", func(t *testing.T) {
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
		assert.False(t, hasParent2, "cherry-pick commit must have no parent2 (single-parent)")
	})

	t.Run("Scenario2_CustomMessageAndAuthor", func(t *testing.T) {
		_, err := env.Client.Database(dbName+"@feature").Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)},
			{Key: "v", Value: int32(3)},
		})
		require.NoError(t, err)

		hashC3feat := dumboDBCommit(t, env, dbName+"@feature", "add-three", "carol <carol@startup.dev>")

		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboCherryPick", Value: int32(1)},
			{Key: "commit", Value: hashC3feat},
			{Key: "message", Value: "port: add item three"},
			{Key: "committer", Value: "alice <alice@acme.com>"},
		})

		assert.EqualValues(t, 1, raw["ok"], "ok must be 1")
		assert.Equal(t, "port: add item three", raw["message"], "custom message must be used verbatim")

		// No annotation when a custom message is provided.
		msg := raw["message"].(string)
		assert.False(t, strings.Contains(msg, "cherry picked from"), "custom message must NOT include the default annotation")

		// The original commit was by "carol <carol@startup.dev>"; the cherry-pick
		// committer parameter ("alice") becomes the committer identity. The original
		// author ("carol") is preserved.
		assert.Equal(t, "carol <carol@startup.dev>", raw["author"],
			"cherry-pick must preserve original commit author (carol)")
		assert.Equal(t, "alice <alice@acme.com>", raw["committer"],
			"committer must be the cherry-picker (alice, from the committer param)")
		assert.NotEqual(t, raw["author"], raw["committer"],
			"committer must differ from author when explicit committer is provided")
		assert.NotNil(t, raw["committerTimestamp"], "committerTimestamp must be present")
	})

	var hashConflictFeat string
	t.Run("Scenario3_ConflictResponse", func(t *testing.T) {
		_, err := env.Client.Database(dbName+"@feature").Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(99)}}}},
		)
		require.NoError(t, err)
		hashConflictFeat = dumboDBCommit(t, env, dbName+"@feature", "conflict-source", "alice <alice@acme.com>")

		_, err = mainDB.Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(100)}}}},
		)
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@main", "conflict-target", "alice <alice@acme.com>")

		// On conflict the server returns ok:0 with code 96; runCommandRaw captures
		// that response document directly so the structured error fields are
		// assertable (the driver would otherwise raise a bare CommandError).
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboCherryPick", Value: int32(1)},
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
		assert.Contains(t, errmsg, "dumboCherryPick", "errmsg must mention the command")

		var statusRaw bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&statusRaw)
		require.NoError(t, err)
		assert.Equal(t, "cherry-pick", statusRaw["mergeState"], "mergeState must indicate cherry-pick in progress")
		assert.Equal(t, true, statusRaw["dirty"], "workspace must be dirty during cherry-pick conflict")
		assert.Nil(t, statusRaw["commitId"], "commitId must be absent during cherry-pick conflict")
		statusConflicts, ok2 := statusRaw["conflicts"].(bson.A)
		require.True(t, ok2, "conflicts must be present during cherry-pick")
		require.Len(t, statusConflicts, 1)
		sc := statusConflicts[0].(bson.M)
		assert.Equal(t, "items", sc["collection"])
		assert.EqualValues(t, 1, sc["count"])
	})

	t.Run("Scenario4_ResolveAndContinue", func(t *testing.T) {
		// Cherry-pick state is stored in the same mergeState struct (isCherryPick=true),
		// so doltConflicts and doltResolveConflict work identically to merge conflicts.
		var conflictsRes bson.M
		err := mainDB.RunCommand(ctx, bson.D{
			{Key: "doltConflicts", Value: int32(1)},
		}).Decode(&conflictsRes)
		require.NoError(t, err, "doltConflicts must succeed while cherry-pick in progress")
		assert.EqualValues(t, 1, conflictsRes["ok"])

		conflicts, ok := conflictsRes["conflicts"].(bson.A)
		require.True(t, ok, "conflicts must be an array, got %T", conflictsRes["conflicts"])
		require.Len(t, conflicts, 1, "must have exactly one conflict in 'items'")

		cf := conflicts[0].(bson.M)
		assert.Equal(t, "items", cf["collection"])
		conflictID, ok := cf["conflictId"].(string)
		require.True(t, ok, "conflictId must be a string")
		require.NotEmpty(t, conflictID, "conflictId must not be empty")

		assert.Equal(t, "document", cf["type"])
		assert.Equal(t, "bothModified", cf["reason"].(bson.M)["code"])
		assert.Equal(t, fmt.Sprintf("branch 'main' (ours) and commit '%s' (theirs) both modified document 1", hashConflictFeat),
			cf["reason"].(bson.M)["message"])

		// Each non-null side carries its own _id and diffType.
		ours, ok := cf["ours"].(bson.M)
		require.True(t, ok, "ours must be a document")
		assert.EqualValues(t, int32(1), ours["_id"], "ours side must carry _id:1")
		assert.Equal(t, "modified", ours["diffType"])
		oursDoc := ours["doc"].(bson.M)
		assert.EqualValues(t, 100, oursDoc["v"], "ours doc must have v:100 (main's version)")
		assert.EqualValues(t, int32(1), oursDoc["_id"], "doc carries the full document including _id")

		theirs, ok := cf["theirs"].(bson.M)
		require.True(t, ok, "theirs must be a document")
		assert.EqualValues(t, int32(1), theirs["_id"], "theirs side must carry _id:1")
		assert.Equal(t, "modified", theirs["diffType"])
		theirsDoc := theirs["doc"].(bson.M)
		assert.EqualValues(t, 99, theirsDoc["v"], "theirs doc must have v:99 (cherry-picked version)")
		assert.EqualValues(t, int32(1), theirsDoc["_id"], "doc carries the full document including _id")

		var resolveRes bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "items"},
			{Key: "conflictId", Value: conflictID},
			{Key: "resolution", Value: "theirs"},
		}).Decode(&resolveRes)
		require.NoError(t, err)
		assert.EqualValues(t, 1, resolveRes["ok"])

		var postResolveRes bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "doltConflicts", Value: int32(1)},
		}).Decode(&postResolveRes)
		require.NoError(t, err)
		postConflicts, ok := postResolveRes["conflicts"].(bson.A)
		require.True(t, ok, "conflicts must be an array after resolution")
		assert.Len(t, postConflicts, 0, "no more conflicts after resolution")

		// Continue mirrors doltMerge continue:1.
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboCherryPick", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		})

		assert.EqualValues(t, 1, raw["ok"], "ok must be 1 after continue")
		commitID, ok := raw["commitId"].(string)
		require.True(t, ok, "commitId must be a string")
		assert.NotEmpty(t, commitID, "commitId must not be empty")
		// No committer param on continue: committer must equal the original author.
		assert.Equal(t, raw["author"], raw["committer"],
			"without committer param, committer must equal original author")
		assert.NotNil(t, raw["committerTimestamp"], "committerTimestamp must be present")

		var doc bson.M
		err = mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc)
		require.NoError(t, err)
		assert.EqualValues(t, 99, doc["v"], "resolved value should be 99 (theirs)")

		var cleanStatus bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&cleanStatus)
		require.NoError(t, err)
		assert.Nil(t, cleanStatus["mergeState"], "mergeState must be absent after resolution")
		assert.Nil(t, cleanStatus["conflicts"], "conflicts must be absent after resolution")
		assert.Equal(t, false, cleanStatus["dirty"], "workspace must not be dirty after resolution")
		assert.NotNil(t, cleanStatus["commitId"], "commitId must be present after resolution")
	})

	t.Run("Scenario5_AbortCherryPick", func(t *testing.T) {
		_, err := env.Client.Database(dbName+"@feature").Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(200)}}}},
		)
		require.NoError(t, err)
		hashConflict2 := dumboDBCommit(t, env, dbName+"@feature", "another-conflict", "alice <alice@acme.com>")

		_, err = mainDB.Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(201)}}}},
		)
		require.NoError(t, err)
		mainHeadBeforeAbort := dumboDBCommit(t, env, dbName+"@main", "another-conflict-target", "alice <alice@acme.com>")

		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboCherryPick", Value: int32(1)},
			{Key: "commit", Value: hashConflict2},
		})
		require.EqualValues(t, 0, raw["ok"], "cherry-pick must produce conflict")

		var abortRes bson.M
		err = mainDB.RunCommand(ctx, bson.D{
			{Key: "dumboCherryPick", Value: int32(1)},
			{Key: "abort", Value: int32(1)},
		}).Decode(&abortRes)
		require.NoError(t, err)
		assert.EqualValues(t, 1, abortRes["ok"], "abort must succeed")
		assert.Equal(t, "cherry-pick aborted", abortRes["message"])

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

	t.Run("Scenario6_ErrorCases", func(t *testing.T) {
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboCherryPick", Value: int32(1)},
		})
		assert.EqualValues(t, 0, raw["ok"], "missing commit must return ok:0")

		rawAbort := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboCherryPick", Value: int32(1)},
			{Key: "abort", Value: int32(1)},
		})
		assert.EqualValues(t, 0, rawAbort["ok"], "abort with no in-progress must return ok:0")
		errmsg, _ := rawAbort["errmsg"].(string)
		assert.Contains(t, errmsg, "no cherry-pick in progress", "abort error must mention no cherry-pick in progress")

		rawContinue := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboCherryPick", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		})
		assert.EqualValues(t, 0, rawContinue["ok"], "continue with no in-progress must return ok:0")
		continueErrmsg, _ := rawContinue["errmsg"].(string)
		assert.Contains(t, continueErrmsg, "no cherry-pick in progress", "continue error must mention no cherry-pick in progress")
	})
}
