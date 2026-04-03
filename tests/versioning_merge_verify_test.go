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

// TestMergeVerify is the automated analog of docs/verify/merge.md.
//
// Each top-level subtest corresponds to one scenario in that document.
// The setup reproduces the manual setup block exactly:
//
//   - Commit C1 on main: items = [ {_id:1, v:1} ]
//   - Branch "feature" pointing at C1 (same as main HEAD)
//
// Note: dongoCommit always commits to the main branch. The merge test therefore
// demonstrates merging main (which advances) into feature (which stays at C1),
// not the other way around.
//
// Subtests run sequentially (no t.Parallel inside) so they share a single
// database and the side effects of one scenario carry into the next.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// runCommandRaw runs a command and returns the raw BSON response as bson.M,
// even when the response contains ok:0. The MongoDB driver normally converts ok:0
// into a CommandError and does not expose the raw document; this helper works
// around that by calling DecodeBytes on the SingleResult and unmarshaling manually.
func runCommandRaw(t *testing.T, db *mongo.Database, cmd interface{}) bson.M {
	t.Helper()

	result := db.RunCommand(context.Background(), cmd)
	rawBytes, err := result.DecodeBytes()
	if err != nil {
		// The driver returns an error for ok:0; try to get the raw bytes anyway.
		if rawBytes == nil {
			t.Fatalf("runCommandRaw: no raw bytes and error: %v", err)
		}
	}

	var m bson.M
	require.NoError(t, bson.Unmarshal(rawBytes, &m))
	return m
}

// mergeVerifySetup mirrors the Setup section of docs/verify/merge.md.
// Returns hashC1 (the initial commit hash on main).
func mergeVerifySetup(t *testing.T, env *dongoTestEnv, dbName string) (hashC1 string) {
	t.Helper()

	ctx := context.Background()
	db := env.client.Database(dbName)

	require.NoError(t, db.Drop(ctx))

	// Baseline: one document, committed on main.
	_, err := db.Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "v", Value: int32(1)},
	})
	require.NoError(t, err)
	hashC1 = dongoCommit(t, env, dbName, "initial")

	// Create "feature" branch from main HEAD.
	var branchResult bson.M
	err = env.client.Database(dbName+"__main").RunCommand(ctx, bson.D{
		{Key: "dongoBranch", Value: int32(1)},
		{Key: "branch", Value: "feature"},
	}).Decode(&branchResult)
	require.NoError(t, err, "dongoBranch to create 'feature'")
	assert.Equal(t, "feature", branchResult["branch"])

	return hashC1
}

