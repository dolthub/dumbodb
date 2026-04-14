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

// TestStatusVerify is the automated analog of docs/verify/status.md.
//
// Each top-level subtest corresponds to one scenario in that document.
// The setup reproduces the manual setup block exactly:
//
//   - Baseline commit: items = [ {_id:1, label:"alpha", score:10} ]
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

// statusResult holds the decoded top-level response from a dumboDBStatus command.
type statusResult struct {
	Branch string
	Tables []tableStatusEntry
}

// tableStatusEntry holds one entry from the "tables" array of a dumboDBStatus response.
type tableStatusEntry struct {
	Name   string
	Status string
}

// decodeStatusResult parses the raw bson.M from a dumboDBStatus RunCommand into the
// typed helpers above, failing the test if the shape is unexpected.
func decodeStatusResult(t *testing.T, raw bson.M) statusResult {
	t.Helper()

	branch, _ := raw["branch"].(string)

	rawTables, ok := raw["collections"]
	require.True(t, ok, "doltStatus result missing 'collections' field")

	tablesArr, ok := rawTables.(bson.A)
	require.True(t, ok, "doltStatus 'collections' is not an array, got %T", rawTables)

	var out statusResult
	out.Branch = branch

	for _, tbl := range tablesArr {
		tm, ok := tbl.(bson.M)
		require.True(t, ok, "collections entry is not a document, got %T", tbl)

		entry := tableStatusEntry{
			Name:   fmt.Sprintf("%v", tm["name"]),
			Status: fmt.Sprintf("%v", tm["status"]),
		}
		out.Tables = append(out.Tables, entry)
	}

	return out
}

// findTableStatus returns the tableStatusEntry for the named collection, or nil.
func findTableStatus(sr statusResult, name string) *tableStatusEntry {
	for i := range sr.Tables {
		if sr.Tables[i].Name == name {
			return &sr.Tables[i]
		}
	}
	return nil
}

func TestStatusVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	// Randomised db name so parallel test runs don't collide.
	dbName := fmt.Sprintf("statusvrfy%d", rand.Int64N(1_000_000))

	// Setup: baseline commit with one document in "items".
	require.NoError(t, env.client.Database(dbName).Drop(ctx))
	_, err := env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "label", Value: "alpha"},
		{Key: "score", Value: int32(10)},
	})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "baseline", "alice <alice@dumbodb>")

	// -------------------------------------------------------------------------
	// Scenario 1: Status on clean repo — empty tables
	// -------------------------------------------------------------------------
	t.Run("Scenario1_CleanRepo", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&raw))

		sr := decodeStatusResult(t, raw)
		assert.Empty(t, sr.Tables, "expected empty tables after commit with no new changes")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: Status after insert — new collection appears as "added"
	// -------------------------------------------------------------------------
	t.Run("Scenario2_AfterInsert", func(t *testing.T) {
		_, err := env.client.Database(dbName).Collection("newcoll").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(1)},
			{Key: "v", Value: "new"},
		})
		require.NoError(t, err)

		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&raw))

		sr := decodeStatusResult(t, raw)

		// "newcoll" must appear as added.
		newcoll := findTableStatus(sr, "newcoll")
		require.NotNil(t, newcoll, "expected 'newcoll' in tables")
		assert.Equal(t, "added", newcoll.Status, "'newcoll' must have status 'added'")

		// "items" must not appear — it was not changed.
		assert.Nil(t, findTableStatus(sr, "items"), "'items' must not appear (unchanged)")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: Status after update — modified collection appears as "modified"
	// -------------------------------------------------------------------------
	t.Run("Scenario3_AfterUpdate", func(t *testing.T) {
		// Commit the "newcoll" addition first.
		dumboDBCommit(t, env, dbName, "add newcoll", "alice <alice@dumbodb>")

		// Modify an existing committed collection.
		_, err := env.client.Database(dbName).Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "score", Value: int32(99)}}}},
		)
		require.NoError(t, err)

		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&raw))

		sr := decodeStatusResult(t, raw)

		// "items" must appear as modified.
		items := findTableStatus(sr, "items")
		require.NotNil(t, items, "expected 'items' in tables")
		assert.Equal(t, "modified", items.Status, "'items' must have status 'modified'")

		// "newcoll" must not appear — it was committed and unchanged.
		assert.Nil(t, findTableStatus(sr, "newcoll"), "'newcoll' must not appear (committed, unchanged)")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: Status after delete — removed collection appears as "deleted"
	// -------------------------------------------------------------------------
	t.Run("Scenario4_AfterDelete", func(t *testing.T) {
		// Commit the items modification first.
		dumboDBCommit(t, env, dbName, "modify items", "alice <alice@dumbodb>")

		// Delete the entire "items" collection.
		require.NoError(t, env.client.Database(dbName).Collection("items").Drop(ctx))

		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&raw))

		sr := decodeStatusResult(t, raw)

		// "items" must appear as deleted.
		items := findTableStatus(sr, "items")
		require.NotNil(t, items, "expected 'items' in tables")
		assert.Equal(t, "deleted", items.Status, "'items' must have status 'deleted'")

		// "newcoll" must not appear (unchanged).
		assert.Nil(t, findTableStatus(sr, "newcoll"), "'newcoll' must not appear (unchanged)")
	})

	// -------------------------------------------------------------------------
	// Scenario 5: Status after commit — clean again
	// -------------------------------------------------------------------------
	t.Run("Scenario5_AfterCommit", func(t *testing.T) {
		// Commit the deletion.
		dumboDBCommit(t, env, dbName, "delete items", "alice <alice@dumbodb>")

		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&raw))

		sr := decodeStatusResult(t, raw)
		assert.Empty(t, sr.Tables, "expected empty tables after committing all changes")
	})
}
