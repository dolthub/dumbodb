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

// TestResetVerify is the automated analog of docs/verify/reset.md.
//
// Each top-level subtest corresponds to one scenario in that document.
// The setup reproduces the manual setup block exactly:
//
//   - Commit C1 (hashC1): tasks = [ {_id:1, v:1} ]
//   - Commit C2 (hashC2, HEAD): tasks = [ {_id:1, v:1}, {_id:2, v:2} ]
//   - Working set is clean after setup.
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
	"go.mongodb.org/mongo-driver/v2/bson"
)

// resetVerifySetup mirrors the Setup section of docs/verify/reset.md.
// Returns hashC1 and hashC2.
func resetVerifySetup(t *testing.T, env *dumboDBTestEnv, dbName string) (hashC1, hashC2 string) {
	t.Helper()

	ctx := context.Background()
	items := env.Client.Database(dbName).Collection("tasks")

	require.NoError(t, env.Client.Database(dbName).Drop(ctx))

	_, err := items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "v", Value: int32(1)},
	})
	require.NoError(t, err)
	hashC1 = dumboDBCommit(t, env, dbName, "initial", "alice <alice@acme.com>")

	_, err = items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "v", Value: int32(2)},
	})
	require.NoError(t, err)
	hashC2 = dumboDBCommit(t, env, dbName, "add-two", "bob <bob@widgets.io>")

	return hashC1, hashC2
}

func branchHead(t *testing.T, env *dumboDBTestEnv, connDB string) string {
	t.Helper()

	var logRaw bson.M
	require.NoError(t, env.Client.Database(connDB).RunCommand(context.Background(), bson.D{
		{Key: "doltLog", Value: int32(1)},
	}).Decode(&logRaw))

	commits, ok := logRaw["commits"].(bson.A)
	require.True(t, ok && len(commits) > 0, "doltLog must return at least one commit for %q", connDB)
	entry, ok := commits[0].(bson.M)
	require.True(t, ok, "first log entry must be a document")
	h, ok := entry["commitId"].(string)
	require.True(t, ok && h != "", "first log entry must have a non-empty commitId")
	return h
}

func resetBranchVerifySetup(t *testing.T, env *dumboDBTestEnv) (brDB, hashM1, hashF1, hashF2 string) {
	t.Helper()
	ctx := context.Background()

	brDB = fmt.Sprintf("resetbranchvrfy%d", rand.Int64N(1_000_000))
	mainDB := env.Client.Database(brDB)
	require.NoError(t, mainDB.Drop(ctx))

	_, err := mainDB.Collection("tasks").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)}, {Key: "v", Value: int32(1)},
	})
	require.NoError(t, err)
	hashM1 = dumboDBCommit(t, env, brDB, "main-base", "alice <alice@acme.com>")

	bsBranchCreate(t, env, brDB, "main", "feature")
	featDB := env.Client.Database(brDB + "@feature")

	_, err = featDB.Collection("tasks").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)}, {Key: "v", Value: int32(2)},
	})
	require.NoError(t, err)
	hashF1 = dumboDBCommit(t, env, brDB+"@feature", "feature-one", "carol <carol@acme.com>")

	_, err = featDB.Collection("tasks").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(3)}, {Key: "v", Value: int32(3)},
	})
	require.NoError(t, err)
	hashF2 = dumboDBCommit(t, env, brDB+"@feature", "feature-two", "carol <carol@acme.com>")
	require.NotEqual(t, hashF1, hashF2, "feature must have two distinct commits")

	return brDB, hashM1, hashF1, hashF2
}

func TestResetVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	// Randomised db name so parallel test runs don't collide.
	dbName := fmt.Sprintf("resetvrfy%d", rand.Int64N(1_000_000))

	hashC1, _ := resetVerifySetup(t, env, dbName)

	t.Run("Scenario1_SoftReset_WorkingSetPreserved", func(t *testing.T) {
		items := env.Client.Database(dbName).Collection("tasks")

		// Insert _id:3 into the working set (do NOT commit).
		_, err := items.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)},
			{Key: "v", Value: int32(3)},
		})
		require.NoError(t, err)

		// Soft reset to hashC1 (no `hard` parameter  -- defaults to false).
		var raw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: hashC1},
		}).Decode(&raw))

		// Response: { hash: hashC1, ok: 1 }
		assert.Equal(t, hashC1, raw["commitId"], "soft reset response hash must equal hashC1")
		assert.EqualValues(t, 1, raw["ok"])

		// Working set is preserved  -- diff shows _id:2 and _id:3 as added
		// (HEAD=C1 has only _id:1).
		var diffRaw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltDiff", Value: int32(1)},
		}).Decode(&diffRaw))

		dr := decodeDiffResult(t, diffRaw)
		cd := findCollDiff(dr, "tasks")
		require.NotNil(t, cd, "expected diff for 'tasks' collection after soft reset")

		addedIDs := make(map[any]bool)
		for _, a := range cd.Added {
			addedIDs[a["_id"]] = true
		}

		assert.True(t, addedIDs[int32(2)], "_id:2 must appear as added (was in C2 but HEAD=C1)")
		assert.True(t, addedIDs[int32(3)], "_id:3 must appear as added (uncommitted insert preserved)")
		assert.Len(t, addedIDs, 2, "exactly _id:2 and _id:3 should be in the added set")
		assert.Empty(t, cd.Removed, "no documents should be removed")
		assert.Empty(t, cd.Modified, "no documents should be modified")
	})

	t.Run("Scenario2_HardReset_WorkingSetDiscarded", func(t *testing.T) {
		items := env.Client.Database(dbName).Collection("tasks")

		// Commit the working set from Scenario 1 to create a new snapshot (C3).
		dumboDBCommit(t, env, dbName, "snapshot", "alice <alice@acme.com>")

		// Add _id:4 to the working set (uncommitted).
		_, err := items.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(4)},
			{Key: "v", Value: int32(4)},
		})
		require.NoError(t, err)

		// Hard reset to hashC1.
		var raw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: hashC1},
			{Key: "hard", Value: true},
		}).Decode(&raw))

		// Response: { hash: hashC1, ok: 1 }
		assert.Equal(t, hashC1, raw["commitId"], "hard reset response hash must equal hashC1")
		assert.EqualValues(t, 1, raw["ok"])

		// Working set matches target  -- diff is empty.
		var diffRaw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltDiff", Value: int32(1)},
		}).Decode(&diffRaw))

		dr := decodeDiffResult(t, diffRaw)
		assert.Empty(t, dr.Collections,
			"after hard reset working set must match HEAD  -- diff must be empty")

		// Only _id:1 should be visible.
		n, err := items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), n,
			"after hard reset to C1: exactly 1 document must be visible")
	})

	t.Run("Scenario3_SoftReset_UndoCommit", func(t *testing.T) {
		items := env.Client.Database(dbName).Collection("tasks")

		// After Scenario 2: HEAD=C1, working set is clean.
		// Insert _id:2 again and commit (C4).
		_, err := items.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(2)},
			{Key: "v", Value: int32(2)},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "re-add-two", "bob <bob@widgets.io>")

		// Soft reset to hashC1  -- "undoes" the re-add-two commit.
		var raw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: hashC1},
		}).Decode(&raw))

		assert.Equal(t, hashC1, raw["commitId"], "soft undo-commit response hash must equal hashC1")

		// _id:2 was in the last commit but the working tree was not changed.
		// dumboDBDiff must show _id:2 as added (HEAD=C1 doesn't have it).
		var diffRaw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltDiff", Value: int32(1)},
		}).Decode(&diffRaw))

		dr := decodeDiffResult(t, diffRaw)
		cd := findCollDiff(dr, "tasks")
		require.NotNil(t, cd, "expected diff for 'tasks' collection after soft undo-commit")
		require.Len(t, cd.Added, 1, "expected exactly _id:2 as the only added doc")
		assert.Equal(t, int32(2), cd.Added[0]["_id"], "added doc must be _id:2")
		assert.Empty(t, cd.Removed, "no removed documents")
		assert.Empty(t, cd.Modified, "no modified documents")
	})

	t.Run("Scenario4_HardResetToHEAD_DiscardsUncommitted", func(t *testing.T) {
		items := env.Client.Database(dbName).Collection("tasks")

		// After Scenario 3: HEAD=C1, working set is clean (only _id:1).
		// Capture the current HEAD hash so we can assert it is returned.
		var logRaw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
		}).Decode(&logRaw))
		commits, ok := logRaw["commits"].(bson.A)
		require.True(t, ok && len(commits) > 0, "doltLog must return at least one commit")
		headEntry, ok := commits[0].(bson.M)
		require.True(t, ok, "first log entry must be a document")
		headHash, ok := headEntry["commitId"].(string)
		require.True(t, ok && headHash != "", "first log entry must have a non-empty commitId")

		// Insert _id:5 into the working set (uncommitted).
		_, err := items.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(5)},
			{Key: "v", Value: int32(5)},
		})
		require.NoError(t, err)

		var preDiffRaw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltDiff", Value: int32(1)},
		}).Decode(&preDiffRaw))
		preCD := findCollDiff(decodeDiffResult(t, preDiffRaw), "tasks")
		require.NotNil(t, preCD, "expected a 'tasks' diff for the uncommitted _id:5 insert")
		preAdded := make(map[any]bool)
		for _, a := range preCD.Added {
			preAdded[a["_id"]] = true
		}
		assert.True(t, preAdded[int32(5)], "_id:5 must show as added before the reset")

		// Hard reset with no `to`  -- should default to HEAD.
		var raw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "hard", Value: true},
		}).Decode(&raw))

		assert.Equal(t, headHash, raw["commitId"], "reset-to-HEAD response must equal current HEAD hash")
		assert.EqualValues(t, 1, raw["ok"])

		// Uncommitted insert of _id:5 must be discarded  -- diff is empty.
		var diffRaw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltDiff", Value: int32(1)},
		}).Decode(&diffRaw))

		dr := decodeDiffResult(t, diffRaw)
		assert.Empty(t, dr.Collections,
			"after hard reset to HEAD: working set must match HEAD  -- diff must be empty")

		// Only _id:1 should be visible (the HEAD=C1 state).
		n, err := items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), n,
			"after hard reset to HEAD: exactly 1 document must be visible")

		_, err = items.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(6)},
			{Key: "v", Value: int32(6)},
		})
		require.NoError(t, err)

		var softRaw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
		}).Decode(&softRaw))
		assert.Equal(t, headHash, softRaw["commitId"], "bare soft reset must return current HEAD hash")
		assert.EqualValues(t, 1, softRaw["ok"], "bare soft reset must report ok:1")

		var softDiffRaw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltDiff", Value: int32(1)},
		}).Decode(&softDiffRaw))
		softCD := findCollDiff(decodeDiffResult(t, softDiffRaw), "tasks")
		require.NotNil(t, softCD, "soft reset to HEAD must preserve the uncommitted _id:6 diff")
		require.Len(t, softCD.Added, 1, "soft reset to HEAD must leave _id:6 as the only addition")
		assert.Equal(t, int32(6), softCD.Added[0]["_id"], "the preserved addition must be _id:6")
		n, err = items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), n, "soft reset to HEAD must not remove the uncommitted _id:6")

		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "hard", Value: true},
		}).Decode(&softRaw))
		n, err = items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), n, "hard reset to HEAD must discard the uncommitted _id:6")
	})

	t.Run("Scenario5_RelativeRootish", func(t *testing.T) {
		items := env.Client.Database(dbName).Collection("tasks")

		// After Scenario 4: HEAD=C1 (only _id:1).
		// Build two more commits so we can walk back N steps:
		//   C5: add _id:6 -> HEAD~2 from final state
		//   C6: add _id:7 -> HEAD~1 from final state
		//   C7: add _id:8 -> HEAD       (after this commit)
		_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(6)}, {Key: "v", Value: int32(6)}})
		require.NoError(t, err)
		hashC5 := dumboDBCommit(t, env, dbName, "add-six", "alice <alice@acme.com>")

		_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(7)}, {Key: "v", Value: int32(7)}})
		require.NoError(t, err)
		hashC6 := dumboDBCommit(t, env, dbName, "add-seven", "alice <alice@acme.com>")

		_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(8)}, {Key: "v", Value: int32(8)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "add-eight", "alice <alice@acme.com>")

		// Hard reset to HEAD~1  -- should land on C6 (only _id:1, _id:6, _id:7).
		var raw bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: "HEAD~1"},
			{Key: "hard", Value: true},
		}).Decode(&raw))
		assert.Equal(t, hashC6, raw["commitId"], "HEAD~1 must resolve to C6")
		assert.EqualValues(t, 1, raw["ok"])

		n, err := items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), n, "after reset to HEAD~1: 3 docs visible (_id:1,6,7)")

		// Hard reset to main~1  -- should land on C5 (only _id:1, _id:6).
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: "main~1"},
			{Key: "hard", Value: true},
		}).Decode(&raw))
		assert.Equal(t, hashC5, raw["commitId"], "main~1 must resolve to C5")

		n, err = items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), n, "after reset to main~1: 2 docs visible (_id:1,6)")

		// Bare HEAD must equal current branch HEAD (no movement).
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: "HEAD"},
		}).Decode(&raw))
		assert.Equal(t, hashC5, raw["commitId"], "to:'HEAD' must return current HEAD hash")

		// Bare branch name must resolve to that branch's HEAD.
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: "main"},
		}).Decode(&raw))
		assert.Equal(t, hashC5, raw["commitId"], "to:'main' must return main's HEAD")
	})

	t.Run("Scenario6_SoftResetOnNonMainBranch", func(t *testing.T) {
		brDB, hashM1, hashF1, _ := resetBranchVerifySetup(t, env)
		featDB := env.Client.Database(brDB + "@feature")

		var raw bson.M
		require.NoError(t, featDB.RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: hashF1},
		}).Decode(&raw))
		assert.Equal(t, hashF1, raw["commitId"], "soft reset on feature must resolve to F1")
		assert.EqualValues(t, 1, raw["ok"])

		assert.Equal(t, hashF1, branchHead(t, env, brDB+"@feature"),
			"feature HEAD must be F1 after soft reset on feature")
		fn, err := featDB.Collection("tasks").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), fn,
			"soft reset preserves the feature working tree (_id:1,2,3)")

		var diffRaw bson.M
		require.NoError(t, featDB.RunCommand(ctx, bson.D{
			{Key: "doltDiff", Value: int32(1)},
		}).Decode(&diffRaw))
		cd := findCollDiff(decodeDiffResult(t, diffRaw), "tasks")
		require.NotNil(t, cd, "expected a 'tasks' diff on feature after soft reset")
		require.Len(t, cd.Added, 1, "exactly _id:3 should be uncommitted after soft reset to F1")
		assert.Equal(t, int32(3), cd.Added[0]["_id"], "the uncommitted addition must be _id:3")

		assert.Equal(t, hashM1, branchHead(t, env, brDB),
			"main HEAD must be unchanged by a soft reset on feature")
		mn, err := env.Client.Database(brDB).Collection("tasks").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), mn,
			"main working set must be untouched by a soft reset on feature")
	})

	t.Run("Scenario7_HardResetOnNonMainBranch", func(t *testing.T) {
		brDB, hashM1, hashF1, _ := resetBranchVerifySetup(t, env)
		featDB := env.Client.Database(brDB + "@feature")

		var raw bson.M
		require.NoError(t, featDB.RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: hashF1},
			{Key: "hard", Value: true},
		}).Decode(&raw))
		assert.Equal(t, hashF1, raw["commitId"], "hard reset on feature must resolve to F1")
		assert.EqualValues(t, 1, raw["ok"])

		assert.Equal(t, hashF1, branchHead(t, env, brDB+"@feature"),
			"feature HEAD must be F1 after hard reset on feature")
		fn, err := featDB.Collection("tasks").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), fn,
			"feature working set must show 2 docs (_id:1,2) after hard reset to F1")

		assert.Equal(t, hashM1, branchHead(t, env, brDB),
			"main HEAD must be unchanged by a hard reset on feature")
		mn, err := env.Client.Database(brDB).Collection("tasks").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), mn,
			"main working set must be untouched (only _id:1) by a hard reset on feature")
	})

	t.Run("Scenario8_HardResetToCommitOnOtherBranch", func(t *testing.T) {
		brDB, _, hashF1, hashF2 := resetBranchVerifySetup(t, env)
		mainDB := env.Client.Database(brDB)
		tasks := mainDB.Collection("tasks")

		var raw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: hashF1},
			{Key: "hard", Value: true},
		}).Decode(&raw))
		assert.Equal(t, hashF1, raw["commitId"], "reset must resolve to the feature commit F1")
		assert.EqualValues(t, 1, raw["ok"])

		assert.Equal(t, hashF1, branchHead(t, env, brDB), "main HEAD must be F1 after hard reset")
		mn, err := tasks.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), mn, "main content must follow F1 (2 docs: _id:1,2)")
		got2, err := tasks.CountDocuments(ctx, bson.D{{Key: "_id", Value: int32(2)}})
		require.NoError(t, err)
		assert.Equal(t, int64(1), got2, "_id:2 (feature content) must now be present on main")
		got3, err := tasks.CountDocuments(ctx, bson.D{{Key: "_id", Value: int32(3)}})
		require.NoError(t, err)
		assert.Equal(t, int64(0), got3, "_id:3 (only on F2) must not be present after reset to F1")

		var diffRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltDiff", Value: int32(1)}}).Decode(&diffRaw))
		assert.Empty(t, decodeDiffResult(t, diffRaw).Collections,
			"after hard reset the working set must match HEAD (empty diff)")

		assert.Equal(t, hashF2, branchHead(t, env, brDB+"@feature"),
			"feature HEAD must be unchanged by a reset on main")
		fn, err := env.Client.Database(brDB+"@feature").Collection("tasks").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), fn, "feature content must be unchanged (3 docs)")
	})

	t.Run("Scenario9_SoftResetToCommitOnOtherBranch", func(t *testing.T) {
		brDB, _, hashF1, hashF2 := resetBranchVerifySetup(t, env)
		mainDB := env.Client.Database(brDB)
		tasks := mainDB.Collection("tasks")

		var raw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: hashF1},
		}).Decode(&raw))
		assert.Equal(t, hashF1, raw["commitId"], "soft reset must resolve to the feature commit F1")
		assert.EqualValues(t, 1, raw["ok"])
		assert.Equal(t, hashF1, branchHead(t, env, brDB), "main HEAD must be F1 after soft reset")

		mn, err := tasks.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), mn, "soft reset preserves main's working tree (_id:1 only)")

		var diffRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltDiff", Value: int32(1)}}).Decode(&diffRaw))
		cd := findCollDiff(decodeDiffResult(t, diffRaw), "tasks")
		require.NotNil(t, cd, "expected a 'tasks' diff after soft reset across branches")
		require.Len(t, cd.Removed, 1, "exactly _id:2 must be missing from the working tree vs the new HEAD")
		assert.Equal(t, int32(2), cd.Removed[0]["_id"], "the removed doc must be _id:2")
		assert.Empty(t, cd.Added, "no docs should be added")
		assert.Empty(t, cd.Modified, "no docs should be modified")

		assert.Equal(t, hashF2, branchHead(t, env, brDB+"@feature"),
			"feature HEAD must be unchanged by a reset on main")
		fn, err := env.Client.Database(brDB+"@feature").Collection("tasks").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), fn, "feature content must be unchanged (3 docs)")
	})

	t.Run("Scenario10_ResetRejectedOnReadOnlyConnection", func(t *testing.T) {
		brDB, hashM1, hashF1, hashF2 := resetBranchVerifySetup(t, env)

		readOnlyConns := []string{brDB + "@" + hashF1, brDB + "@feature~1"}
		for _, conn := range readOnlyConns {
			db := env.Client.Database(conn)

			softErr := db.RunCommand(ctx, bson.D{
				{Key: "doltReset", Value: int32(1)},
				{Key: "to", Value: hashF1},
			}).Err()
			assertWriteBlockedOperationFailed(t, softErr, "soft doltReset on "+conn)

			hardErr := db.RunCommand(ctx, bson.D{
				{Key: "doltReset", Value: int32(1)},
				{Key: "to", Value: hashF1},
				{Key: "hard", Value: true},
			}).Err()
			assertWriteBlockedOperationFailed(t, hardErr, "hard doltReset on "+conn)
		}

		assert.Equal(t, hashM1, branchHead(t, env, brDB), "main HEAD must be unchanged")
		assert.Equal(t, hashF2, branchHead(t, env, brDB+"@feature"), "feature HEAD must be unchanged")
	})
}