func TestMergeVerify(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()

	// Randomised db name so parallel test runs don't collide.
	dbName := fmt.Sprintf("mergvrfy%d", rand.Int64N(1_000_000))

	mergeVerifySetup(t, env, dbName)

	// hashC2 is set by Scenario 1 and used by Scenarios 2 and 3.
	var hashC2 string

	// -------------------------------------------------------------------------
	// Scenario 1: Already up-to-date — from branch is behind into branch
	// -------------------------------------------------------------------------
	t.Run("Scenario1_AlreadyUpToDate_FromBehind", func(t *testing.T) {
		// Advance main to C2 (feature stays at C1).
		_, err := env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(2)},
			{Key: "v", Value: int32(2)},
		})
		require.NoError(t, err)
		hashC2 = dongoCommit(t, env, dbName, "add-two")

		// Merge feature (at C1, behind) into main (at C2).
		var raw bson.M
		require.NoError(t, env.client.Database(dbName+"__main").RunCommand(ctx, bson.D{
			{Key: "dongoMerge", Value: int32(1)},
			{Key: "merge_in", Value: "feature"},
		}).Decode(&raw))

		// Response: { hash: hashC2, message: "already up-to-date", ok: 1 }
		assert.Equal(t, "already up-to-date", raw["message"],
			"merging behind branch into main must report 'already up-to-date'")
		assert.Equal(t, hashC2, raw["commitId"],
			"already-up-to-date hash must equal main's current HEAD (unchanged)")
		assert.EqualValues(t, 1, raw["ok"])
	})

	// -------------------------------------------------------------------------
	// Scenario 2: Fast-forward merge — bring feature up to date with main
	// -------------------------------------------------------------------------
	t.Run("Scenario2_FastForward_FeatureCatchesUp", func(t *testing.T) {
		// feature is still at C1; main is at C2.
		// Merge main into feature — feature fast-forwards to C2.
		var raw bson.M
		require.NoError(t, env.client.Database(dbName+"__feature").RunCommand(ctx, bson.D{
			{Key: "dongoMerge", Value: int32(1)},
			{Key: "merge_in", Value: "main"},
		}).Decode(&raw))

		// Response: { hash: hashC2, message: "fast-forward", ok: 1 }
		assert.Equal(t, "fast-forward", raw["message"],
			"merging ahead-of-feature main into feature must report 'fast-forward'")
		assert.Equal(t, hashC2, raw["commitId"],
			"fast-forward hash must equal main's HEAD (no new commit created)")
		assert.EqualValues(t, 1, raw["ok"])

		// feature now has both documents.
		n, err := env.client.Database(dbName + "__feature").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), n, "feature must have 2 documents after fast-forward merge")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: Already up-to-date — branches are now equal after fast-forward
	// -------------------------------------------------------------------------
	t.Run("Scenario3_AlreadyUpToDate_EqualBranches", func(t *testing.T) {
		// After Scenario 2: feature and main both point to C2.
		var raw bson.M
		require.NoError(t, env.client.Database(dbName+"__feature").RunCommand(ctx, bson.D{
			{Key: "dongoMerge", Value: int32(1)},
			{Key: "merge_in", Value: "main"},
		}).Decode(&raw))

		// Response: { commitId: hashC2, message: "already up-to-date", ok: 1 }
		assert.Equal(t, "already up-to-date", raw["message"],
			"merging equal branches must report 'already up-to-date'")
		assert.Equal(t, hashC2, raw["commitId"],
			"already-up-to-date commitId must equal the shared HEAD (unchanged)")
		assert.EqualValues(t, 1, raw["ok"])
	})

	// -------------------------------------------------------------------------
	// Scenario 4: True three-way merge — diverged branches produce a merge commit
	// -------------------------------------------------------------------------
	t.Run("Scenario4_ThreeWayMerge", func(t *testing.T) {
		// After Scenario 3: both main and feature point to C2.

		// Commit _id:3 on main → C3.
		_, err := env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)},
			{Key: "v", Value: int32(3)},
		})
		require.NoError(t, err)
		hashC3 := dongoCommit(t, env, dbName, "add-three")

		// Commit _id:4 on feature independently → C4.
		_, err = env.client.Database(dbName + "__feature").Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(4)},
			{Key: "v", Value: int32(4)},
		})
		require.NoError(t, err)
		hashC4 := dongoCommit(t, env, dbName+"__feature", "add-four")

		// Merge feature (at C4) into main (at C3) — true three-way merge.
		var raw bson.M
		require.NoError(t, env.client.Database(dbName+"__main").RunCommand(ctx, bson.D{
			{Key: "dongoMerge", Value: int32(1)},
			{Key: "merge_in", Value: "feature"},
		}).Decode(&raw))

		mergeCommitID, ok := raw["commitId"].(string)
		require.True(t, ok, "commitId must be a string")
		assert.NotEmpty(t, mergeCommitID, "three-way merge must produce a new commit")
		assert.NotEqual(t, hashC3, mergeCommitID, "merge commitId must differ from main pre-merge HEAD (C3)")
		assert.NotEqual(t, hashC4, mergeCommitID, "merge commitId must differ from feature HEAD (C4)")
		assert.Equal(t, "Merge branch 'feature' into 'main'", raw["message"])
		assert.EqualValues(t, 1, raw["ok"])

		// dongoLog must show the merge commit at HEAD with parent1=C3, parent2=C4.
		var logRaw bson.M
		require.NoError(t, env.client.Database(dbName+"__main").RunCommand(ctx, bson.D{
			{Key: "dongoLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logRaw))
		lr := decodeLogResult(t, logRaw)
		require.Len(t, lr.Commits, 1, "dongoLog with limit=1 must return exactly 1 commit")
		head := lr.Commits[0]
		assert.Equal(t, mergeCommitID, head.CommitID, "HEAD must be the merge commit")
		assert.Equal(t, hashC3, head.Parent1, "merge commit parent1 must be main's pre-merge HEAD (C3)")
		assert.Equal(t, hashC4, head.Parent2, "merge commit parent2 must be feature's HEAD (C4)")

		// All four documents must be visible on main after the merge.
		n, err := env.client.Database(dbName + "__main").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(4), n, "main must have all 4 documents after three-way merge")
	})
}

