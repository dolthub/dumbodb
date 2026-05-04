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

// TestRootishVerify is the automated analog of docs/verify/rootish.md.
//
// Setup:
//   - Commit 1 (hash1): items = [ { _id:1, label:"first", version:1 } ]
//   - Commit 2 (hash2): items = [ { _id:1, ... }, { _id:2, label:"second", version:2 } ]
//   - Branch "feature" at commit 2
//   - Tag "release-1" at commit 1
//
// Subtests run sequentially (no t.Parallel) so side effects carry forward.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// rootishVerifySetup mirrors the Setup section of docs/verify/rootish.md.
func rootishVerifySetup(t *testing.T, env *dumboDBTestEnv, dbName string) (hash1, hash2 string) {
	t.Helper()

	ctx := context.Background()
	db := env.client.Database(dbName)
	items := db.Collection("items")

	require.NoError(t, db.Drop(ctx))

	// Commit 1: one document.
	_, err := items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "label", Value: "first"},
		{Key: "version", Value: int32(1)},
	})
	require.NoError(t, err)
	hash1 = dumboDBCommit(t, env, dbName, "first commit", "alice <alice@acme.com>")

	// Commit 2: two documents.
	_, err = items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "label", Value: "second"},
		{Key: "version", Value: int32(2)},
	})
	require.NoError(t, err)
	hash2 = dumboDBCommit(t, env, dbName, "second commit", "bob <bob@widgets.io>")

	// Create branch "feature" from main HEAD.
	var branchResult bson.M
	err = env.client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "feature"},
	}).Decode(&branchResult)
	require.NoError(t, err, "doltBranch to create feature")
	assert.Equal(t, "feature", branchResult["branch"])

	// Create tag "release-1" at commit 1.
	var tagResult bson.M
	err = env.client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "dumboTag", Value: int32(1)},
		{Key: "name", Value: "release-1"},
		{Key: "hash", Value: hash1},
	}).Decode(&tagResult)
	require.NoError(t, err, "dumboTag to create release-1")
	assert.EqualValues(t, 1, tagResult["ok"])

	return hash1, hash2
}

func TestRootishVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("vrfy%d", rand.Int64N(1_000_000))

	hash1, hash2 := rootishVerifySetup(t, env, dbName)

	// -------------------------------------------------------------------------
	// Scenario 1: @main -- reads and writes work
	// -------------------------------------------------------------------------
	t.Run("Scenario1_MainBranch", func(t *testing.T) {
		items := env.client.Database(dbName + "@main").Collection("items")

		n, err := items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), n, "main: expected 2 docs")

		_, err = items.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)},
			{Key: "label", Value: "third"},
		})
		require.NoError(t, err, "insert into main must succeed")

		n, err = items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), n)

		_, err = items.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(3)}})
		require.NoError(t, err)
	})

	// -------------------------------------------------------------------------
	// Scenario 2: @feature -- branch isolation
	// -------------------------------------------------------------------------
	t.Run("Scenario2_BranchIsolation", func(t *testing.T) {
		featItems := env.client.Database(dbName + "@feature").Collection("items")
		mainItems := env.client.Database(dbName + "@main").Collection("items")

		n, err := featItems.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), n, "feature: expected 2 docs")

		_, err = featItems.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(10)},
			{Key: "label", Value: "feature-only"},
		})
		require.NoError(t, err)

		nFeat, err := featItems.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), nFeat, "feature: 3 after insert")

		nMain, err := mainItems.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), nMain, "main: still 2 (isolated)")

		_, err = featItems.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(10)}})
		require.NoError(t, err)
	})

	// -------------------------------------------------------------------------
	// Scenario 3: @release-1 -- tag is read-only
	// -------------------------------------------------------------------------
	t.Run("Scenario3_TagReadOnly", func(t *testing.T) {
		tagItems := env.client.Database(dbName + "@release-1").Collection("items")

		n, err := tagItems.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), n, "release-1 tag: expected 1 doc (commit 1)")

		// Write must fail -- tags are read-only.
		_, err = tagItems.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}})
		assertWriteBlockedOperationFailed(t, err, "insert on tag")

		// Branch creation from tag works.
		tagDB := env.client.Database(dbName + "@release-1")
		var branchResult bson.M
		require.NoError(t, tagDB.RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "from-tag"},
		}).Decode(&branchResult))
		assert.Equal(t, "from-tag", branchResult["branch"])

		nFromTag, err := env.client.Database(dbName+"@from-tag").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), nFromTag, "from-tag: expected 1 doc")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: @<hash> -- commit hash is read-only
	// -------------------------------------------------------------------------
	t.Run("Scenario4_CommitHash", func(t *testing.T) {
		snap1 := env.client.Database(dbName + "@" + hash1).Collection("items")
		snap2 := env.client.Database(dbName + "@" + hash2).Collection("items")

		n1, err := snap1.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), n1, "hash1: expected 1 doc")

		n2, err := snap2.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), n2, "hash2: expected 2 docs")

		_, err = snap1.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}})
		assertWriteBlockedOperationFailed(t, err, "insert on hash snapshot")

		var branchResult bson.M
		require.NoError(t, env.client.Database(dbName+"@"+hash1).RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "from-hash1"},
		}).Decode(&branchResult))
		assert.Equal(t, "from-hash1", branchResult["branch"])

		nNew, err := env.client.Database(dbName+"@from-hash1").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), nNew, "from-hash1: expected 1 doc")
	})

	// -------------------------------------------------------------------------
	// Scenario 5: @main~1 -- ancestor expression is read-only
	// -------------------------------------------------------------------------
	// -------------------------------------------------------------------------
	// Scenario 5: HEAD is rejected
	// -------------------------------------------------------------------------
	t.Run("Scenario5_HEAD_Rejected", func(t *testing.T) {
		assertRootishRejected(t, env.client.Database(dbName+"@HEAD"), "HEAD")
		assertRootishRejected(t, env.client.Database(dbName+"@HEAD~1"), "HEAD~1")
		assertRootishRejected(t, env.client.Database(dbName+"@HEAD^"), "HEAD^")
	})

	// -------------------------------------------------------------------------
	// Scenario 6: Tilde, caret, and chained traversal
	// -------------------------------------------------------------------------
	t.Run("Scenario6_Tilde_Caret_Chained", func(t *testing.T) {
		// Tilde: main~1 = first parent (read-only).
		n, err := env.client.Database(dbName+"@main~1").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), n, "main~1: expected 1 doc")

		// main~0 = main itself.
		nSame, err := env.client.Database(dbName+"@main~0").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), nSame, "main~0: expected 2 docs")

		// Write blocked on ancestor expression.
		_, err = env.client.Database(dbName+"@main~1").Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}})
		assertWriteBlockedOperationFailed(t, err, "insert on main~1")

		// Caret: main^ = first parent (same as main~1 for non-merge).
		nCaret, err := env.client.Database(dbName+"@main^").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), nCaret, "main^: expected 1 doc")

		_, err = env.client.Database(dbName+"@main^").Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}})
		assertWriteBlockedOperationFailed(t, err, "insert on main^")

		// Caret parent selection and chaining require a merge commit.
		chainDB := fmt.Sprintf("chain%d", rand.Int64N(1_000_000))

		mainCol := env.client.Database(chainDB + "@main").Collection("items")
		_, err = mainCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "root"}})
		require.NoError(t, err)
		hashC1 := dumboDBCommit(t, env, chainDB+"@main", "C1-root", "alice <alice@acme.com>")

		require.NoError(t, env.client.Database(chainDB+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "feature"},
		}).Err())

		featCol := env.client.Database(chainDB + "@feature").Collection("items")
		_, err = featCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "feat"}})
		require.NoError(t, err)
		hashC2 := dumboDBCommit(t, env, chainDB+"@feature", "C2-feature", "bob <bob@widgets.io>")

		_, err = mainCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "v", Value: "main-adv"}})
		require.NoError(t, err)
		hashC3 := dumboDBCommit(t, env, chainDB+"@main", "C3-main", "alice <alice@acme.com>")

		mergeRaw := runCommandRaw(t, env.client.Database(chainDB+"@main"), bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "merge_in", Value: "feature"},
		})
		require.EqualValues(t, 1, mergeRaw["ok"], "merge must succeed")

		getCommitID := func(rootish string) string {
			var logRes bson.M
			require.NoError(t, env.client.Database(chainDB+"@"+rootish).RunCommand(ctx, bson.D{
				{Key: "doltLog", Value: int32(1)}, {Key: "limit", Value: int32(1)},
			}).Decode(&logRes))
			return logRes["commits"].(bson.A)[0].(bson.M)["commitId"].(string)
		}

		// ^1 = first parent (C3)
		assert.Equal(t, hashC3, getCommitID("main^1"), "main^1 must be C3")

		// ^2 = second parent (C2, feature tip)
		assert.Equal(t, hashC2, getCommitID("main^2"), "main^2 must be C2")

		// ^0 = the merge commit itself
		assert.Equal(t, mergeRaw["commitId"], getCommitID("main^0"), "main^0 must be the merge commit")

		// Chained: ^1~1 = parent of first parent = C1
		assert.Equal(t, hashC1, getCommitID("main^1~1"), "main^1~1 must be C1")

		// Chained: ~1^1 = first parent of (first parent of HEAD) = C1
		assert.Equal(t, hashC1, getCommitID("main~1^1"), "main~1^1 must be C1")

		// ^^ = first parent of first parent = C1
		assert.Equal(t, hashC1, getCommitID("main^^"), "main^^ must be C1")

		_ = hashC1 // used above
	})

	// -------------------------------------------------------------------------
	// Scenario 7: Percent-encoding for special characters
	// -------------------------------------------------------------------------
	t.Run("Scenario7_PercentEncoding", func(t *testing.T) {
		// Create branch "v1.0" (dot in name).
		require.NoError(t, env.client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "v1.0"},
		}).Err())

		// Unencoded dot fails -- the Go driver sends it but the server
		// parses the name incorrectly (dot is a namespace separator).
		_, unencodedErr := env.client.Database(dbName + "@v1.0").Collection("items").CountDocuments(ctx, bson.D{})
		require.Error(t, unencodedErr, "unencoded dot in rootish must fail")

		// Percent-encoded dot works.
		v1Items := env.client.Database(dbName + "@v1%2E0").Collection("items")

		n, err := v1Items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), n, "v1.0 via percent-encoding: expected 2 docs")

		// Write works -- it is a branch.
		_, err = v1Items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(20)}, {Key: "label", Value: "encoded"}})
		require.NoError(t, err)

		nV1, err := v1Items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), nV1)

		// main unchanged.
		nMain, err := env.client.Database(dbName+"@main").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), nMain)

		_, err = v1Items.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(20)}})
		require.NoError(t, err)
	})

	// -------------------------------------------------------------------------
	// Scenario 8: reflog -- rejected (code 96)
	// -------------------------------------------------------------------------
	t.Run("Scenario8_Reflog_Rejected", func(t *testing.T) {
		assertRootishRejected(t, env.client.Database(dbName+"@main%40%7Byesterday%7D"), "reflog_yesterday")
		assertRootishRejected(t, env.client.Database(dbName+"@main%40%7B5%20minutes%20ago%7D"), "reflog_minutes_ago")
		assertRootishRejected(t, env.client.Database(dbName+"@%40%7B1%7D"), "reflog_bare")
	})

	// -------------------------------------------------------------------------
	// Scenario 9: range -- rejected (code 96)
	// -------------------------------------------------------------------------
	t.Run("Scenario9_Range_Rejected", func(t *testing.T) {
		assertRootishRejected(t, env.client.Database(dbName+"@main%2E%2Efeature"), "range_two_dot")
		assertRootishRejected(t, env.client.Database(dbName+"@main%2E%2E%2Efeature"), "range_three_dot")
	})

	_ = hash2 // used in Scenario4
}

// assertRootishRejected and assertWriteBlockedOperationFailed are defined in
// versioning_rootish_test.go and versioning_readonly_test.go respectively.
