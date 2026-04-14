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

// TestResetVerify is the automated analog of docs/verify/reset.md.
//
// Each top-level subtest corresponds to one scenario in that document.
// The setup reproduces the manual setup block exactly:
//
//   - Commit C1 (hashC1): items = [ {_id:1, v:1} ]
//   - Commit C2 (hashC2, HEAD): items = [ {_id:1, v:1}, {_id:2, v:2} ]
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
	"go.mongodb.org/mongo-driver/bson"
)

// resetVerifySetup mirrors the Setup section of docs/verify/reset.md.
// Returns hashC1 and hashC2.
func resetVerifySetup(t *testing.T, env *dumboDBTestEnv, dbName string) (hashC1, hashC2 string) {
	t.Helper()

	ctx := context.Background()
	items := env.client.Database(dbName).Collection("items")

	require.NoError(t, env.client.Database(dbName).Drop(ctx))

	_, err := items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "v", Value: int32(1)},
	})
	require.NoError(t, err)
	hashC1 = dumboDBCommit(t, env, dbName, "initial", "alice <alice@dumbodb>")

	_, err = items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "v", Value: int32(2)},
	})
	require.NoError(t, err)
	hashC2 = dumboDBCommit(t, env, dbName, "add-two", "alice <alice@dumbodb>")

	return hashC1, hashC2
}

func TestResetVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	// Randomised db name so parallel test runs don't collide.
	dbName := fmt.Sprintf("resetvrfy%d", rand.Int64N(1_000_000))

	hashC1, _ := resetVerifySetup(t, env, dbName)

	// -------------------------------------------------------------------------
	// Scenario 1: Soft reset (default) — HEAD moves back, working set preserved
	// -------------------------------------------------------------------------
	t.Run("Scenario1_SoftReset_WorkingSetPreserved", func(t *testing.T) {
		items := env.client.Database(dbName).Collection("items")

		// Insert _id:3 into the working set (do NOT commit).
		_, err := items.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)},
			{Key: "v", Value: int32(3)},
		})
		require.NoError(t, err)

		// Soft reset to hashC1 (no `hard` parameter — defaults to false).
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: hashC1},
		}).Decode(&raw))

		// Response: { hash: hashC1, ok: 1 }
		assert.Equal(t, hashC1, raw["commitId"], "soft reset response hash must equal hashC1")
		assert.EqualValues(t, 1, raw["ok"])

		// Working set is preserved — diff shows _id:2 and _id:3 as added
		// (HEAD=C1 has only _id:1).
		var diffRaw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltDiff", Value: int32(1)},
		}).Decode(&diffRaw))

		dr := decodeDiffResult(t, diffRaw)
		cd := findCollDiff(dr, "items")
		require.NotNil(t, cd, "expected diff for 'items' collection after soft reset")

		addedIDs := make(map[any]bool)
		for _, a := range cd.Added {
			addedIDs[a["_id"]] = true
		}

		assert.True(t, addedIDs[int32(2)], "_id:2 must appear as added (was in C2 but HEAD=C1)")
		assert.True(t, addedIDs[int32(3)], "_id:3 must appear as added (uncommitted insert preserved)")
		assert.Empty(t, cd.Removed, "no documents should be removed")
		assert.Empty(t, cd.Modified, "no documents should be modified")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: Hard reset — HEAD and working set both reset to target
	// -------------------------------------------------------------------------
	t.Run("Scenario2_HardReset_WorkingSetDiscarded", func(t *testing.T) {
		items := env.client.Database(dbName).Collection("items")

		// Commit the working set from Scenario 1 to create a new snapshot (C3).
		dumboDBCommit(t, env, dbName, "snapshot", "alice <alice@dumbodb>")

		// Add _id:4 to the working set (uncommitted).
		_, err := items.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(4)},
			{Key: "v", Value: int32(4)},
		})
		require.NoError(t, err)

		// Hard reset to hashC1.
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: hashC1},
			{Key: "hard", Value: true},
		}).Decode(&raw))

		// Response: { hash: hashC1, ok: 1 }
		assert.Equal(t, hashC1, raw["commitId"], "hard reset response hash must equal hashC1")
		assert.EqualValues(t, 1, raw["ok"])

		// Working set matches target — diff is empty.
		var diffRaw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltDiff", Value: int32(1)},
		}).Decode(&diffRaw))

		dr := decodeDiffResult(t, diffRaw)
		assert.Empty(t, dr.Collections,
			"after hard reset working set must match HEAD — diff must be empty")

		// Only _id:1 should be visible.
		n, err := items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), n,
			"after hard reset to C1: exactly 1 document must be visible")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: Soft reset undoes a committed change — it becomes uncommitted
	// -------------------------------------------------------------------------
	t.Run("Scenario3_SoftReset_UndoCommit", func(t *testing.T) {
		items := env.client.Database(dbName).Collection("items")

		// After Scenario 2: HEAD=C1, working set is clean.
		// Insert _id:2 again and commit (C4).
		_, err := items.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(2)},
			{Key: "v", Value: int32(2)},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "re-add-two", "alice <alice@dumbodb>")

		// Soft reset to hashC1 — "undoes" the re-add-two commit.
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "to", Value: hashC1},
		}).Decode(&raw))

		assert.Equal(t, hashC1, raw["commitId"], "soft undo-commit response hash must equal hashC1")

		// _id:2 was in the last commit but the working tree was not changed.
		// dumboDBDiff must show _id:2 as added (HEAD=C1 doesn't have it).
		var diffRaw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltDiff", Value: int32(1)},
		}).Decode(&diffRaw))

		dr := decodeDiffResult(t, diffRaw)
		cd := findCollDiff(dr, "items")
		require.NotNil(t, cd, "expected diff for 'items' collection after soft undo-commit")
		require.Len(t, cd.Added, 1, "expected exactly _id:2 as the only added doc")
		assert.Equal(t, int32(2), cd.Added[0]["_id"], "added doc must be _id:2")
		assert.Empty(t, cd.Removed, "no removed documents")
		assert.Empty(t, cd.Modified, "no modified documents")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: Hard reset to HEAD (no `to`) — discards uncommitted changes
	// -------------------------------------------------------------------------
	t.Run("Scenario4_HardResetToHEAD_DiscardsUncommitted", func(t *testing.T) {
		items := env.client.Database(dbName).Collection("items")

		// After Scenario 3: HEAD=C1, working set is clean (only _id:1).
		// Capture the current HEAD hash so we can assert it is returned.
		var logRaw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
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

		// Hard reset with no `to` — should default to HEAD.
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltReset", Value: int32(1)},
			{Key: "hard", Value: true},
		}).Decode(&raw))

		assert.Equal(t, headHash, raw["commitId"], "reset-to-HEAD response must equal current HEAD hash")
		assert.EqualValues(t, 1, raw["ok"])

		// Uncommitted insert of _id:5 must be discarded — diff is empty.
		var diffRaw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltDiff", Value: int32(1)},
		}).Decode(&diffRaw))

		dr := decodeDiffResult(t, diffRaw)
		assert.Empty(t, dr.Collections,
			"after hard reset to HEAD: working set must match HEAD — diff must be empty")

		// Only _id:1 should be visible (the HEAD=C1 state).
		n, err := items.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), n,
			"after hard reset to HEAD: exactly 1 document must be visible")
	})
}