// TestMergeConflictWorkflow tests the conflict resolution workflow:
// two branches independently modify the same document, creating a conflict.
//
// Scenarios:
//   5. Conflicting merge returns ok:0 with conflicts array
//   6. dongoConflicts lists the conflict
//   7. dongoResolveConflict with "ours" preserves our version
//   8. dongoCommit after resolution creates a merge commit
//
// Additionally tests:
//   - dongoCommit is blocked while conflicts remain
//   - dongoMerge abort discards the in-progress merge
//   - dongoResolveConflict with "theirs" and "custom"
//   - dongoMerge rejects a new initiation while one is in progress
func TestMergeConflictWorkflow(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("conftest%d", rand.Int64N(1_000_000))

	// Setup: C1 on main + feature branch.
	mergeVerifySetup(t, env, dbName)

	mainDB := env.client.Database(dbName + "__main")
	featDB := env.client.Database(dbName + "__feature")

	// Advance both branches: main modifies _id:1 to v:10, feature modifies _id:1 to v:20.
	_, err := mainDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(10)}}}},
	)
	require.NoError(t, err)
	hashMain := dongoCommit(t, env, dbName+"__main", "main-modifies-one")

	_, err = featDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(20)}}}},
	)
	require.NoError(t, err)
	hashFeat := dongoCommit(t, env, dbName+"__feature", "feature-modifies-one")

	// Scenario 5: Conflicting merge returns ok:0 with conflicts array
	t.Run("Scenario5_ConflictingMerge_ReturnsConflicts", func(t *testing.T) {
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "dongoMerge", Value: int32(1)},
			{Key: "merge_in", Value: "feature"},
		})

		okVal, _ := raw["ok"]
		assert.EqualValues(t, 0, okVal, "conflicting merge must return ok:0")

		conflictsRaw, ok := raw["conflicts"]
		require.True(t, ok, "conflicting merge response must include 'conflicts' field")

		conflictsArr, ok := conflictsRaw.(bson.A)
		require.True(t, ok, "conflicts must be an array")
		require.Len(t, conflictsArr, 1, "one collection must have conflicts")

		entry := conflictsArr[0].(bson.M)
		assert.Equal(t, "items", entry["collection"], "conflicting collection must be 'items'")
		assert.EqualValues(t, 1, entry["count"], "one conflict in 'items'")
	})

	// Scenario 6: dongoConflicts lists the conflict
	t.Run("Scenario6_DongoConflicts_ListsConflict", func(t *testing.T) {
		// Summary (no collection filter)
		var summaryRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "dongoConflicts", Value: int32(1)},
		}).Decode(&summaryRaw))
		assert.EqualValues(t, 1, summaryRaw["ok"])

		colls := summaryRaw["collections"].(bson.A)
		require.Len(t, colls, 1, "one collection with conflicts")
		collEntry := colls[0].(bson.M)
		assert.Equal(t, "items", collEntry["name"])
		assert.EqualValues(t, 1, collEntry["count"])

		// Per-collection detail
		var detailRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "dongoConflicts", Value: int32(1)},
			{Key: "collection", Value: "items"},
		}).Decode(&detailRaw))
		assert.EqualValues(t, 1, detailRaw["ok"])

		conflicts := detailRaw["conflicts"].(bson.A)
		require.Len(t, conflicts, 1, "one conflict in 'items'")

		cf := conflicts[0].(bson.M)
		conflictID, ok := cf["conflictId"].(string)
		require.True(t, ok, "conflictId must be a string")
		require.NotEmpty(t, conflictID, "conflictId must not be empty")

		assert.Equal(t, "modified", cf["ourDiffType"])
		assert.Equal(t, "modified", cf["theirDiffType"])

		// base, ours, theirs must be present
		oursDoc := cf["ours"].(bson.M)
		assert.EqualValues(t, 10, oursDoc["v"], "ours doc must have v:10 (main's version)")

		theirsDoc := cf["theirs"].(bson.M)
		assert.EqualValues(t, 20, theirsDoc["v"], "theirs doc must have v:20 (feature's version)")
	})

	// Scenario 7: dongoCommit blocked while conflicts remain
	t.Run("Scenario7_CommitBlockedWithConflicts", func(t *testing.T) {
		err := mainDB.RunCommand(ctx, bson.D{
			{Key: "dongoCommit", Value: int32(1)},
			{Key: "message", Value: "should fail"},
			{Key: "author", Value: "tester"},
		}).Err()
		require.Error(t, err, "dongoCommit must fail while conflicts remain")
		assert.Contains(t, err.Error(), "unresolved merge conflicts", "error must mention unresolved conflicts")
	})

	// Scenario 8: dongoMerge rejects new merge while one is in progress
	t.Run("Scenario8_MergeInProgress_Rejected", func(t *testing.T) {
		err := mainDB.RunCommand(ctx, bson.D{
			{Key: "dongoMerge", Value: int32(1)},
			{Key: "merge_in", Value: "feature"},
		}).Err()
		require.Error(t, err, "new dongoMerge while merge in progress must fail")
	})

	// Scenario 9: Resolve with "ours" and then commit creates a merge commit
	t.Run("Scenario9_ResolveOurs_ThenCommit", func(t *testing.T) {
		// Get the conflict ID
		var detailRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "dongoConflicts", Value: int32(1)},
			{Key: "collection", Value: "items"},
		}).Decode(&detailRaw))
		conflicts := detailRaw["conflicts"].(bson.A)
		cf := conflicts[0].(bson.M)
		conflictID := cf["conflictId"].(string)

		// Resolve with "ours"
		var resolveRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "dongoResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "items"},
			{Key: "conflictId", Value: conflictID},
			{Key: "resolution", Value: "ours"},
		}).Decode(&resolveRaw))
		assert.EqualValues(t, 1, resolveRaw["ok"])

		// No more conflicts
		var summaryRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "dongoConflicts", Value: int32(1)},
		}).Decode(&summaryRaw))
		colls := summaryRaw["collections"].(bson.A)
		assert.Len(t, colls, 0, "no more conflicts after resolution")

		// Commit creates a merge commit
		var commitRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "dongoCommit", Value: int32(1)},
			{Key: "message", Value: "resolve conflict: ours"},
			{Key: "author", Value: "tester"},
		}).Decode(&commitRaw))
		assert.EqualValues(t, 1, commitRaw["ok"])

		mergeCommitID, ok := commitRaw["commitId"].(string)
		require.True(t, ok)
		require.NotEmpty(t, mergeCommitID)
		assert.NotEqual(t, hashMain, mergeCommitID)
		assert.NotEqual(t, hashFeat, mergeCommitID)

		// dongoLog HEAD must show a merge commit with two parents
		var logRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "dongoLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logRaw))
		lr := decodeLogResult(t, logRaw)
		require.Len(t, lr.Commits, 1)
		head := lr.Commits[0]
		assert.Equal(t, mergeCommitID, head.CommitID)
		assert.Equal(t, hashMain, head.Parent1, "parent1 must be main's pre-merge HEAD")
		assert.Equal(t, hashFeat, head.Parent2, "parent2 must be feature's HEAD")

		// Document _id:1 must have v:10 (ours resolution)
		var doc bson.M
		require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
		assert.EqualValues(t, 10, doc["v"], "ours resolution: v must be 10")
	})
}

