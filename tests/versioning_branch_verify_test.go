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

// TestBranchVerify is the automated analog of docs/verify/branch.md.
//
// Each top-level subtest corresponds to one scenario in that document.
// The setup reproduces the manual setup block exactly:
//
//   - Commit 1 (hash1): items = [ { _id:1, label:"alpha" } ]
//   - Commit 2 (hash2, HEAD): items = [ { _id:1, ... }, { _id:2, label:"beta" } ]
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

// branchVerifySetup mirrors the Setup section of docs/verify/branch.md.
// Returns hash1 (commit 1) and hash2 (commit 2, same as main HEAD).
func branchVerifySetup(t *testing.T, env *docudoltTestEnv, dbName string) (hash1, hash2 string) {
	t.Helper()

	ctx := context.Background()
	db := env.client.Database(dbName)
	items := db.Collection("items")

	require.NoError(t, db.Drop(ctx))

	// Commit 1: one document.
	_, err := items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "label", Value: "alpha"},
	})
	require.NoError(t, err)
	hash1 = docudoltCommit(t, env, dbName, "commit one")

	// Commit 2: second document added.
	_, err = items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "label", Value: "beta"},
	})
	require.NoError(t, err)
	hash2 = docudoltCommit(t, env, dbName, "commit two")

	return hash1, hash2
}

