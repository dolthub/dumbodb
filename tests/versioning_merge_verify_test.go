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
)

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

		// Response: { hash: hashC2, message: "already up-to-date", ok: 1 }
		assert.Equal(t, "already up-to-date", raw["message"],
			"merging equal branches must report 'already up-to-date'")
		assert.Equal(t, hashC2, raw["commitId"],
			"already-up-to-date hash must equal the shared HEAD (unchanged)")
		assert.EqualValues(t, 1, raw["ok"])
	})
}
