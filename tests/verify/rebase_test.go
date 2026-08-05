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

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// rebaseVerifySetup returns hashC1 (main initial), hashC2 (feature adds _id:2),
// hashC3 (main adds _id:3), with feature diverged from main.
func rebaseVerifySetup(t *testing.T, env *dumboDBTestEnv, dbName string) (hashC1, hashC2, hashC3 string) {
	t.Helper()

	ctx := context.Background()
	db := env.Client.Database(dbName)

	require.NoError(t, db.Drop(ctx))

	_, err := db.Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "v", Value: int32(1)},
	})
	require.NoError(t, err)
	hashC1 = dumboDBCommit(t, env, dbName, "initial", "test <test@example.com>")

	var branchResult bson.M
	err = env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "feature"},
	}).Decode(&branchResult)
	require.NoError(t, err, "doltBranch to create 'feature'")

	_, err = env.Client.Database(dbName+"@feature").Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "v", Value: int32(2)},
	})
	require.NoError(t, err)
	hashC2 = dumboDBCommit(t, env, dbName+"@feature", "feature-adds-2", "test <test@example.com>")

	_, err = db.Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(3)},
		{Key: "v", Value: int32(3)},
	})
	require.NoError(t, err)
	hashC3 = dumboDBCommit(t, env, dbName, "main-adds-3", "test <test@example.com>")

	return hashC1, hashC2, hashC3
}

func TestRebaseVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("rebasvrfy%d", rand.Int64N(1_000_000))

	hashC1, hashC2, hashC3 := rebaseVerifySetup(t, env, dbName)
	_, _ = hashC1, hashC2

	featureDB := env.Client.Database(dbName + "@feature")

	t.Run("Scenario1_CleanRebase_ResponseShape", func(t *testing.T) {
		raw := runCommandRaw(t, featureDB, bson.D{
			{Key: "dumboRebase", Value: int32(1)},
			{Key: "onto", Value: "main"},
			{Key: "committer", Value: "rebaser <rebaser@acme.com>"},
		})

		assert.EqualValues(t, 1, raw["ok"], "ok must be 1 for a clean rebase")

		commitsReplayed, ok := raw["commitsReplayed"]
		require.True(t, ok, "commitsReplayed must be present")
		assert.EqualValues(t, 1, commitsReplayed, "one commit (C2) must have been replayed")

		newTip, ok := raw["newTip"].(string)
		require.True(t, ok, "newTip must be a string")
		assert.NotEmpty(t, newTip, "newTip must not be empty")
		assert.NotEqual(t, hashC2, newTip, "rebase creates new commits distinct from originals")
		assert.NotEqual(t, hashC3, newTip, "newTip is the rebased commit, not main's tip")
	})

	t.Run("Scenario1b_RebasedCommitCommitterIdentity", func(t *testing.T) {
		var logResult bson.M
		err := featureDB.RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logResult)
		require.NoError(t, err)

		commits, ok := logResult["commits"].(bson.A)
		require.True(t, ok, "commits must be an array")
		require.NotEmpty(t, commits, "commits array must not be empty")

		head := commits[0].(bson.M)
		assert.Equal(t, "test <test@example.com>", head["author"],
			"rebased commit must preserve original author")
		assert.Equal(t, "rebaser <rebaser@acme.com>", head["committer"],
			"rebased commit committer must be the rebaser, not the original author")
		assert.NotEqual(t, head["author"], head["committer"],
			"committer must differ from author for rebased commits")
		assert.NotNil(t, head["committerTimestamp"], "rebased commit must have committerTimestamp")
	})

	t.Run("Scenario1c_RebaseWithoutCommitter", func(t *testing.T) {
		noCommDB := fmt.Sprintf("rebasenocomm%d", rand.Int64N(1_000_000))
		_, _, _ = rebaseVerifySetup(t, env, noCommDB)

		noCommFeatureDB := env.Client.Database(noCommDB + "@feature")

		raw := runCommandRaw(t, noCommFeatureDB, bson.D{
			{Key: "dumboRebase", Value: int32(1)},
			{Key: "onto", Value: "main"},
		})
		assert.EqualValues(t, 1, raw["ok"])

		var logRes bson.M
		err := noCommFeatureDB.RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logRes)
		require.NoError(t, err)

		commits := logRes["commits"].(bson.A)
		head := commits[0].(bson.M)
		assert.Equal(t, "test <test@example.com>", head["author"],
			"rebased commit must preserve original author")
		assert.Equal(t, head["author"], head["committer"],
			"without committer param, committer must equal original author")
		assert.NotNil(t, head["committerTimestamp"])
	})

	t.Run("Scenario2_DataVisibleAfterRebase", func(t *testing.T) {
		count, err := featureDB.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 3, count, "feature must have 3 documents after rebase onto main")

		res := featureDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(3)}})
		require.NoError(t, res.Err(), "_id:3 must be visible on feature after rebase")

		res = featureDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
		require.NoError(t, res.Err(), "_id:2 must be present on feature after rebase")
	})

	t.Run("Scenario3_RebasedCommitSingleParent", func(t *testing.T) {
		var logResult bson.M
		err := featureDB.RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logResult)
		require.NoError(t, err)

		commits, ok := logResult["commits"].(bson.A)
		require.True(t, ok, "commits must be an array")
		require.NotEmpty(t, commits, "commits array must not be empty")

		head := commits[0].(bson.M)
		_, hasParent2 := head["parent2"]
		assert.False(t, hasParent2, "rebased commit must be single-parent (no parent2)")

		parent1, ok := head["parent1"].(string)
		require.True(t, ok, "parent1 must be a string")
		assert.Equal(t, hashC3, parent1, "rebased commit's parent must be main's C3")
	})

	t.Run("Scenario4_AlreadyUpToDate", func(t *testing.T) {
		// After Scenario 1, feature = C2' sitting on main's tip C3, and main has
		// not advanced. onto (main) is therefore already an ancestor of feature's
		// HEAD, so a second rebase replays nothing and leaves the tip unchanged --
		// matching git's "Current branch is up to date". (Replaying the lone
		// feature commit again would duplicate it on every rebase.)
		var before bson.M
		require.NoError(t, featureDB.RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&before))
		tipBefore := before["commitId"]
		require.NotNil(t, tipBefore, "feature must have a HEAD before the no-op rebase")

		raw := runCommandRaw(t, featureDB, bson.D{
			{Key: "dumboRebase", Value: int32(1)},
			{Key: "onto", Value: "main"},
		})

		assert.EqualValues(t, 1, raw["ok"], "ok must be 1 for an up-to-date rebase")
		assert.EqualValues(t, 0, raw["commitsReplayed"], "nothing to replay when already based on onto")
		assert.Equal(t, tipBefore, raw["newTip"], "tip must be unchanged -- no commit duplicated")
	})

	t.Run("Scenario5_AbortNoRebase", func(t *testing.T) {
		raw := runCommandRaw(t, featureDB, bson.D{
			{Key: "dumboRebase", Value: int32(1)},
			{Key: "abort", Value: int32(1)},
		})
		assert.EqualValues(t, 0, raw["ok"], "abort with no rebase must fail")
		assert.NotEmpty(t, raw["errmsg"], "must include error message")
	})

	t.Run("Scenario6_ThreeCommitRebase", func(t *testing.T) {
		tdb := fmt.Sprintf("rebase3c%d", rand.Int64N(1_000_000))
		mainCol := env.Client.Database(tdb + "@main").Collection("items")

		_, err := mainCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: int32(1)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@main", "C1", "alice <alice@acme.com>")

		require.NoError(t, env.Client.Database(tdb+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "feature"},
		}).Err())

		featCol := env.Client.Database(tdb + "@feature").Collection("items")

		_, err = featCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(10)}, {Key: "v", Value: int32(10)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@feature", "F1", "bob <bob@widgets.io>")

		_, err = featCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(11)}, {Key: "v", Value: int32(11)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@feature", "F2", "bob <bob@widgets.io>")

		_, err = featCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(12)}, {Key: "v", Value: int32(12)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@feature", "F3", "bob <bob@widgets.io>")

		_, err = mainCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: int32(2)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@main", "C2", "alice <alice@acme.com>")

		raw := runCommandRaw(t, env.Client.Database(tdb+"@feature"), bson.D{
			{Key: "dumboRebase", Value: int32(1)},
			{Key: "onto", Value: "main"},
		})
		assert.EqualValues(t, 1, raw["ok"], "three-commit rebase must succeed")
		assert.EqualValues(t, 3, raw["commitsReplayed"], "all 3 feature commits must be replayed")

		n, err := env.Client.Database(tdb+"@feature").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(5), n, "feature must have 5 docs (C1 + C2 + F1 + F2 + F3)")
	})

	t.Run("Scenario7_ThreeCommit_FirstConflicts", func(t *testing.T) {
		tdb := fmt.Sprintf("rebase3cf%d", rand.Int64N(1_000_000))
		mainCol := env.Client.Database(tdb + "@main").Collection("items")

		_, err := mainCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: int32(1)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@main", "C1", "alice <alice@acme.com>")

		require.NoError(t, env.Client.Database(tdb+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "feature"},
		}).Err())

		featCol := env.Client.Database(tdb + "@feature").Collection("items")

		// F1 modifies _id:1, conflicting with main's C2 below.
		_, err = featCol.UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(100)}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@feature", "F1-conflict", "bob <bob@widgets.io>")

		_, err = featCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(10)}, {Key: "v", Value: int32(10)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@feature", "F2-clean", "bob <bob@widgets.io>")

		_, err = featCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(11)}, {Key: "v", Value: int32(11)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@feature", "F3-clean", "bob <bob@widgets.io>")

		_, err = mainCol.UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(200)}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@main", "C2-conflict", "alice <alice@acme.com>")

		featDB := env.Client.Database(tdb + "@feature")

		raw := runCommandRaw(t, featDB, bson.D{
			{Key: "dumboRebase", Value: int32(1)},
			{Key: "onto", Value: "main"},
		})
		require.EqualValues(t, 0, raw["ok"], "rebase must conflict on F1")

		var conflictsRes bson.M
		require.NoError(t, featDB.RunCommand(ctx, bson.D{
			{Key: "doltConflicts", Value: int32(1)},
		}).Decode(&conflictsRes))
		cfl := conflictsRes["conflicts"].(bson.A)
		cid := cfl[0].(bson.M)["conflictId"].(string)

		var resolveRes bson.M
		require.NoError(t, featDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "items"},
			{Key: "conflictId", Value: cid},
			{Key: "resolution", Value: "ours"},
		}).Decode(&resolveRes))
		assert.EqualValues(t, 1, resolveRes["ok"])

		contRaw := runCommandRaw(t, featDB, bson.D{
			{Key: "dumboRebase", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		})
		assert.EqualValues(t, 1, contRaw["ok"], "continue must succeed")
		assert.EqualValues(t, 3, contRaw["commitsReplayed"], "all 3 commits must be replayed")

		n, err := featDB.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), n, "feature must have 3 docs (_id:1 + _id:10 + _id:11)")
	})

	t.Run("Scenario8_ThreeCommit_ThirdConflicts", func(t *testing.T) {
		tdb := fmt.Sprintf("rebase3cl%d", rand.Int64N(1_000_000))
		mainCol := env.Client.Database(tdb + "@main").Collection("items")

		_, err := mainCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: int32(1)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@main", "C1", "alice <alice@acme.com>")

		require.NoError(t, env.Client.Database(tdb+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "feature"},
		}).Err())

		featCol := env.Client.Database(tdb + "@feature").Collection("items")

		_, err = featCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(10)}, {Key: "v", Value: int32(10)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@feature", "F1-clean", "bob <bob@widgets.io>")

		_, err = featCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(11)}, {Key: "v", Value: int32(11)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@feature", "F2-clean", "bob <bob@widgets.io>")

		// F3 modifies _id:1, conflicting with main's C2 below.
		_, err = featCol.UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(100)}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@feature", "F3-conflict", "bob <bob@widgets.io>")

		_, err = mainCol.UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(200)}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, tdb+"@main", "C2-conflict", "alice <alice@acme.com>")

		featDB := env.Client.Database(tdb + "@feature")

		raw := runCommandRaw(t, featDB, bson.D{
			{Key: "dumboRebase", Value: int32(1)},
			{Key: "onto", Value: "main"},
		})
		require.EqualValues(t, 0, raw["ok"], "rebase must conflict on F3")

		// After the rebase swap, "theirs" is the onto/main side (v:200); "ours"
		// would be the replayed feature commit (v:100).
		var conflictsRes bson.M
		require.NoError(t, featDB.RunCommand(ctx, bson.D{
			{Key: "doltConflicts", Value: int32(1)},
		}).Decode(&conflictsRes))
		cfl := conflictsRes["conflicts"].(bson.A)
		cid := cfl[0].(bson.M)["conflictId"].(string)

		var resolveRes bson.M
		require.NoError(t, featDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "items"},
			{Key: "conflictId", Value: cid},
			{Key: "resolution", Value: "theirs"},
		}).Decode(&resolveRes))
		assert.EqualValues(t, 1, resolveRes["ok"])

		contRaw := runCommandRaw(t, featDB, bson.D{
			{Key: "dumboRebase", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		})
		assert.EqualValues(t, 1, contRaw["ok"], "continue must succeed")
		assert.EqualValues(t, 3, contRaw["commitsReplayed"], "all 3 commits must be replayed")

		var doc bson.M
		require.NoError(t, featDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
		assert.EqualValues(t, 200, doc["v"], "theirs resolution keeps onto/main's value")

		n, err := featDB.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), n, "feature must have 3 docs (_id:1 + _id:10 + _id:11)")
	})

	t.Run("Scenario9_ConflictResponse", func(t *testing.T) {
		conflictDB := fmt.Sprintf("rebaseconfl%d", rand.Int64N(1_000_000))
		_, hashC2c, _ := rebaseVerifySetup(t, env, conflictDB)
		_ = hashC2c

		conflictFeatureDB := env.Client.Database(conflictDB + "@feature")

		conflictMainDB := env.Client.Database(conflictDB + "@main")
		_, err := conflictMainDB.Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(100)}}}},
		)
		require.NoError(t, err)
		dumboDBCommit(t, env, conflictDB+"@main", "main-changes-1", "test <test@example.com>")

		_, err = conflictFeatureDB.Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(200)}}}},
		)
		require.NoError(t, err)
		dumboDBCommit(t, env, conflictDB+"@feature", "feature-changes-1", "test <test@example.com>")

		raw := runCommandRaw(t, conflictFeatureDB, bson.D{
			{Key: "dumboRebase", Value: int32(1)},
			{Key: "onto", Value: "main"},
		})

		assert.EqualValues(t, 0, raw["ok"], "ok must be 0 when rebase conflicts exist")

		conflicts, ok := raw["conflicts"].(bson.A)
		require.True(t, ok, "conflicts must be an array")
		require.NotEmpty(t, conflicts, "conflicts must be non-empty")

		first := conflicts[0].(bson.M)
		assert.Equal(t, "items", first["collection"], "conflict collection must be 'items'")

		conflictCommit, ok := raw["conflictCommit"].(string)
		require.True(t, ok, "conflictCommit must be a string")
		assert.NotEmpty(t, conflictCommit, "conflictCommit must not be empty")

		errmsg, _ := raw["errmsg"].(string)
		assert.Contains(t, errmsg, "dumboRebase", "errmsg must mention the command")

		abortRaw := runCommandRaw(t, conflictFeatureDB, bson.D{
			{Key: "dumboRebase", Value: int32(1)},
			{Key: "abort", Value: int32(1)},
		})
		assert.EqualValues(t, 1, abortRaw["ok"], "abort must succeed")
		_, hasTip := abortRaw["newTip"]
		assert.True(t, hasTip, "abort response must include newTip")
	})

	t.Run("Scenario10_ConflictResolveAndContinue", func(t *testing.T) {
		conflictDB := fmt.Sprintf("rebaseresol%d", rand.Int64N(1_000_000))
		_, _, _ = rebaseVerifySetup(t, env, conflictDB)

		conflictFeatureDB := env.Client.Database(conflictDB + "@feature")
		conflictMainDB := env.Client.Database(conflictDB + "@main")

		_, err := conflictMainDB.Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(100)}}}},
		)
		require.NoError(t, err)
		dumboDBCommit(t, env, conflictDB+"@main", "main-modifies-1", "test <test@example.com>")

		_, err = conflictFeatureDB.Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(200)}}}},
		)
		require.NoError(t, err)
		featModHash := dumboDBCommit(t, env, conflictDB+"@feature", "feature-modifies-1", "test <test@example.com>")

		raw := runCommandRaw(t, conflictFeatureDB, bson.D{
			{Key: "dumboRebase", Value: int32(1)},
			{Key: "onto", Value: "main"},
		})
		require.EqualValues(t, 0, raw["ok"], "rebase must conflict")
		require.NotEmpty(t, raw["conflicts"])

		var statusRaw bson.M
		err = conflictFeatureDB.RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&statusRaw)
		require.NoError(t, err)
		assert.Equal(t, "rebase", statusRaw["mergeState"], "mergeState must indicate rebase in progress")
		assert.Equal(t, true, statusRaw["dirty"], "workspace must be dirty during rebase conflict")
		assert.Nil(t, statusRaw["commitId"], "commitId must be absent during rebase conflict")
		statusConflicts, ok := statusRaw["conflicts"].(bson.A)
		require.True(t, ok, "conflicts must be present during rebase")
		require.Len(t, statusConflicts, 1)
		sc := statusConflicts[0].(bson.M)
		assert.Equal(t, "items", sc["collection"])
		assert.EqualValues(t, 1, sc["count"])

		var conflictsResult bson.M
		err = conflictFeatureDB.RunCommand(ctx, bson.D{
			{Key: "doltConflicts", Value: int32(1)},
		}).Decode(&conflictsResult)
		require.NoError(t, err)

		conflictList, ok2 := conflictsResult["conflicts"].(bson.A)
		require.True(t, ok2 && len(conflictList) > 0, "must have at least one conflict detail")
		firstConflict := conflictList[0].(bson.M)
		conflictID, _ := firstConflict["conflictId"].(string)
		require.NotEmpty(t, conflictID, "conflictId must not be empty")
		assert.Equal(t, fmt.Sprintf("commit '%s' (ours) and branch 'main' (theirs) both modified document 1", featModHash),
			firstConflict["reason"].(bson.M)["message"])
		// After the rebase swap, "ours" is the replayed feature commit (v:200),
		// "theirs" is the onto/main value (v:100).
		assert.EqualValues(t, 200, firstConflict["ours"].(bson.M)["doc"].(bson.M)["v"], "ours = replayed feature commit")
		assert.EqualValues(t, 100, firstConflict["theirs"].(bson.M)["doc"].(bson.M)["v"], "theirs = onto/main")

		var resolveResult bson.M
		err = conflictFeatureDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "items"},
			{Key: "conflictId", Value: conflictID},
			{Key: "resolution", Value: "ours"},
		}).Decode(&resolveResult)
		require.NoError(t, err)
		assert.EqualValues(t, 1, resolveResult["ok"], "resolveConflict must succeed")

		continueRaw := runCommandRaw(t, conflictFeatureDB, bson.D{
			{Key: "dumboRebase", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		})
		assert.EqualValues(t, 1, continueRaw["ok"], "continue must succeed after conflict resolution")
		// Two commits replay: feature-adds-2 (clean) before the conflict, then
		// feature-modifies-1 after the resolution.
		assert.EqualValues(t, 2, continueRaw["commitsReplayed"], "two commits must have been replayed across the rebase")
		newTip, hasTip := continueRaw["newTip"]
		assert.True(t, hasTip, "continue response must include newTip")
		_ = newTip

		var cleanStatus bson.M
		err = conflictFeatureDB.RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&cleanStatus)
		require.NoError(t, err)
		assert.Nil(t, cleanStatus["mergeState"], "mergeState must be absent after resolution")
		assert.Nil(t, cleanStatus["conflicts"], "conflicts must be absent after resolution")
		assert.Equal(t, false, cleanStatus["dirty"], "workspace must not be dirty after resolution")
		assert.NotNil(t, cleanStatus["commitId"], "commitId must be present after resolution")

		var doc1 bson.M
		err = conflictFeatureDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc1)
		require.NoError(t, err)
		assert.EqualValues(t, 200, doc1["v"], "resolve-ours keeps the replayed commit's value")

		var logResult bson.M
		err = conflictFeatureDB.RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
			{Key: "limit", Value: int32(2)},
		}).Decode(&logResult)
		require.NoError(t, err)
		logCommits, ok := logResult["commits"].(bson.A)
		require.True(t, ok && len(logCommits) > 0)
		for _, lc := range logCommits {
			lcm := lc.(bson.M)
			assert.Equal(t, lcm["author"], lcm["committer"],
				"without committer param, rebased commit committer must equal author")
			assert.NotNil(t, lcm["committerTimestamp"], "rebased commit must have committerTimestamp")
		}
	})
}
