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

// dumboDBCommit always commits to the main branch, so the merge tests merge main
// (which advances) into feature, not the other way around.
//
// Subtests run sequentially so they share a single database and side effects of
// one scenario carry into the next.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// mergeVerifySetup returns hashC1, the initial commit hash on main.
func mergeVerifySetup(t *testing.T, env *dumboDBTestEnv, dbName string) (hashC1 string) {
	t.Helper()

	ctx := context.Background()
	db := env.Client.Database(dbName)

	require.NoError(t, db.Drop(ctx))

	_, err := db.Collection("inventory").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "v", Value: int32(1)},
	})
	require.NoError(t, err)
	hashC1 = dumboDBCommit(t, env, dbName, "initial", "alice <alice@acme.com>")

	var branchResult bson.M
	err = env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "feature"},
	}).Decode(&branchResult)
	require.NoError(t, err, "doltBranch to create 'feature'")
	assert.Equal(t, "feature", branchResult["branch"])

	return hashC1
}

func TestMergeVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	// Randomised db name so parallel test runs don't collide.
	dbName := fmt.Sprintf("mergvrfy%d", rand.Int64N(1_000_000))

	mergeVerifySetup(t, env, dbName)

	// hashC2 is set by Scenario 1 and used by Scenarios 2 and 3.
	var hashC2 string

	t.Run("Scenario1_AlreadyUpToDate_FromBehind", func(t *testing.T) {
		_, err := env.Client.Database(dbName).Collection("inventory").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(2)},
			{Key: "v", Value: int32(2)},
		})
		require.NoError(t, err)
		hashC2 = dumboDBCommit(t, env, dbName, "add-two", "alice <alice@acme.com>")

		// Merge feature (at C1, behind) into main (at C2).
		var raw bson.M
		require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "mergeIn", Value: "feature"},
		}).Decode(&raw))

		assert.Equal(t, "already up-to-date", raw["message"],
			"merging behind branch into main must report 'already up-to-date'")
		assert.Equal(t, hashC2, raw["commitId"],
			"already-up-to-date hash must equal main's current HEAD (unchanged)")
		assert.EqualValues(t, 1, raw["ok"])
	})

	t.Run("Scenario2_FastForward_FeatureCatchesUp", func(t *testing.T) {
		// feature is still at C1; main is at C2, so this fast-forwards.
		var raw bson.M
		require.NoError(t, env.Client.Database(dbName+"@feature").RunCommand(ctx, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "mergeIn", Value: "main"},
		}).Decode(&raw))

		assert.Equal(t, "fast-forward", raw["message"],
			"merging ahead-of-feature main into feature must report 'fast-forward'")
		assert.Equal(t, hashC2, raw["commitId"],
			"fast-forward hash must equal main's HEAD (no new commit created)")
		assert.EqualValues(t, 1, raw["ok"])

		n, err := env.Client.Database(dbName+"@feature").Collection("inventory").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), n, "feature must have 2 documents after fast-forward merge")
	})

	t.Run("Scenario3_AlreadyUpToDate_EqualBranches", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.Client.Database(dbName+"@feature").RunCommand(ctx, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "mergeIn", Value: "main"},
		}).Decode(&raw))

		assert.Equal(t, "already up-to-date", raw["message"],
			"merging equal branches must report 'already up-to-date'")
		assert.Equal(t, hashC2, raw["commitId"],
			"already-up-to-date commitId must equal the shared HEAD (unchanged)")
		assert.EqualValues(t, 1, raw["ok"])
	})

	t.Run("Scenario4_ThreeWayMerge", func(t *testing.T) {
		_, err := env.Client.Database(dbName).Collection("inventory").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)},
			{Key: "v", Value: int32(3)},
		})
		require.NoError(t, err)
		hashC3 := dumboDBCommit(t, env, dbName, "add-three", "alice <alice@acme.com>")

		_, err = env.Client.Database(dbName+"@feature").Collection("inventory").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(4)},
			{Key: "v", Value: int32(4)},
		})
		require.NoError(t, err)
		hashC4 := dumboDBCommit(t, env, dbName+"@feature", "add-four", "alice <alice@acme.com>")

		var raw bson.M
		require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "mergeIn", Value: "feature"},
			{Key: "message", Value: "custom merge msg"},
			{Key: "author", Value: "bob <bob@x>"},
		}).Decode(&raw))

		mergeCommitID, ok := raw["commitId"].(string)
		require.True(t, ok, "commitId must be a string")
		assert.NotEmpty(t, mergeCommitID, "three-way merge must produce a new commit")
		assert.NotEqual(t, hashC3, mergeCommitID, "merge commitId must differ from main pre-merge HEAD (C3)")
		assert.NotEqual(t, hashC4, mergeCommitID, "merge commitId must differ from feature HEAD (C4)")
		assert.Equal(t, "custom merge msg", raw["message"])
		assert.Equal(t, raw["author"], raw["committer"], "committer must equal author for merge commit")
		assert.NotNil(t, raw["committerTimestamp"], "committerTimestamp must be present in merge response")
		assert.EqualValues(t, 1, raw["ok"])

		var logRaw bson.M
		require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logRaw))
		lr := decodeLogResult(t, logRaw)
		require.Len(t, lr.Commits, 1, "doltLog with limit=1 must return exactly 1 commit")
		head := lr.Commits[0]
		assert.Equal(t, mergeCommitID, head.CommitID, "HEAD must be the merge commit")
		assert.Equal(t, hashC3, head.Parent1, "merge commit parent1 must be main's pre-merge HEAD (C3)")
		assert.Equal(t, hashC4, head.Parent2, "merge commit parent2 must be feature's HEAD (C4)")
		assert.Equal(t, "custom merge msg", head.Message, "merge commit message must be the custom message")
		assert.Equal(t, "bob <bob@x>", head.Author, "merge commit author must be bob <bob@x>")
		assert.Equal(t, head.Author, head.Committer, "merge commit committer must equal author")
		assert.NotNil(t, head.CommitterTimestamp, "merge commit committerTimestamp must be present in log")

		n, err := env.Client.Database(dbName+"@main").Collection("inventory").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(4), n, "main must have all 4 documents after three-way merge")
	})
}