func TestBranchVerify(t *testing.T) {
	env := startDocudolt(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("branchvrfy%d", rand.Int64N(1_000_000))

	hash1, _ := branchVerifySetup(t, env, dbName)

	// -------------------------------------------------------------------------
	// Scenario 1: Create branch from main HEAD — response shape
	// -------------------------------------------------------------------------
	t.Run("Scenario1_CreateBranch_ResponseShape", func(t *testing.T) {
		var result bson.M
		err := env.client.Database(dbName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltBranch", Value: int32(1)},
			{Key: "branch", Value: "feature"},
		}).Decode(&result)
		require.NoError(t, err, "docudoltBranch must succeed")

		assert.Equal(t, "feature", result["branch"], "branch must echo the provided name")
		assert.EqualValues(t, 1, result["ok"], "ok must be 1")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: New branch points to the same commit as its source
	// -------------------------------------------------------------------------
	t.Run("Scenario2_NewBranchMatchesSourceCommit", func(t *testing.T) {
		// Create "snapshot" branch from current main HEAD.
		require.NoError(t, env.client.Database(dbName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltBranch", Value: int32(1)},
			{Key: "branch", Value: "snapshot"},
		}).Err(), "docudoltBranch to create 'snapshot'")

		// Diff main vs snapshot — identical commits → empty collections.
		var raw bson.M
		require.NoError(t, env.client.Database(dbName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
			{Key: "from", Value: "snapshot"},
			{Key: "to", Value: "main"},
		}).Decode(&raw))

		dr := decodeDiffResult(t, raw)
		assert.Empty(t, dr.Collections,
			"diff between new branch and source must be empty (identical commits)")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: Branch isolation — writes on branch do not affect source
	// -------------------------------------------------------------------------
	t.Run("Scenario3_BranchIsolation", func(t *testing.T) {
		featureDB := env.client.Database(dbName + "__feature")

		// Insert _id:3 on the feature branch and commit it.
		_, err := featureDB.Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)},
			{Key: "label", Value: "gamma"},
		})
		require.NoError(t, err)
		docudoltCommit(t, env, dbName+"__feature", "feature adds gamma")

		// main must still have exactly 2 documents.
		mainCount, err := env.client.Database(dbName+"__main").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), mainCount,
			"main must still have 2 documents; feature write must not leak to main")

		// feature must have 3 documents.
		featureCount, err := featureDB.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), featureCount,
			"feature branch must have 3 documents after committing _id:3")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: Create branch from a commit hash rootish
	// -------------------------------------------------------------------------
	t.Run("Scenario4_CreateBranchFromHashRootish", func(t *testing.T) {
		// Create "at-commit-one" from the commit-hash rootish at hash1.
		var result bson.M
		err := env.client.Database(dbName+"__"+hash1).RunCommand(ctx, bson.D{
			{Key: "docudoltBranch", Value: int32(1)},
			{Key: "branch", Value: "at-commit-one"},
		}).Decode(&result)
		require.NoError(t, err, "docudoltBranch from hash rootish must succeed")

		assert.Equal(t, "at-commit-one", result["branch"])
		assert.EqualValues(t, 1, result["ok"])

		// The new branch must see only the one document from commit 1.
		count, err := env.client.Database(dbName+"__at-commit-one").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), count,
			"branch created from hash1 must see only 1 document (commit 1 state)")

		var docs []bson.M
		cursor, err := env.client.Database(dbName+"__at-commit-one").Collection("items").Find(ctx, bson.D{})
		require.NoError(t, err)
		require.NoError(t, cursor.All(ctx, &docs))
		require.Len(t, docs, 1)
		assert.Equal(t, int32(1), docs[0]["_id"], "only _id:1 must be present at hash1 state")
	})

	// -------------------------------------------------------------------------
	// Scenario 5: Create branch from an ancestor expression rootish
	// Uses a fresh isolated database to avoid state drift from prior scenarios.
	// -------------------------------------------------------------------------
	t.Run("Scenario5_CreateBranchFromAncestorExpression", func(t *testing.T) {
		// Fresh database with a controlled two-commit history.
		ancDbName := fmt.Sprintf("branchvrfy_anc%d", rand.Int64N(1_000_000))
		ancHash1, _ := branchVerifySetup(t, env, ancDbName)
		_ = ancHash1

		// main~1 resolves to commit 1 (one document).
		var result bson.M
		err := env.client.Database(ancDbName+"__main~1").RunCommand(ctx, bson.D{
			{Key: "docudoltBranch", Value: int32(1)},
			{Key: "branch", Value: "back-one"},
		}).Decode(&result)
		require.NoError(t, err, "docudoltBranch from ancestor expression rootish must succeed")

		assert.Equal(t, "back-one", result["branch"])
		assert.EqualValues(t, 1, result["ok"])

		// back-one must see only one document (state at main~1 = commit 1).
		count, err := env.client.Database(ancDbName+"__back-one").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), count,
			"branch created from main~1 must see only 1 document")

		var docs []bson.M
		cursor, err := env.client.Database(ancDbName+"__back-one").Collection("items").Find(ctx, bson.D{})
		require.NoError(t, err)
		require.NoError(t, cursor.All(ctx, &docs))
		require.Len(t, docs, 1)
		assert.Equal(t, int32(1), docs[0]["_id"], "only _id:1 must be visible at main~1 state")
	})

	// -------------------------------------------------------------------------
	// Scenario 6: Safe delete (-d) — branch already merged into main
	// -------------------------------------------------------------------------
	t.Run("Scenario6_SafeDelete_MergedBranch", func(t *testing.T) {
		delDbName := fmt.Sprintf("branchvrfy_del%d", rand.Int64N(1_000_000))
		branchVerifySetup(t, env, delDbName)

		// Create "merged-branch" from main HEAD.
		require.NoError(t, env.client.Database(delDbName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltBranch", Value: int32(1)},
			{Key: "branch", Value: "merged-branch"},
		}).Err(), "creating merged-branch must succeed")

		// Safe delete: merged-branch HEAD equals main HEAD, so it is reachable from
		// main — delete must succeed.
		var result bson.M
		require.NoError(t, env.client.Database(delDbName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltBranch", Value: int32(1)},
			{Key: "branch", Value: "merged-branch"},
			{Key: "d", Value: true},
		}).Decode(&result), "safe delete of a merged branch must succeed")

		assert.Equal(t, "merged-branch", result["branch"])
		assert.EqualValues(t, 1, result["ok"])
	})

	// -------------------------------------------------------------------------
	// Scenario 7: Safe delete (-d) — branch has unmerged commits, rejected
	// -------------------------------------------------------------------------
	t.Run("Scenario7_SafeDelete_UnmergedBranch_Rejected", func(t *testing.T) {
		delDbName := fmt.Sprintf("branchvrfy_unm%d", rand.Int64N(1_000_000))
		branchVerifySetup(t, env, delDbName)

		// Create "unmerged-branch" from main and advance it with an extra commit.
		require.NoError(t, env.client.Database(delDbName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltBranch", Value: int32(1)},
			{Key: "branch", Value: "unmerged-branch"},
		}).Err(), "creating unmerged-branch must succeed")

		_, err := env.client.Database(delDbName+"__unmerged-branch").Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(99)},
			{Key: "label", Value: "extra"},
		})
		require.NoError(t, err)
		docudoltCommit(t, env, delDbName+"__unmerged-branch", "extra commit on unmerged-branch")

		// Safe delete must be rejected because unmerged-branch has a commit not in main.
		err = env.client.Database(delDbName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltBranch", Value: int32(1)},
			{Key: "branch", Value: "unmerged-branch"},
			{Key: "d", Value: true},
		}).Err()
		require.Error(t, err, "safe delete of a branch with unmerged commits must fail")
	})

	// -------------------------------------------------------------------------
	// Scenario 8: Force delete (-D) — branch has unmerged commits, succeeds
	// -------------------------------------------------------------------------
	t.Run("Scenario8_ForceDelete_UnmergedBranch", func(t *testing.T) {
		delDbName := fmt.Sprintf("branchvrfy_frc%d", rand.Int64N(1_000_000))
		branchVerifySetup(t, env, delDbName)

		// Create "force-branch" and advance it with an extra commit.
		require.NoError(t, env.client.Database(delDbName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltBranch", Value: int32(1)},
			{Key: "branch", Value: "force-branch"},
		}).Err(), "creating force-branch must succeed")

		_, err := env.client.Database(delDbName+"__force-branch").Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(77)},
			{Key: "label", Value: "gone"},
		})
		require.NoError(t, err)
		docudoltCommit(t, env, delDbName+"__force-branch", "unmerged commit on force-branch")

		// Force delete must succeed regardless of merge status.
		var result bson.M
		require.NoError(t, env.client.Database(delDbName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltBranch", Value: int32(1)},
			{Key: "branch", Value: "force-branch"},
			{Key: "D", Value: true},
		}).Decode(&result), "force delete must succeed even with unmerged commits")

		assert.Equal(t, "force-branch", result["branch"])
		assert.EqualValues(t, 1, result["ok"])
	})
}
