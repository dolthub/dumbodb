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

// TestCommitVerify is the automated analog of docs/verify/commit.md.
//
// Each top-level subtest corresponds to one scenario in that document.
// The setup reproduces the manual setup block exactly:
//
//   - Baseline commit (hashBase): items = [ {_id:1, label:"alpha", v:1},
//     {_id:2, label:"beta", v:2} ]
//
// Subtests run sequentially (no t.Parallel inside) so they share a single
// database and the side effects of one scenario carry into the next.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// commitVerifySetup mirrors the Setup section of docs/verify/commit.md.
// Returns hashBase (the baseline commit hash).
func commitVerifySetup(t *testing.T, env *docudoltTestEnv, dbName string) (hashBase string) {
	t.Helper()

	ctx := context.Background()
	db := env.client.Database(dbName)
	items := db.Collection("items")

	require.NoError(t, db.Drop(ctx))

	_, err := items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "label", Value: "alpha"},
		{Key: "v", Value: int32(1)},
	})
	require.NoError(t, err)

	_, err = items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "label", Value: "beta"},
		{Key: "v", Value: int32(2)},
	})
	require.NoError(t, err)

	hashBase = docudoltCommit(t, env, dbName, "baseline")
	return hashBase
}

func TestCommitVerify(t *testing.T) {
	env := startDocudolt(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("commitvrfy%d", rand.Int64N(1_000_000))

	hashBase := commitVerifySetup(t, env, dbName)
	_ = hashBase

	// -------------------------------------------------------------------------
	// Scenario 1: Response shape — hash, branch, message, author, timestamp, ok
	// -------------------------------------------------------------------------
	t.Run("Scenario1_ResponseShape", func(t *testing.T) {
		var result bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltCommit", Value: int32(1)},
			{Key: "message", Value: "shape check"},
			{Key: "author", Value: "alice <alice@docudolt>"},
		}).Decode(&result))

		hash, ok := result["commitId"].(string)
		require.True(t, ok, "hash must be a string, got %T", result["commitId"])
		assert.NotEmpty(t, hash, "hash must not be empty")

		assert.Equal(t, "main", result["branch"], "branch must be 'main' for plain db name")
		assert.Equal(t, "shape check", result["message"], "message must echo the provided string")
		assert.Equal(t, "alice <alice@docudolt>", result["author"], "author must echo the provided value")
		assert.NotNil(t, result["timestamp"], "timestamp must be present")
		assert.EqualValues(t, 1, result["ok"], "ok must be 1")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: Commit on a named branch — data is committed to that branch
	// -------------------------------------------------------------------------
	t.Run("Scenario2_NamedBranch_CommitGoesToBranch", func(t *testing.T) {
		// Create a "feature" branch from main HEAD.
		var branchResult bson.M
		err := env.client.Database(dbName+"__d_main").RunCommand(ctx, bson.D{
			{Key: "docudoltBranch", Value: int32(1)},
			{Key: "branch", Value: "feature"},
		}).Decode(&branchResult)
		require.NoError(t, err, "docudoltBranch must succeed")
		assert.Equal(t, "feature", branchResult["branch"])

		// Insert a document on the feature branch.
		featureDB := env.client.Database(dbName + "__d_feature")
		_, err = featureDB.Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)},
			{Key: "label", Value: "gamma"},
			{Key: "v", Value: int32(3)},
		})
		require.NoError(t, err)

		// Commit on feature — verify response shape.
		var commitResult bson.M
		require.NoError(t, featureDB.RunCommand(ctx, bson.D{
			{Key: "docudoltCommit", Value: int32(1)},
			{Key: "message", Value: "feature commit"},
			{Key: "author", Value: "testuser"},
		}).Decode(&commitResult))

		hash, ok := commitResult["commitId"].(string)
		require.True(t, ok, "hash must be a string")
		assert.NotEmpty(t, hash, "hash must not be empty")
		assert.Equal(t, "feature commit", commitResult["message"])
		assert.EqualValues(t, 1, commitResult["ok"])

		// Verify the commit went to the feature branch: feature has 3 docs, main has 2.
		featureCount, err := featureDB.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), featureCount, "feature branch must have 3 documents after commit")

		mainCount, err := env.client.Database(dbName+"__d_main").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), mainCount, "main must still have 2 documents (feature commit must not affect main)")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: Successive commits have distinct hashes
	// -------------------------------------------------------------------------
	t.Run("Scenario3_SuccessiveCommitsHaveDistinctHashes", func(t *testing.T) {
		// Commit A: insert _id:10
		_, err := env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(10)},
			{Key: "label", Value: "ten"},
			{Key: "v", Value: int32(10)},
		})
		require.NoError(t, err)

		var resultA bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltCommit", Value: int32(1)},
			{Key: "message", Value: "commit A"},
			{Key: "author", Value: "testuser"},
		}).Decode(&resultA))
		hashA, _ := resultA["commitId"].(string)
		require.NotEmpty(t, hashA)

		// Commit B: insert _id:11
		_, err = env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(11)},
			{Key: "label", Value: "eleven"},
			{Key: "v", Value: int32(11)},
		})
		require.NoError(t, err)

		var resultB bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltCommit", Value: int32(1)},
			{Key: "message", Value: "commit B"},
			{Key: "author", Value: "testuser"},
		}).Decode(&resultB))
		hashB, _ := resultB["commitId"].(string)
		require.NotEmpty(t, hashB)

		assert.NotEqual(t, hashA, hashB, "successive commits must return distinct hashes")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: Commit on empty working set succeeds
	// -------------------------------------------------------------------------
	t.Run("Scenario4_EmptyWorkingSetCommitSucceeds", func(t *testing.T) {
		// After Scenario 3, all changes are committed. No pending changes exist.
		var result bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltCommit", Value: int32(1)},
			{Key: "message", Value: "empty"},
			{Key: "author", Value: "testuser"},
		}).Decode(&result))

		hash, ok := result["commitId"].(string)
		require.True(t, ok, "hash must be a string even for an empty commit")
		assert.NotEmpty(t, hash, "hash must not be empty")
		assert.EqualValues(t, 1, result["ok"], "ok must be 1")
	})

	// -------------------------------------------------------------------------
	// Scenario 5: Committed hash is a valid docudoltDiff reference
	// -------------------------------------------------------------------------
	t.Run("Scenario5_HashIsValidDiffReference", func(t *testing.T) {
		// Commit current state and save hashBefore.
		hashBefore := docudoltCommit(t, env, dbName, "pre-change")

		// Insert _id:99 and commit — save hashAfter.
		_, err := env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(99)},
			{Key: "label", Value: "new"},
			{Key: "v", Value: int32(99)},
		})
		require.NoError(t, err)

		hashAfter := docudoltCommit(t, env, dbName, "post-change")

		// Diff from hashBefore to hashAfter must show _id:99 as added.
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltDiff", Value: int32(1)},
			{Key: "from", Value: hashBefore},
			{Key: "to", Value: hashAfter},
		}).Decode(&raw))

		dr := decodeDiffResult(t, raw)
		cd := findCollDiff(dr, "items")
		require.NotNil(t, cd, "expected diff for 'items' collection")
		require.Len(t, cd.Added, 1, "expected exactly 1 added document")
		assert.Equal(t, int32(99), cd.Added[0]["_id"], "added doc must be _id:99")
		assert.Empty(t, cd.Removed, "no documents should be removed")
		assert.Empty(t, cd.Modified, "no documents should be modified")
	})

	// -------------------------------------------------------------------------
	// Scenario 6: Author is echoed in the response and visible in docudoltLog
	// -------------------------------------------------------------------------
	t.Run("Scenario6_AuthorEchoedAndVisibleInLog", func(t *testing.T) {
		var result bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltCommit", Value: int32(1)},
			{Key: "message", Value: "authored commit"},
			{Key: "author", Value: "bob <bob@docudolt>"},
		}).Decode(&result))

		assert.Equal(t, "bob <bob@docudolt>", result["author"], "author must be echoed in response")
		assert.NotNil(t, result["timestamp"], "timestamp must be present in response")
		assert.EqualValues(t, 1, result["ok"])

		// Verify the author is stored and visible via docudoltLog.
		var logResult bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logResult))

		commits, ok := logResult["commits"].(bson.A)
		require.True(t, ok, "commits must be an array")
		require.Len(t, commits, 1, "expected 1 commit in log")

		entry, ok := commits[0].(bson.M)
		require.True(t, ok)
		assert.Equal(t, "bob <bob@docudolt>", entry["author"], "docudoltLog must reflect the commit author")
	})

	// -------------------------------------------------------------------------
	// Scenario 7: Custom timestamp is stored and echoed
	// -------------------------------------------------------------------------
	t.Run("Scenario7_CustomTimestampStoredAndEchoed", func(t *testing.T) {
		fixedTime := time.Date(2020, 6, 15, 12, 0, 0, 0, time.UTC)

		var result bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltCommit", Value: int32(1)},
			{Key: "message", Value: "fixed-time commit"},
			{Key: "author", Value: "carol <carol@docudolt>"},
			{Key: "timestamp", Value: fixedTime},
		}).Decode(&result))

		assert.Equal(t, "carol <carol@docudolt>", result["author"], "author must be echoed")
		assert.EqualValues(t, 1, result["ok"])

		// The echoed timestamp must equal the provided value (within millisecond precision).
		echoedTS, ok := result["timestamp"].(primitive.DateTime)
		require.True(t, ok, "timestamp must be a BSON datetime, got %T", result["timestamp"])
		assert.Equal(t, fixedTime.UnixMilli(), int64(echoedTS),
			"echoed timestamp must match the provided fixed time")

		// Verify via docudoltLog that the stored timestamp matches.
		var logResult bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&logResult))

		commits, ok := logResult["commits"].(bson.A)
		require.True(t, ok)
		require.Len(t, commits, 1)

		entry, ok := commits[0].(bson.M)
		require.True(t, ok)
		logTS, ok := entry["timestamp"].(primitive.DateTime)
		require.True(t, ok, "log timestamp must be a BSON datetime")
		assert.Equal(t, fixedTime.UnixMilli(), int64(logTS),
			"docudoltLog timestamp must match the provided fixed time")
	})
}