func TestMergeCustomMessageAuthor(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	t.Run("Scenario4_ThreeWayMerge_CustomMessageAuthor", func(t *testing.T) {
		dbName := fmt.Sprintf("mergcustom%d", rand.Int64N(1_000_000))
		mergeVerifySetup(t, env, dbName)

		mainDB := env.Client.Database(dbName + "@main")
		featDB := env.Client.Database(dbName + "@feature")

		_, err := mainDB.Collection("inventory").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(2)},
			{Key: "v", Value: int32(2)},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@main", "add-two", "alice <alice@acme.com>")

		_, err = featDB.Collection("inventory").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)},
			{Key: "v", Value: int32(3)},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "add-three", "alice <alice@acme.com>")

		var raw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "mergeIn", Value: "feature"},
			{Key: "message", Value: "custom msg"},
			{Key: "author", Value: "bob <bob@x>"},
		}).Decode(&raw))
		assert.EqualValues(t, 1, raw["ok"])
		assert.Equal(t, "custom msg", raw["message"], "merge response message must match custom message")

		mergeCommitID, ok := raw["commitId"].(string)
		require.True(t, ok, "commitId must be a string")
		require.NotEmpty(t, mergeCommitID)

		var logRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logRaw))
		lr := decodeLogResult(t, logRaw)
		require.Len(t, lr.Commits, 1)
		head := lr.Commits[0]
		assert.Equal(t, mergeCommitID, head.CommitID)
		assert.Equal(t, "custom msg", head.Message, "doltLog must show the custom message")
		assert.Equal(t, "bob <bob@x>", head.Author, "doltLog must show the custom author")
		assert.Equal(t, head.Author, head.Committer, "merge commit committer must equal author in log")
		assert.NotNil(t, head.CommitterTimestamp, "merge commit committerTimestamp must be present in log")
	})

	t.Run("Scenario11_Continue_CustomMessageAuthor", func(t *testing.T) {
		dbName := fmt.Sprintf("mergcont%d", rand.Int64N(1_000_000))
		mergeVerifySetup(t, env, dbName)

		mainDB := env.Client.Database(dbName + "@main")
		featDB := env.Client.Database(dbName + "@feature")

		_, err := mainDB.Collection("inventory").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(10)}}}},
		)
		require.NoError(t, err)
		hashMain := dumboDBCommit(t, env, dbName+"@main", "main-v10", "alice")

		_, err = featDB.Collection("inventory").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(20)}}}},
		)
		require.NoError(t, err)
		hashFeat := dumboDBCommit(t, env, dbName+"@feature", "feature-v20", "bob")

		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "mergeIn", Value: "feature"},
		})
		require.EqualValues(t, 0, raw["ok"], "conflicting merge must return ok:0")

		var detailRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltConflicts", Value: int32(1)},
		}).Decode(&detailRaw))
		conflicts := detailRaw["conflicts"].(bson.A)
		cf := conflicts[0].(bson.M)
		conflictID := cf["conflictId"].(string)

		var resolveRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "inventory"},
			{Key: "conflictId", Value: conflictID},
			{Key: "resolution", Value: "ours"},
		}).Decode(&resolveRaw))
		assert.EqualValues(t, 1, resolveRaw["ok"])

		var continueRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
			{Key: "message", Value: "Resolve merge conflicts"},
			{Key: "author", Value: "alice <alice@acme.com>"},
		}).Decode(&continueRaw))
		assert.EqualValues(t, 1, continueRaw["ok"])
		assert.Equal(t, "Resolve merge conflicts", continueRaw["message"], "continue response message must match custom message")
		assert.Equal(t, continueRaw["author"], continueRaw["committer"], "committer must equal author for merge continue")
		assert.NotNil(t, continueRaw["committerTimestamp"], "committerTimestamp must be present in merge continue response")

		mergeCommitID, ok := continueRaw["commitId"].(string)
		require.True(t, ok, "commitId must be a string")
		require.NotEmpty(t, mergeCommitID)
		assert.NotEqual(t, hashMain, mergeCommitID)
		assert.NotEqual(t, hashFeat, mergeCommitID)

		var logRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logRaw))
		lr := decodeLogResult(t, logRaw)
		require.Len(t, lr.Commits, 1)
		head := lr.Commits[0]
		assert.Equal(t, mergeCommitID, head.CommitID)
		assert.Equal(t, hashMain, head.Parent1, "parent1 must be main's pre-merge HEAD")
		assert.Equal(t, hashFeat, head.Parent2, "parent2 must be feature's HEAD")
		assert.Equal(t, "Resolve merge conflicts", head.Message, "doltLog must show the custom message")
		assert.Equal(t, "alice <alice@acme.com>", head.Author, "doltLog must show the custom author")
		assert.Equal(t, head.Author, head.Committer, "merge commit committer must equal author in log")
		assert.NotNil(t, head.CommitterTimestamp, "merge commit committerTimestamp must be present in log")
	})
}

