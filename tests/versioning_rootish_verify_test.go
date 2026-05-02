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
// Each top-level subtest corresponds to one scenario in that document.
// The setup reproduces the manual setup block exactly:
//
//   - Commit 1 (hash1): items = [ { _id:1, label:"first",  version:1 } ]
//   - Commit 2 (hash2): items = [ { _id:1, ... }, { _id:2, label:"second", version:2 } ]
//   - Branch "v1.0" at commit 2 (same as main HEAD); accessed via dbname@v1%2E0
//
// Subtests run sequentially (no t.Parallel inside) so they share a single database
// and the side effects of one scenario (e.g. branch creation) carry into the next.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// rootishVerifySetup mirrors the Setup section of docs/verify/rootish.md.
// Returns hash1 (commit 1) and hash2 (commit 2).
func rootishVerifySetup(t *testing.T, env *dumboDBTestEnv, dbName string) (hash1, hash2 string) {
	t.Helper()

	ctx := context.Background()
	db := env.client.Database(dbName)
	items := db.Collection("items")

	// Drop any existing data (mirrors "Start fresh  -- drop if it exists").
	require.NoError(t, db.Drop(ctx))

	// Insert first document and commit.
	_, err := items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "label", Value: "first"},
		{Key: "version", Value: int32(1)},
	})
	require.NoError(t, err)
	hash1 = dumboDBCommit(t, env, dbName, "first commit", "alice <alice@acme.com>")

	// Insert second document and commit.
	_, err = items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "label", Value: "second"},
		{Key: "version", Value: int32(2)},
	})
	require.NoError(t, err)
	hash2 = dumboDBCommit(t, env, dbName, "second commit", "alice <alice@acme.com>")

	// Create branch "v1.0" from main HEAD.
	// The branch name contains a dot; access it via dbname@v1%2E0.
	var branchResult bson.M
	err = env.client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "v1.0"},
	}).Decode(&branchResult)
	require.NoError(t, err, "doltBranch to create v1.0")
	assert.Equal(t, "v1.0", branchResult["branch"])

	return hash1, hash2
}

func TestRootishVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	// Use a randomised db name so parallel test runs don't collide.
	dbName := fmt.Sprintf("vrfy%d", rand.Int64N(1_000_000))

	hash1, hash2 := rootishVerifySetup(t, env, dbName)

	// -------------------------------------------------------------------------
	// Scenario 1: verifydb@main  -- reads and writes work
	// -------------------------------------------------------------------------
	t.Run("Scenario1_MainBranch_ReadsAndWritesWork", func(t *testing.T) {
		main := env.client.Database(dbName + "@main")
		items := main.Collection("items")

		// Read: should return both documents.
		docs, err := items.Find(ctx, bson.D{})
		require.NoError(t, err)
		var rows []bson.M
		require.NoError(t, docs.All(ctx, &rows))
		assert.Len(t, rows, 2, "main items: expected 2 docs")

		// Write: insert a third document then remove it to keep state clean.
		_, err = items.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)},
			{Key: "label", Value: "third"},
			{Key: "version", Value: int32(3)},
		})
		require.NoError(t, err, "insert into main must succeed")

		n, err := items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), n, "main items: expected 3 docs after insert")

		_, err = items.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(3)}})
		require.NoError(t, err, "delete from main must succeed")

		// dumboDBCurrentBranch returns "main".
		var result bson.M
		require.NoError(t, main.RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Decode(&result))
		assert.Equal(t, "main", result["branch"])
		assert.EqualValues(t, 1, result["ok"])
	})

	// -------------------------------------------------------------------------
	// Scenario 2: verifydb@v1%2E0  -- reads and writes work, isolated from main
	// -------------------------------------------------------------------------
	t.Run("Scenario2_BranchV1_ReadsWritesIsolated", func(t *testing.T) {
		// Access the v1.0 branch via its percent-encoded name.
		v1DB := env.client.Database(dbName + "@v1%2E0")
		v1Items := v1DB.Collection("items")
		mainItems := env.client.Database(dbName + "@main").Collection("items")

		// Read: returns both documents (same as main HEAD at setup time).
		docs, err := v1Items.Find(ctx, bson.D{})
		require.NoError(t, err)
		var rows []bson.M
		require.NoError(t, docs.All(ctx, &rows))
		assert.Len(t, rows, 2, "v1.0 items: expected 2 docs")

		// Write on v1.0: inserts into the v1.0 working set, NOT main's.
		_, err = v1Items.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(10)},
			{Key: "label", Value: "v1-only"},
		})
		require.NoError(t, err, "insert into v1.0 branch must succeed")

		// v1.0 now has three documents.
		nV1, err := v1Items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), nV1, "v1.0 items: expected 3 docs after insert")

		// main is unchanged  -- the v1.0 write is isolated.
		nMain, err := mainItems.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), nMain, "main items: must still be 2 (v1.0 write must not leak)")

		// dumboDBCurrentBranch on v1.0 returns the decoded branch name "v1.0".
		var result bson.M
		require.NoError(t, v1DB.RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Decode(&result))
		assert.Equal(t, "v1.0", result["branch"])

		// Clean up the test write.
		_, err = v1Items.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(10)}})
		require.NoError(t, err, "cleanup delete from v1.0 must succeed")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: verifydb@<hash>  -- correct snapshot, writes blocked, branch OK
	// -------------------------------------------------------------------------
	t.Run("Scenario3_CommitHash", func(t *testing.T) {
		snap1DB := env.client.Database(dbName + "@" + hash1)
		snap1Items := snap1DB.Collection("items")
		snap2Items := env.client.Database(dbName + "@" + hash2).Collection("items")

		// hash1: one document only.
		n1, err := snap1Items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), n1, "hash1 snapshot: expected 1 doc")

		var snap1Rows []bson.M
		cur, err := snap1Items.Find(ctx, bson.D{})
		require.NoError(t, err)
		require.NoError(t, cur.All(ctx, &snap1Rows))
		require.Len(t, snap1Rows, 1)
		assert.Equal(t, int32(1), snap1Rows[0]["_id"])

		// hash2: two documents.
		n2, err := snap2Items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), n2, "hash2 snapshot: expected 2 docs")

		// Write on commit hash: must fail (code 96).
		_, err = snap1Items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}})
		assertWriteBlockedOperationFailed(t, err, "insert on hash1 snapshot")

		// dumboDBCurrentBranch: no branch name to return (code 96).
		err = snap1DB.RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Err()
		assertWriteBlockedOperationFailed(t, err, "doltCurrentBranch on hash rootish")

		// dumboDBBranch: works  -- branch creation needs only a resolved commit address.
		var branchResult bson.M
		require.NoError(t, snap1DB.RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "from-hash1"},
		}).Decode(&branchResult))
		assert.Equal(t, "from-hash1", branchResult["branch"])

		// Verify the new branch sees the hash1 state (one document).
		nNew, err := env.client.Database(dbName+"@from-hash1").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), nNew, "from-hash1 branch: expected 1 doc (hash1 state)")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: verifydb@main~1  -- ancestor data, writes blocked, branch OK
	// -------------------------------------------------------------------------
	t.Run("Scenario4_AncestorExpression", func(t *testing.T) {
		parentDB := env.client.Database(dbName + "@main~1")
		parentItems := parentDB.Collection("items")

		// main~1 is the parent of HEAD (commit 1: one document).
		nParent, err := parentItems.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), nParent, "main~1: expected 1 doc")

		var parentRows []bson.M
		cur, err := parentItems.Find(ctx, bson.D{})
		require.NoError(t, err)
		require.NoError(t, cur.All(ctx, &parentRows))
		require.Len(t, parentRows, 1)
		assert.Equal(t, int32(1), parentRows[0]["_id"])

		// main~0 is main itself (same as current HEAD): two documents.
		nSame, err := env.client.Database(dbName+"@main~0").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), nSame, "main~0: expected 2 docs (same as HEAD)")

		// Write on ancestor expression: must fail (code 96).
		_, err = parentItems.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}})
		assertWriteBlockedOperationFailed(t, err, "insert on main~1")

		// dumboDBCurrentBranch: no branch name to return (code 96).
		err = parentDB.RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Err()
		assertWriteBlockedOperationFailed(t, err, "doltCurrentBranch on ancestor rootish")

		// dumboDBBranch: works  -- ancestor expression resolves to a commit.
		var branchResult bson.M
		require.NoError(t, parentDB.RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "back-one"},
		}).Decode(&branchResult))
		assert.Equal(t, "back-one", branchResult["branch"])

		// Verify back-one is at the main~1 state (one document).
		nNew, err := env.client.Database(dbName+"@back-one").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), nNew, "back-one branch: expected 1 doc (main~1 state)")
	})

	// -------------------------------------------------------------------------
	// Scenario 5: verifydb@HEAD  -- aliases main; HEAD~N aliases main~N.
	// HEAD^ aliases main^ (first parent); HEAD^2 selects second parent on merge commits.
	// -------------------------------------------------------------------------
	t.Run("Scenario5_HEAD_AliasesMain", func(t *testing.T) {
		headDB := env.client.Database(dbName + "@HEAD")
		headItems := headDB.Collection("items")

		// Read via HEAD returns the same two docs main has at HEAD.
		nHead, err := headItems.CountDocuments(ctx, bson.D{})
		require.NoError(t, err, "find on HEAD must succeed")
		assert.Equal(t, int64(2), nHead, "HEAD: expected 2 docs (same as main HEAD)")

		// doltCurrentBranch reports "main" because HEAD was rewritten.
		var branchResult bson.M
		require.NoError(t, headDB.RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Decode(&branchResult))
		assert.Equal(t, "main", branchResult["branch"], "HEAD resolves to main")

		// Write via HEAD goes to main's working set. Insert, verify visible on main,
		// then clean up to keep state stable for later scenarios.
		_, err = headItems.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(5)},
			{Key: "label", Value: "via-HEAD"},
		})
		require.NoError(t, err, "write via HEAD must succeed (HEAD aliases main)")

		mainItems := env.client.Database(dbName + "@main").Collection("items")
		nMain, err := mainItems.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), nMain, "HEAD write must be visible on main  -- same working set")

		_, err = headItems.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(5)}})
		require.NoError(t, err, "cleanup delete via HEAD must succeed")

		// HEAD~1 aliases main~1  -- read returns commit 1 state (one document),
		// writes blocked with code 96.
		prevDB := env.client.Database(dbName + "@HEAD~1")
		prevItems := prevDB.Collection("items")

		nPrev, err := prevItems.CountDocuments(ctx, bson.D{})
		require.NoError(t, err, "find on HEAD~1 must succeed")
		assert.Equal(t, int64(1), nPrev, "HEAD~1: expected 1 doc (main~1 state)")

		_, err = prevItems.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}})
		assertWriteBlockedOperationFailed(t, err, "insert on HEAD~1")

		// HEAD^ is read-only (first parent of HEAD). Reads succeed, writes are blocked.
		caretDB := env.client.Database(dbName + "@HEAD^")
		nCaret, err := caretDB.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err, "find on HEAD^ must succeed")
		assert.Equal(t, int64(1), nCaret, "HEAD^: expected 1 doc (first parent = commit 1)")

		_, err = caretDB.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}})
		assertWriteBlockedOperationFailed(t, err, "insert on HEAD^")
	})

	// -------------------------------------------------------------------------
	// Scenario 6: reflog  -- rejected on first command (code 96).
	// Uses percent-encoded DB names matching the docs: raw @, {, }, space are
	// invalid in mongosh DB names, so the docs show percent-encoded forms.
	// Exercising the same encoded forms here proves a mongosh user following
	// the doc actually reaches the server-side parser.
	// -------------------------------------------------------------------------
	t.Run("Scenario6_Reflog_AnyCommandFails", func(t *testing.T) {
		assertRootishRejected(t, env.client.Database(dbName+"@main%40%7Byesterday%7D"), "reflog_yesterday")
		assertRootishRejected(t, env.client.Database(dbName+"@main%40%7B5%20minutes%20ago%7D"), "reflog_minutes_ago")
		assertRootishRejected(t, env.client.Database(dbName+"@%40%7B1%7D"), "reflog_bare")
	})

	// -------------------------------------------------------------------------
	// Scenario 7: range  -- rejected on first command (code 96).
	// Uses percent-encoded `.` (%2E) matching the docs: raw `.` is invalid in
	// MongoDB DB names and rejected by mongosh client-side.
	// -------------------------------------------------------------------------
	t.Run("Scenario7_Range_AnyCommandFails", func(t *testing.T) {
		assertRootishRejected(t, env.client.Database(dbName+"@main%2E%2Efeature"), "range_two_dot")
		assertRootishRejected(t, env.client.Database(dbName+"@main%2E%2E%2Efeature"), "range_three_dot")
	})

	// -------------------------------------------------------------------------
	// Scenario 8: Chained traversal expressions (^N, ~N combined)
	// Create a merge commit so we can test ^2, then chain operators.
	// -------------------------------------------------------------------------
	t.Run("Scenario8_ChainedTraversal", func(t *testing.T) {
		chainDB := fmt.Sprintf("chainvrfy%d", rand.Int64N(1_000_000))

		mainCol := env.client.Database(chainDB + "@main").Collection("items")

		// C1: root commit
		_, err := mainCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "root"}})
		require.NoError(t, err)
		hashC1 := dumboDBCommit(t, env, chainDB+"@main", "C1-root", "alice <alice@acme.com>")

		// Create feature branch, add a commit
		require.NoError(t, env.client.Database(chainDB+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "feature"},
		}).Err())

		featCol := env.client.Database(chainDB + "@feature").Collection("items")
		_, err = featCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "feat"}})
		require.NoError(t, err)
		hashC2 := dumboDBCommit(t, env, chainDB+"@feature", "C2-feature", "bob <bob@widgets.io>")

		// Advance main independently
		_, err = mainCol.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "v", Value: "main-adv"}})
		require.NoError(t, err)
		hashC3 := dumboDBCommit(t, env, chainDB+"@main", "C3-main", "alice <alice@acme.com>")

		// Merge feature into main (3-way merge)
		mergeRaw := runCommandRaw(t, env.client.Database(chainDB+"@main"), bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "merge_in", Value: "feature"},
		})
		require.EqualValues(t, 1, mergeRaw["ok"], "merge must succeed")
		_ = hashC1

		// Now main HEAD is the merge commit M with:
		//   M^1 = C3 (main's pre-merge tip)
		//   M^2 = C2 (feature's tip)
		//   M^1~1 = C1 (root)

		// Test main^1 = C3
		var logP1 bson.M
		require.NoError(t, env.client.Database(chainDB+"@main^1").RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)}, {Key: "limit", Value: int32(1)},
		}).Decode(&logP1))
		p1Head := logP1["commits"].(bson.A)[0].(bson.M)
		assert.Equal(t, hashC3, p1Head["commitId"], "main^1 must be C3")

		// Test main^2 = C2
		var logP2 bson.M
		require.NoError(t, env.client.Database(chainDB+"@main^2").RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)}, {Key: "limit", Value: int32(1)},
		}).Decode(&logP2))
		p2Head := logP2["commits"].(bson.A)[0].(bson.M)
		assert.Equal(t, hashC2, p2Head["commitId"], "main^2 must be C2")

		// Test chained: main^1~1 = C1 (parent of C3 = root)
		var logChain bson.M
		require.NoError(t, env.client.Database(chainDB+"@main^1~1").RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)}, {Key: "limit", Value: int32(1)},
		}).Decode(&logChain))
		chainHead := logChain["commits"].(bson.A)[0].(bson.M)
		assert.Equal(t, hashC1, chainHead["commitId"], "main^1~1 must be C1 (root)")

		// Test main^0 = the merge commit itself
		var logP0 bson.M
		require.NoError(t, env.client.Database(chainDB+"@main^0").RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)}, {Key: "limit", Value: int32(1)},
		}).Decode(&logP0))
		p0Head := logP0["commits"].(bson.A)[0].(bson.M)
		assert.Equal(t, mergeRaw["commitId"], p0Head["commitId"], "main^0 must be the merge commit")

		// Test main~1^1 = C1 (first parent of main's first parent)
		var logTildeCaret bson.M
		require.NoError(t, env.client.Database(chainDB+"@main~1^1").RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)}, {Key: "limit", Value: int32(1)},
		}).Decode(&logTildeCaret))
		tcHead := logTildeCaret["commits"].(bson.A)[0].(bson.M)
		assert.Equal(t, hashC1, tcHead["commitId"], "main~1^1 must be C1")

		// Test main^^ = C1 (first parent of first parent)
		var logDoubleCaret bson.M
		require.NoError(t, env.client.Database(chainDB+"@main^^").RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)}, {Key: "limit", Value: int32(1)},
		}).Decode(&logDoubleCaret))
		dcHead := logDoubleCaret["commits"].(bson.A)[0].(bson.M)
		assert.Equal(t, hashC1, dcHead["commitId"], "main^^ must be C1")
	})
}

// assertRootishRejectedCode96 checks that a RunCommand call against the given
// database returns OperationFailed (96). Used for commands other than find.
func assertRootishRejectedCode96(tb testing.TB, db *mongo.Database, cmd bson.D, op string) {
	tb.Helper()

	ctx := context.Background()
	err := db.RunCommand(ctx, cmd).Err()
	require.Error(tb, err, "%s: expected code-96 rejection", op)

	cmdErr, ok := err.(mongo.CommandError)
	require.True(tb, ok, "%s: expected CommandError, got %T: %v", op, err, err)
	assert.EqualValues(tb, 96, cmdErr.Code,
		"%s: expected OperationFailed (96), got %d: %s", op, cmdErr.Code, cmdErr.Message)
}
