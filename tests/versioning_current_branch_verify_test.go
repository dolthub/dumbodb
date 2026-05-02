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

// TestCurrentBranchVerify is the automated analog of docs/verify/current-branch.md.
//
// Each top-level subtest corresponds to one scenario in that document.
// The setup reproduces the manual setup block exactly:
//
//   - Commit 1 (hash1): items = [ { _id:1, v:"first" } ]
//   - Commit 2 (hash2): items = [ { _id:1, ... }, { _id:2, v:"second" } ]
//   - Branch "feature" pointing at commit 2 (same as main HEAD)
//
// Subtests run sequentially (no t.Parallel inside) so they share a single
// database and build on the same setup.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// currentBranchVerifySetup mirrors the Setup section of docs/verify/current-branch.md.
// Returns hash1 (commit 1) and hash2 (commit 2).
func currentBranchVerifySetup(t *testing.T, env *dumboDBTestEnv, dbName string) (hash1, hash2 string) {
	t.Helper()

	ctx := context.Background()
	db := env.client.Database(dbName)
	items := db.Collection("items")

	require.NoError(t, db.Drop(ctx))

	// Insert first document and commit.
	_, err := items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "v", Value: "first"},
	})
	require.NoError(t, err)
	hash1 = dumboDBCommit(t, env, dbName, "first commit", "alice <alice@acme.com>")

	// Insert second document and commit.
	_, err = items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "v", Value: "second"},
	})
	require.NoError(t, err)
	hash2 = dumboDBCommit(t, env, dbName, "second commit", "alice <alice@acme.com>")

	// Create branch "feature" from main HEAD.
	var branchResult bson.M
	err = env.client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "feature"},
	}).Decode(&branchResult)
	require.NoError(t, err, "doltBranch to create 'feature'")
	assert.Equal(t, "feature", branchResult["branch"])

	return hash1, hash2
}

func TestCurrentBranchVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("cbrvrfy%d", rand.Int64N(1_000_000))

	hash1, _ := currentBranchVerifySetup(t, env, dbName)

	// -------------------------------------------------------------------------
	// Scenario 1: Plain db name  -- returns "main"
	// -------------------------------------------------------------------------
	t.Run("Scenario1_PlainDbName_ReturnsMain", func(t *testing.T) {
		var result bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Decode(&result))
		assert.Equal(t, "main", result["branch"])
		assert.EqualValues(t, 1, result["ok"])
	})

	// -------------------------------------------------------------------------
	// Scenario 2: branchdb@main  -- returns "main"
	// -------------------------------------------------------------------------
	t.Run("Scenario2_ExplicitMain_ReturnsMain", func(t *testing.T) {
		var result bson.M
		require.NoError(t, env.client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Decode(&result))
		assert.Equal(t, "main", result["branch"])
		assert.EqualValues(t, 1, result["ok"])
	})

	// -------------------------------------------------------------------------
	// Scenario 3: branchdb@feature  -- returns "feature"
	// -------------------------------------------------------------------------
	t.Run("Scenario3_FeatureBranch_ReturnsFeature", func(t *testing.T) {
		var result bson.M
		require.NoError(t, env.client.Database(dbName+"@feature").RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Decode(&result))
		assert.Equal(t, "feature", result["branch"])
		assert.EqualValues(t, 1, result["ok"])
	})

	// -------------------------------------------------------------------------
	// Scenario 4: branchdb@<hash>  -- error (code 96)
	// -------------------------------------------------------------------------
	t.Run("Scenario4_CommitHash_ReturnsError96", func(t *testing.T) {
		err := env.client.Database(dbName+"@"+hash1).RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Err()
		assertWriteBlockedOperationFailed(t, err, "doltCurrentBranch on commit hash rootish")
	})

	// -------------------------------------------------------------------------
	// Scenario 5: branchdb@main~1  -- error (code 96)
	// -------------------------------------------------------------------------
	t.Run("Scenario5_AncestorExpression_ReturnsError96", func(t *testing.T) {
		err := env.client.Database(dbName+"@main~1").RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Err()
		assertWriteBlockedOperationFailed(t, err, "doltCurrentBranch on ancestor expression rootish")
	})
}