func TestMergeConflictWorkflow(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("conftest%d", rand.Int64N(1_000_000))

	mergeVerifySetup(t, env, dbName)

	mainDB := env.Client.Database(dbName + "@main")
	featDB := env.Client.Database(dbName + "@feature")

	_, err := mainDB.Collection("inventory").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(10)}}}},
	)
	require.NoError(t, err)
	hashMain := dumboDBCommit(t, env, dbName+"@main", "main-v10", "alice")

	_, err = featDB.Collection("inventory").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(20)}}}},
	)
	require.NoError(t, err)
	hashFeat := dumboDBCommit(t, env, dbName+"@feature", "feature-v20", "bob")

	t.Run("Scenario5_ConflictingMerge_ReturnsConflicts", func(t *testing.T) {
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "mergeIn", Value: "feature"},
		})

		okVal, _ := raw["ok"]
		assert.EqualValues(t, 0, okVal, "conflicting merge must return ok:0")

		conflictsRaw, ok := raw["conflicts"]
		require.True(t, ok, "conflicting merge response must include 'conflicts' field")

		conflictsArr, ok := conflictsRaw.(bson.A)
		require.True(t, ok, "conflicts must be an array")
		require.Len(t, conflictsArr, 1, "one collection must have conflicts")

		entry := conflictsArr[0].(bson.M)
		assert.Equal(t, "inventory", entry["collection"], "conflicting collection must be 'inventory'")
		assert.EqualValues(t, 1, entry["count"], "one conflict in 'inventory'")

		var statusRaw bson.M
		err := mainDB.RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&statusRaw)
		require.NoError(t, err)
		assert.Equal(t, "merge", statusRaw["mergeState"], "mergeState must indicate merge in progress")
		assert.Equal(t, true, statusRaw["dirty"], "workspace must be dirty during merge conflict")
		assert.Nil(t, statusRaw["commitId"], "commitId must be absent during merge conflict")
		statusConflicts, ok2 := statusRaw["conflicts"].(bson.A)
		require.True(t, ok2, "conflicts must be present during merge")
		require.Len(t, statusConflicts, 1)
		sc := statusConflicts[0].(bson.M)
		assert.Equal(t, "inventory", sc["collection"])
		assert.EqualValues(t, 1, sc["count"])
	})

	t.Run("Scenario6_DumboDBConflicts_ListsConflict", func(t *testing.T) {
		var conflictsRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltConflicts", Value: int32(1)},
		}).Decode(&conflictsRaw))
		assert.EqualValues(t, 1, conflictsRaw["ok"])

		conflicts := conflictsRaw["conflicts"].(bson.A)
		require.Len(t, conflicts, 1, "one conflict")

		cf := conflicts[0].(bson.M)
		assert.Equal(t, "inventory", cf["name"])
		conflictID, ok := cf["conflictId"].(string)
		require.True(t, ok, "conflictId must be a string")
		require.NotEmpty(t, conflictID, "conflictId must not be empty")

		assert.Equal(t, "document", cf["type"])
		reason := cf["reason"].(bson.M)
		assert.Equal(t, "bothModified", reason["code"])
		assert.Equal(t, "branch 'main' (ours) and branch 'feature' (theirs) both modified document 1", reason["message"],
			"message names both branches and the _id of the contended document")

		// Each non-null side carries its own _id (sibling of doc).
		ours := cf["ours"].(bson.M)
		assert.EqualValues(t, int32(1), ours["_id"], "ours side must carry _id:1")
		assert.Equal(t, "modified", ours["diffType"])
		oursDoc := ours["doc"].(bson.M)
		assert.EqualValues(t, 10, oursDoc["v"], "ours doc must have v:10 (main's version)")
		assert.EqualValues(t, int32(1), oursDoc["_id"], "doc carries the full document including _id")

		theirs := cf["theirs"].(bson.M)
		assert.EqualValues(t, int32(1), theirs["_id"], "theirs side must carry _id:1")
		assert.Equal(t, "modified", theirs["diffType"])
		theirsDoc := theirs["doc"].(bson.M)
		assert.EqualValues(t, 20, theirsDoc["v"], "theirs doc must have v:20 (feature's version)")
		assert.EqualValues(t, int32(1), theirsDoc["_id"], "doc carries the full document including _id")
	})

	t.Run("Scenario7_CommitBlockedWithConflicts", func(t *testing.T) {
		err := mainDB.RunCommand(ctx, bson.D{
			{Key: "doltCommit", Value: int32(1)},
			{Key: "message", Value: "should fail"},
			{Key: "author", Value: "tester"},
		}).Err()
		require.Error(t, err, "doltCommit must fail while conflicts remain")
		assert.Contains(t, err.Error(), "unresolved merge conflicts", "error must mention unresolved conflicts")
	})

	t.Run("Scenario8_MergeInProgress_Rejected", func(t *testing.T) {
		err := mainDB.RunCommand(ctx, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "mergeIn", Value: "feature"},
		}).Err()
		require.Error(t, err, "new dumboDBMerge while merge in progress must fail")
	})

	t.Run("Scenario9_ResolveOurs_ThenContinue", func(t *testing.T) {
		var detailRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltConflicts", Value: int32(1)},
		}).Decode(&detailRaw))
		conflicts := detailRaw["conflicts"].(bson.A)
		cf := conflicts[0].(bson.M)
		conflictID := cf["conflictId"].(string)

		var resolveRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "inventory"},
			{Key: "conflictId", Value: conflictID},
			{Key: "resolution", Value: "ours"},
		}).Decode(&resolveRaw))
		assert.EqualValues(t, 1, resolveRaw["ok"])

		var postResolveRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltConflicts", Value: int32(1)},
		}).Decode(&postResolveRaw))
		postConflicts := postResolveRaw["conflicts"].(bson.A)
		assert.Len(t, postConflicts, 0, "no more conflicts after resolution")

		// dumboDBCommit is blocked even when all conflicts are resolved  -- merge in progress
		err := mainDB.RunCommand(ctx, bson.D{
			{Key: "doltCommit", Value: int32(1)},
			{Key: "message", Value: "should fail: merge in progress"},
			{Key: "author", Value: "tester"},
		}).Err()
		require.Error(t, err, "doltCommit must fail while merge is in progress (even with no conflicts)")
		assert.Contains(t, err.Error(), "merge in progress", "error must mention merge in progress")

		var commitRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		}).Decode(&commitRaw))
		assert.EqualValues(t, 1, commitRaw["ok"])
		assert.NotNil(t, commitRaw["committer"], "committer must be present in merge continue response")
		assert.NotNil(t, commitRaw["committerTimestamp"], "committerTimestamp must be present in merge continue response")

		mergeCommitID, ok := commitRaw["commitId"].(string)
		require.True(t, ok)
		require.NotEmpty(t, mergeCommitID)
		assert.NotEqual(t, hashMain, mergeCommitID)
		assert.NotEqual(t, hashFeat, mergeCommitID)

		var logRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logRaw))
		lr := decodeLogResult(t, logRaw)
		require.Len(t, lr.Commits, 1)
		head := lr.Commits[0]
		assert.Equal(t, mergeCommitID, head.CommitID)
		assert.Equal(t, hashMain, head.Parent1, "parent1 must be main's pre-merge HEAD")
		assert.Equal(t, hashFeat, head.Parent2, "parent2 must be feature's HEAD")

		var doc bson.M
		require.NoError(t, mainDB.Collection("inventory").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
		assert.EqualValues(t, 10, doc["v"], "ours resolution: v must be 10")

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
}

