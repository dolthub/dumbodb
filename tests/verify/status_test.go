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
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestStatusVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	// Randomised db name so parallel test runs don't collide.
	dbName := fmt.Sprintf("statusvrfy%d", rand.Int64N(1_000_000))

	// Setup: baseline commit with one document in "items".
	require.NoError(t, env.Client.Database(dbName).Drop(ctx))
	_, err := env.Client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "label", Value: "alpha"},
		{Key: "score", Value: int32(10)},
	})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "baseline", "alice <alice@acme.com>")

	// -------------------------------------------------------------------------
	// Scenario 1: Status on clean repo  -- empty tables
	// -------------------------------------------------------------------------
	t.Run("Scenario1_CleanRepo", func(t *testing.T) {
		sr := runStatus(t, env, dbName)
		assert.Empty(t, sr.Tables, "expected empty collections after commit with no new changes")
		assert.NotEmpty(t, sr.CommitID, "commitId must be present when workspace is clean")

		var rawStatus bson.M
		err := env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&rawStatus)
		require.NoError(t, err)
		assert.Nil(t, rawStatus["mergeState"], "mergeState must be absent on clean status")
		assert.Equal(t, false, rawStatus["dirty"], "clean repo must not be dirty")
		assert.Equal(t, false, rawStatus["readonly"], "branch connection is not readonly")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: Status after insert  -- new collection appears as "added" with counts
	// -------------------------------------------------------------------------
	t.Run("Scenario2_AfterInsert", func(t *testing.T) {
		// First verify that dirty workspace does NOT include commitId.
		_, err := env.Client.Database(dbName).Collection("newcoll").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(1)},
			{Key: "v", Value: "new"},
		})
		require.NoError(t, err)

		sr := runStatus(t, env, dbName)

		assert.Empty(t, sr.CommitID, "commitId must be absent when workspace is dirty")

		// dirty flag must be true after uncommitted insert.
		var rawDirty bson.M
		require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&rawDirty))
		assert.Equal(t, true, rawDirty["dirty"], "workspace with uncommitted changes must be dirty")

		newcoll := findTableStatus(sr, "newcoll")
		require.NotNil(t, newcoll, "expected 'newcoll' in collections")
		assert.Equal(t, "added", newcoll.Status)
		assert.Equal(t, 1, newcoll.Added, "newcoll: 1 inserted doc")
		assert.Equal(t, 0, newcoll.Modified)
		assert.Equal(t, 0, newcoll.Deleted)

		assert.Nil(t, findTableStatus(sr, "items"), "'items' must not appear (unchanged)")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: Status after update  -- modified collection appears as "modified"
	// -------------------------------------------------------------------------
	t.Run("Scenario3_AfterUpdate", func(t *testing.T) {
		dumboDBCommit(t, env, dbName, "add newcoll", "alice <alice@acme.com>")

		_, err := env.Client.Database(dbName).Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "score", Value: int32(99)}}}},
		)
		require.NoError(t, err)

		sr := runStatus(t, env, dbName)

		items := findTableStatus(sr, "items")
		require.NotNil(t, items, "expected 'items' in collections")
		assert.Equal(t, "modified", items.Status)
		assert.Equal(t, 0, items.Added)
		assert.Equal(t, 1, items.Modified, "items: 1 modified doc")
		assert.Equal(t, 0, items.Deleted)

		assert.Nil(t, findTableStatus(sr, "newcoll"), "'newcoll' must not appear (committed, unchanged)")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: Status after delete  -- removed collection appears as "deleted"
	// -------------------------------------------------------------------------
	t.Run("Scenario4_AfterDelete", func(t *testing.T) {
		dumboDBCommit(t, env, dbName, "modify items", "alice <alice@acme.com>")

		require.NoError(t, env.Client.Database(dbName).Collection("items").Drop(ctx))

		sr := runStatus(t, env, dbName)

		items := findTableStatus(sr, "items")
		require.NotNil(t, items, "expected 'items' in collections")
		assert.Equal(t, "deleted", items.Status)
		assert.Equal(t, 0, items.Added)
		assert.Equal(t, 0, items.Modified)
		assert.Equal(t, 1, items.Deleted, "items: 1 deleted doc (the only one)")

		assert.Nil(t, findTableStatus(sr, "newcoll"), "'newcoll' must not appear (unchanged)")
	})

	// -------------------------------------------------------------------------
	// Scenario 5: Status after commit  -- clean again
	// -------------------------------------------------------------------------
	t.Run("Scenario5_AfterCommit", func(t *testing.T) {
		dumboDBCommit(t, env, dbName, "delete items", "alice <alice@acme.com>")

		sr := runStatus(t, env, dbName)
		assert.Empty(t, sr.Tables, "expected empty collections after committing all changes")
	})

	// -------------------------------------------------------------------------
	// Scenario 6: Multiple collections changed simultaneously
	// -------------------------------------------------------------------------
	t.Run("Scenario6_MultipleCollections", func(t *testing.T) {
		// Reset to a known baseline: commit three starter collections, then mutate all three.
		// - "orders":   committed baseline -> will be modified (3 adds, 1 mod, 2 deletes)
		// - "users":    not in HEAD -> will be "added" (5 inserts)
		// - "archive":  committed baseline -> will be "deleted" (Drop)
		for i := int32(1); i <= 3; i++ {
			_, err := env.Client.Database(dbName).Collection("orders").InsertOne(ctx, bson.D{
				{Key: "_id", Value: i},
				{Key: "total", Value: i * int32(10)},
			})
			require.NoError(t, err)
		}
		_, err := env.Client.Database(dbName).Collection("archive").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(1)},
			{Key: "note", Value: "old"},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "baseline for multi-collection", "alice <alice@acme.com>")

		// Now modify all three collections in one working set.

		// orders: 3 new docs, 1 modified doc, 2 deleted docs.
		for i := int32(4); i <= 6; i++ {
			_, err := env.Client.Database(dbName).Collection("orders").InsertOne(ctx, bson.D{
				{Key: "_id", Value: i},
				{Key: "total", Value: i * int32(10)},
			})
			require.NoError(t, err)
		}
		_, err = env.Client.Database(dbName).Collection("orders").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "total", Value: int32(999)}}}},
		)
		require.NoError(t, err)
		_, err = env.Client.Database(dbName).Collection("orders").DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
		require.NoError(t, err)
		_, err = env.Client.Database(dbName).Collection("orders").DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(3)}})
		require.NoError(t, err)

		// users: new collection with 5 inserts.
		for i := int32(1); i <= 5; i++ {
			_, err := env.Client.Database(dbName).Collection("users").InsertOne(ctx, bson.D{
				{Key: "_id", Value: i},
				{Key: "name", Value: fmt.Sprintf("user%d", i)},
			})
			require.NoError(t, err)
		}

		// archive: drop the whole collection.
		require.NoError(t, env.Client.Database(dbName).Collection("archive").Drop(ctx))

		sr := runStatus(t, env, dbName)

		orders := findTableStatus(sr, "orders")
		require.NotNil(t, orders, "expected 'orders' in collections")
		assert.Equal(t, "modified", orders.Status)
		assert.Equal(t, 3, orders.Added, "orders: 3 inserted docs")
		assert.Equal(t, 1, orders.Modified, "orders: 1 modified doc")
		assert.Equal(t, 2, orders.Deleted, "orders: 2 deleted docs")

		users := findTableStatus(sr, "users")
		require.NotNil(t, users, "expected 'users' in collections")
		assert.Equal(t, "added", users.Status)
		assert.Equal(t, 5, users.Added, "users: 5 inserted docs")
		assert.Equal(t, 0, users.Modified)
		assert.Equal(t, 0, users.Deleted)

		archive := findTableStatus(sr, "archive")
		require.NotNil(t, archive, "expected 'archive' in collections")
		assert.Equal(t, "deleted", archive.Status)
		assert.Equal(t, 0, archive.Added)
		assert.Equal(t, 0, archive.Modified)
		assert.Equal(t, 1, archive.Deleted, "archive: 1 doc removed (the only one)")
	})

	// -------------------------------------------------------------------------
	// Scenario 7: Multi-field modification  -- several fields changed in one doc
	// -------------------------------------------------------------------------
	t.Run("Scenario7_MultiFieldDocModification", func(t *testing.T) {
		// Commit Scenario 6's state to get a fresh baseline.
		dumboDBCommit(t, env, dbName, "checkpoint before multi-field", "alice <alice@acme.com>")

		// Modify one doc in "users" with changes spanning several fields:
		// - set a new field   (age)
		// - modify an existing field (name)
		// - unset an existing field  (later doltStatus still counts the doc as 1 modified)
		_, err := env.Client.Database(dbName).Collection("users").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{
				{Key: "$set", Value: bson.D{
					{Key: "name", Value: "user1-renamed"},
					{Key: "age", Value: int32(30)},
				}},
			},
		)
		require.NoError(t, err)

		sr := runStatus(t, env, dbName)

		users := findTableStatus(sr, "users")
		require.NotNil(t, users, "expected 'users' in collections")
		assert.Equal(t, "modified", users.Status)
		assert.Equal(t, 0, users.Added, "multi-field changes are still one modified doc, not an add")
		assert.Equal(t, 1, users.Modified, "users: exactly 1 doc modified regardless of field count")
		assert.Equal(t, 0, users.Deleted)
	})

	// -------------------------------------------------------------------------
	// Scenario 8: Read-only rootish  -- returns readonly:true, dirty:false
	// -------------------------------------------------------------------------
	t.Run("Scenario8_ReadOnlyRootish", func(t *testing.T) {
		// Commit current state so we have a stable commit hash to point at.
		commitHash := dumboDBCommitAllowEmpty(t, env, dbName, "commit for read-only status", "alice <alice@acme.com>")

		// Commit hash rootish.
		var snapStatus bson.M
		require.NoError(t, env.Client.Database(dbName+"@"+commitHash).RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&snapStatus))

		assert.EqualValues(t, 1, snapStatus["ok"])
		assert.Equal(t, true, snapStatus["readonly"], "commit hash rootish must be readonly")
		assert.Equal(t, false, snapStatus["dirty"], "read-only rootish is never dirty")
		assert.Equal(t, commitHash, snapStatus["commitId"], "commitId must be the resolved hash")

		// Ancestor expression.
		var parentStatus bson.M
		require.NoError(t, env.Client.Database(dbName+"@main~1").RunCommand(ctx, bson.D{
			{Key: "doltStatus", Value: int32(1)},
		}).Decode(&parentStatus))

		assert.EqualValues(t, 1, parentStatus["ok"])
		assert.Equal(t, true, parentStatus["readonly"])
		assert.Equal(t, false, parentStatus["dirty"])
		assert.NotEmpty(t, parentStatus["commitId"], "commitId must be present for ancestor rootish")
	})
}