// TestMergeConflictAbort tests that dongoMerge abort restores the pre-merge state.
func TestMergeConflictAbort(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("confabort%d", rand.Int64N(1_000_000))
	mergeVerifySetup(t, env, dbName)

	mainDB := env.client.Database(dbName + "__main")

	// Create a conflict: both branches modify _id:1.
	_, err := mainDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(100)}}}},
	)
	require.NoError(t, err)
	dongoCommit(t, env, dbName+"__main", "main-100")

	_, err = env.client.Database(dbName + "__feature").Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(200)}}}},
	)
	require.NoError(t, err)
	dongoCommit(t, env, dbName+"__feature", "feature-200")

	// Trigger a conflicting merge.
	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "dongoMerge", Value: int32(1)},
		{Key: "merge_in", Value: "feature"},
	})
	okVal, _ := raw["ok"]
	require.EqualValues(t, 0, okVal, "conflicting merge must return ok:0")

	// Abort the merge.
	var abortRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "dongoMerge", Value: int32(1)},
		{Key: "abort", Value: true},
	}).Decode(&abortRaw))
	assert.EqualValues(t, 1, abortRaw["ok"])
	assert.Equal(t, "merge aborted", abortRaw["message"])

	// After abort, dongoCommit must succeed (no more conflicts).
	var commitRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "dongoCommit", Value: int32(1)},
		{Key: "message", Value: "after abort"},
		{Key: "author", Value: "tester"},
	}).Decode(&commitRaw))
	assert.EqualValues(t, 1, commitRaw["ok"])

	// Document must have the pre-merge value (v:100).
	var doc bson.M
	require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
	assert.EqualValues(t, 100, doc["v"], "after abort, document must be back to pre-merge value")
}