func TestMergeConflictAbort(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("confabort%d", rand.Int64N(1_000_000))
	mergeVerifySetup(t, env, dbName)

	mainDB := env.Client.Database(dbName + "@main")

	_, err := mainDB.Collection("inventory").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(100)}}}},
	)
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main-100", "alice")

	_, err = env.Client.Database(dbName+"@feature").Collection("inventory").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(200)}}}},
	)
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature-200", "bob")

	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "mergeIn", Value: "feature"},
	})
	okVal, _ := raw["ok"]
	require.EqualValues(t, 0, okVal, "conflicting merge must return ok:0")

	var abortRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "abort", Value: int32(1)},
	}).Decode(&abortRaw))
	assert.EqualValues(t, 1, abortRaw["ok"])
	assert.Equal(t, "merge aborted", abortRaw["message"])

	// Working set is clean post-abort, so allowEmpty:true isolates the
	// merge-state guard as the only thing that could fail.
	var commitRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: int32(1)},
		{Key: "message", Value: "after abort"},
		{Key: "author", Value: "tester"},
		{Key: "allowEmpty", Value: true},
	}).Decode(&commitRaw))
	assert.EqualValues(t, 1, commitRaw["ok"])

	var doc bson.M
	require.NoError(t, mainDB.Collection("inventory").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
	assert.EqualValues(t, 100, doc["v"], "after abort, document must be back to pre-merge value")
}

func TestMergeConflictResolveTheirs(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("conftheirs%d", rand.Int64N(1_000_000))
	mergeVerifySetup(t, env, dbName)

	mainDB := env.Client.Database(dbName + "@main")
	featDB := env.Client.Database(dbName + "@feature")

	_, err := mainDB.Collection("inventory").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(11)}}}},
	)
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main-11", "alice")

	_, err = featDB.Collection("inventory").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(22)}}}},
	)
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature-22", "bob")

	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "mergeIn", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"])

	var detailRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltConflicts", Value: int32(1)},
	}).Decode(&detailRaw))
	conflicts := detailRaw["conflicts"].(bson.A)
	cf := conflicts[0].(bson.M)
	conflictID := cf["conflictId"].(string)

	var resolveRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "collection", Value: "inventory"},
		{Key: "conflictId", Value: conflictID},
		{Key: "resolution", Value: "theirs"},
	}).Decode(&resolveRaw))
	assert.EqualValues(t, 1, resolveRaw["ok"])

	// HEAD has v:11 (ours), resolved working set has v:22 (theirs).
	var diffRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltDiff", Value: int32(1)},
	}).Decode(&diffRaw))
	dr := decodeDiffResult(t, diffRaw)
	cd := findCollDiff(dr, "inventory")
	require.NotNil(t, cd, "doltDiff must show changes after resolving with theirs")
	require.Len(t, cd.Modified, 1, "one modified document (_id:1)")

	var continueRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "continue", Value: int32(1)},
	}).Decode(&continueRaw))
	assert.EqualValues(t, 1, continueRaw["ok"])

	var doc bson.M
	require.NoError(t, mainDB.Collection("inventory").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
	assert.EqualValues(t, 22, doc["v"], "theirs resolution: v must be 22")
}