// TestMergeConflictResolveTheirs tests the "theirs" resolution strategy.
func TestMergeConflictResolveTheirs(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("conftheirs%d", rand.Int64N(1_000_000))
	mergeVerifySetup(t, env, dbName)

	mainDB := env.client.Database(dbName + "__main")
	featDB := env.client.Database(dbName + "__feature")

	// Both branches modify _id:1.
	_, err := mainDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(11)}}}},
	)
	require.NoError(t, err)
	dongoCommit(t, env, dbName+"__main", "main-11")

	_, err = featDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(22)}}}},
	)
	require.NoError(t, err)
	dongoCommit(t, env, dbName+"__feature", "feature-22")

	// Trigger conflict.
	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "dongoMerge", Value: int32(1)},
		{Key: "merge_in", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"])

	// Get conflict ID.
	var detailRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "dongoConflicts", Value: int32(1)},
		{Key: "collection", Value: "items"},
	}).Decode(&detailRaw))
	conflicts := detailRaw["conflicts"].(bson.A)
	cf := conflicts[0].(bson.M)
	conflictID := cf["conflictId"].(string)

	// Resolve with "theirs".
	var resolveRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "dongoResolveConflict", Value: int32(1)},
		{Key: "collection", Value: "items"},
		{Key: "conflictId", Value: conflictID},
		{Key: "resolution", Value: "theirs"},
	}).Decode(&resolveRaw))
	assert.EqualValues(t, 1, resolveRaw["ok"])

	// Commit.
	var commitRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "dongoCommit", Value: int32(1)},
		{Key: "message", Value: "resolve: theirs"},
		{Key: "author", Value: "tester"},
	}).Decode(&commitRaw))
	assert.EqualValues(t, 1, commitRaw["ok"])

	// Document must have theirs value (v:22).
	var doc bson.M
	require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
	assert.EqualValues(t, 22, doc["v"], "theirs resolution: v must be 22")
}

// TestMergeConflictResolveCustom tests the "custom" resolution strategy.
func TestMergeConflictResolveCustom(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("confcustom%d", rand.Int64N(1_000_000))
	mergeVerifySetup(t, env, dbName)

	mainDB := env.client.Database(dbName + "__main")
	featDB := env.client.Database(dbName + "__feature")

	// Both branches modify _id:1.
	_, err := mainDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(55)}}}},
	)
	require.NoError(t, err)
	dongoCommit(t, env, dbName+"__main", "main-55")

	_, err = featDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(66)}}}},
	)
	require.NoError(t, err)
	dongoCommit(t, env, dbName+"__feature", "feature-66")

	// Trigger conflict.
	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "dongoMerge", Value: int32(1)},
		{Key: "merge_in", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"])

	// Get conflict ID.
	var detailRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "dongoConflicts", Value: int32(1)},
		{Key: "collection", Value: "items"},
	}).Decode(&detailRaw))
	conflicts := detailRaw["conflicts"].(bson.A)
	cf := conflicts[0].(bson.M)
	conflictID := cf["conflictId"].(string)

	// Resolve with "custom" value v:99.
	var resolveRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "dongoResolveConflict", Value: int32(1)},
		{Key: "collection", Value: "items"},
		{Key: "conflictId", Value: conflictID},
		{Key: "resolution", Value: "custom"},
		{Key: "value", Value: bson.D{
			{Key: "_id", Value: int32(1)},
			{Key: "v", Value: int32(99)},
		}},
	}).Decode(&resolveRaw))
	assert.EqualValues(t, 1, resolveRaw["ok"])

	// Commit.
	var commitRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "dongoCommit", Value: int32(1)},
		{Key: "message", Value: "resolve: custom"},
		{Key: "author", Value: "tester"},
	}).Decode(&commitRaw))
	assert.EqualValues(t, 1, commitRaw["ok"])

	// Document must have custom value v:99.
	var doc bson.M
	require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
	assert.EqualValues(t, 99, doc["v"], "custom resolution: v must be 99")
}