func TestMergeConflictResolveCustom(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("confcustom%d", rand.Int64N(1_000_000))
	mergeVerifySetup(t, env, dbName)

	mainDB := env.Client.Database(dbName + "@main")
	featDB := env.Client.Database(dbName + "@feature")

	_, err := mainDB.Collection("inventory").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(55)}}}},
	)
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main-55", "alice")

	_, err = featDB.Collection("inventory").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(66)}}}},
	)
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature-66", "bob")

	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "mergeIn", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"])

	var detailRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltConflicts", Value: int32(1)},
	}).Decode(&detailRaw))
	conflicts := detailRaw["conflicts"].(bson.A)
	cf := conflicts[0].(bson.M)
	conflictID := cf["conflictId"].(string)

	var resolveRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "collection", Value: "inventory"},
		{Key: "conflictId", Value: conflictID},
		{Key: "resolution", Value: "custom"},
		{Key: "value", Value: bson.D{
			{Key: "_id", Value: int32(1)},
			{Key: "v", Value: int32(99)},
		}},
	}).Decode(&resolveRaw))
	assert.EqualValues(t, 1, resolveRaw["ok"])

	var continueRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "continue", Value: int32(1)},
	}).Decode(&continueRaw))
	assert.EqualValues(t, 1, continueRaw["ok"])

	var doc bson.M
	require.NoError(t, mainDB.Collection("inventory").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
	assert.EqualValues(t, 99, doc["v"], "custom resolution: v must be 99")
}

func TestMergePartialConflict(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("partial%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)
	require.NoError(t, db.Drop(ctx))

	_, err := db.Collection("items").InsertMany(ctx, []interface{}{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "original"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "original"}},
	})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "baseline", "alice <alice@acme.com>")

	mainDB := env.Client.Database(dbName + "@main")
	featDB := env.Client.Database(dbName + "@feature")

	var branchRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "feature"},
	}).Decode(&branchRaw))

	_, err = mainDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "main-v1"}}}},
	)
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main updates id1", "alice <alice@acme.com>")

	// Feature: modify both _id:1 (will conflict) and _id:2 (clean).
	_, err = featDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat-v1"}}}},
	)
	require.NoError(t, err)
	_, err = featDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(2)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat-v2"}}}},
	)
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature updates both", "bob <bob@widgets.io>")

	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "mergeIn", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"], "merge must return ok:0 due to conflict on _id:1")

	var conflictsRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltConflicts", Value: int32(1)},
	}).Decode(&conflictsRaw))

	conflicts := conflictsRaw["conflicts"].(bson.A)
	require.Len(t, conflicts, 1, "exactly one conflict (_id:1 only)")
	cf := conflicts[0].(bson.M)
	assert.Equal(t, "items", cf["name"])
	assert.EqualValues(t, int32(1), cf["ours"].(bson.M)["_id"], "conflicting document must be _id:1")
	conflictID := cf["conflictId"].(string)

	var resolveRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "collection", Value: "items"},
		{Key: "conflictId", Value: conflictID},
		{Key: "resolution", Value: "theirs"},
	}).Decode(&resolveRaw))
	assert.EqualValues(t, 1, resolveRaw["ok"])

	var continueRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "continue", Value: int32(1)},
		{Key: "message", Value: "merge with partial conflict"},
		{Key: "author", Value: "alice <alice@acme.com>"},
	}).Decode(&continueRaw))
	assert.EqualValues(t, 1, continueRaw["ok"])
	require.NotNil(t, continueRaw["commitId"], "merge continue must produce a commit")

	var doc1, doc2 bson.M
	require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc1))
	assert.Equal(t, "feat-v1", doc1["v"], "_id:1 must have theirs resolution (feat-v1)")

	require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(2)}}).Decode(&doc2))
	assert.Equal(t, "feat-v2", doc2["v"], "_id:2 must have cleanly merged value (feat-v2)")
}
