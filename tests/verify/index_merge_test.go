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

// TestIndexMergeVerify is the automated analog of
// docs/verify/index-merge.md. Each subtest corresponds to one scenario
// in that document and uses its own database, exactly as the manual
// steps do.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func idxvBranch(t *testing.T, env *dumboDBTestEnv, dbName, branch string) {
	t.Helper()
	require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(context.Background(), bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: branch},
	}).Err())
}

func idxvMerge(t *testing.T, env *dumboDBTestEnv, dbName, branch string) bson.M {
	t.Helper()
	return runCommandRaw(t, env.Client.Database(dbName+"@main"), bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "merge_in", Value: branch},
	})
}

// idxvWinningPlan returns the winningPlan document for a find filter.
func idxvWinningPlan(t *testing.T, db *mongo.Database, coll string, filter bson.D) bson.M {
	t.Helper()
	var res bson.M
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "explain", Value: bson.D{
			{Key: "find", Value: coll},
			{Key: "filter", Value: filter},
		}},
		{Key: "verbosity", Value: "queryPlanner"},
	}).Decode(&res))
	qp, ok := res["queryPlanner"].(bson.M)
	require.True(t, ok, "explain response missing queryPlanner: %v", res)
	wp, ok := qp["winningPlan"].(bson.M)
	require.True(t, ok, "queryPlanner missing winningPlan: %v", qp)
	return wp
}

func idxvIxscanName(wp bson.M) string {
	for cur := wp; cur != nil; {
		if cur["stage"] == "IXSCAN" {
			name, _ := cur["indexName"].(string)
			return name
		}
		next, _ := cur["inputStage"].(bson.M)
		cur = next
	}
	return ""
}

func TestIndexMergeVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	suffix := rand.Int64N(1_000_000)

	t.Run("Scenario1_MergedIndexCoversBothBranches", func(t *testing.T) {
		dbName := fmt.Sprintf("idxmrg1v%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		items := db.Collection("items")

		_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "base"}})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "name", Value: int32(1)}}, Options: options.Index().SetName("by_name"),
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "seed + index", "alice <alice@acme.com>")
		idxvBranch(t, env, dbName, "feature")

		_, err = items.InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(10)}, {Key: "name", Value: "alpha"}},
			bson.D{{Key: "_id", Value: int32(11)}, {Key: "name", Value: "bravo"}},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: a-side", "alice <alice@acme.com>")

		feat := env.Client.Database(dbName + "@feature")
		_, err = feat.Collection("items").InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(20)}, {Key: "name", Value: "november"}},
			bson.D{{Key: "_id", Value: int32(21)}, {Key: "name", Value: "oscar"}},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature: n-side", "bob <bob@widgets.io>")

		raw := idxvMerge(t, env, dbName, "feature")
		assert.EqualValues(t, 1, raw["ok"], "merge must complete cleanly: %v", raw)

		assert.Equal(t, []int32{20}, idxvFindIDs(t, db, "items", bson.D{{Key: "name", Value: "november"}}))
		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "name", Value: "november"}}))
		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "name", Value: "alpha"}}))

		wp := idxvWinningPlan(t, db, "items", bson.D{{Key: "name", Value: "november"}})
		assert.Equal(t, "by_name", idxvIxscanName(wp), "lookup must be served by the index: %v", wp)
	})

	t.Run("Scenario2_OneSidedIndexCoversMergedDocs", func(t *testing.T) {
		dbName := fmt.Sprintf("idxmrg2v%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		items := db.Collection("items")

		_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "city", Value: "base"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "seed, no index", "alice <alice@acme.com>")
		idxvBranch(t, env, dbName, "feature")

		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "city", Value: int32(1)}}, Options: options.Index().SetName("by_city"),
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: create by_city", "alice <alice@acme.com>")

		feat := env.Client.Database(dbName + "@feature")
		_, err = feat.Collection("items").InsertOne(ctx,
			bson.D{{Key: "_id", Value: int32(20)}, {Key: "city", Value: "november"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature: november", "bob <bob@widgets.io>")

		raw := idxvMerge(t, env, dbName, "feature")
		assert.EqualValues(t, 1, raw["ok"], "merge must complete cleanly: %v", raw)

		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "city", Value: "november"}}))
		wp := idxvWinningPlan(t, db, "items", bson.D{{Key: "city", Value: "november"}})
		assert.Equal(t, "by_city", idxvIxscanName(wp), "lookup must be served by the index: %v", wp)
	})

	// Scenario 2b: a base document (present in the index) deleted on the feature
	// branch must drop out of the merged index.
	t.Run("Scenario2b_DeleteOnBranchDropsFromMergedIndex", func(t *testing.T) {
		dbName := fmt.Sprintf("idxmrg2bv%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		items := db.Collection("items")

		// Base: two docs, NO index yet.
		_, err := items.InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "city", Value: "base"}},
			bson.D{{Key: "_id", Value: int32(2)}, {Key: "city", Value: "paris"}},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "seed (no index)", "alice <alice@acme.com>")
		idxvBranch(t, env, dbName, "feature")

		// The index is created only on main; feature never sees it. This makes
		// the branches diverge so the merge below is a real 3-way merge.
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "city", Value: int32(1)}}, Options: options.Index().SetName("by_city"),
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: create by_city", "alice <alice@acme.com>")

		// Feature (without the index) deletes the base doc carrying "paris".
		feat := env.Client.Database(dbName + "@feature")
		_, err = feat.Collection("items").DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature: delete doc 2", "bob <bob@widgets.io>")

		raw := idxvMerge(t, env, dbName, "feature")
		assert.EqualValues(t, 1, raw["ok"], "merge must complete cleanly: %v", raw)
		assert.NotEqual(t, "fast-forward", raw["message"], "must be a real 3-way merge, not a fast-forward")
		assert.NotEqual(t, "already up-to-date", raw["message"])

		// The deleted doc's indexed value is gone from the merged index.
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "city", Value: "paris"}}), "paris must be gone after merge")
		assert.Empty(t, idxvFindIDs(t, db, "items", bson.D{{Key: "city", Value: "paris"}}))
		// The untouched base doc is still indexed.
		assert.Equal(t, []int32{1}, idxvFindIDs(t, db, "items", bson.D{{Key: "city", Value: "base"}}))
		wp := idxvWinningPlan(t, db, "items", bson.D{{Key: "city", Value: "paris"}})
		assert.Equal(t, "by_city", idxvIxscanName(wp), "lookup still served by the index: %v", wp)
	})

	// Scenario 2c: changing an indexed field of a base document on the feature
	// branch must update the merged index (new value in, old value out).
	t.Run("Scenario2c_UpdateIndexedFieldOnBranchUpdatesMergedIndex", func(t *testing.T) {
		dbName := fmt.Sprintf("idxmrg2cv%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		items := db.Collection("items")

		// Base: two docs, NO index yet.
		_, err := items.InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "city", Value: "base"}},
			bson.D{{Key: "_id", Value: int32(2)}, {Key: "city", Value: "paris"}},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "seed (no index)", "alice <alice@acme.com>")
		idxvBranch(t, env, dbName, "feature")

		// The index is created only on main; feature never sees it, so the
		// merge below is a real 3-way merge.
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "city", Value: int32(1)}}, Options: options.Index().SetName("by_city"),
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: create by_city", "alice <alice@acme.com>")

		// Feature (without the index) changes the indexed field: paris -> london.
		feat := env.Client.Database(dbName + "@feature")
		_, err = feat.Collection("items").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(2)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "city", Value: "london"}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature: doc 2 -> london", "bob <bob@widgets.io>")

		raw := idxvMerge(t, env, dbName, "feature")
		assert.EqualValues(t, 1, raw["ok"], "merge must complete cleanly: %v", raw)
		assert.NotEqual(t, "fast-forward", raw["message"], "must be a real 3-way merge, not a fast-forward")
		assert.NotEqual(t, "already up-to-date", raw["message"])

		// New value indexed; old value gone.
		assert.Equal(t, []int32{2}, idxvFindIDs(t, db, "items", bson.D{{Key: "city", Value: "london"}}), "london (new value) must be indexed")
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "city", Value: "paris"}}), "paris (old value) must be gone")
		assert.Equal(t, []int32{1}, idxvFindIDs(t, db, "items", bson.D{{Key: "city", Value: "base"}}), "untouched base doc still indexed")
		wp := idxvWinningPlan(t, db, "items", bson.D{{Key: "city", Value: "london"}})
		assert.Equal(t, "by_city", idxvIxscanName(wp), "lookup served by the index: %v", wp)
	})

	t.Run("Scenario3_DropWins", func(t *testing.T) {
		dbName := fmt.Sprintf("idxmrg3v%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		items := db.Collection("items")

		_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "base"}})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "name", Value: int32(1)}}, Options: options.Index().SetName("by_name"),
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "seed + index", "alice <alice@acme.com>")
		idxvBranch(t, env, dbName, "feature")

		require.NoError(t, items.Indexes().DropOne(ctx, "by_name"))
		dumboDBCommit(t, env, dbName, "main: drop by_name", "alice <alice@acme.com>")

		feat := env.Client.Database(dbName + "@feature")
		_, err = feat.Collection("items").InsertOne(ctx,
			bson.D{{Key: "_id", Value: int32(20)}, {Key: "name", Value: "november"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature: november", "bob <bob@widgets.io>")

		raw := idxvMerge(t, env, dbName, "feature")
		assert.EqualValues(t, 1, raw["ok"], "merge must complete cleanly: %v", raw)

		cur, err := items.Indexes().List(ctx)
		require.NoError(t, err)
		var idxRows []bson.M
		require.NoError(t, cur.All(ctx, &idxRows))
		names := []string{}
		for _, r := range idxRows {
			names = append(names, r["name"].(string))
		}
		assert.Equal(t, []string{"_id_"}, names, "by_name must stay dropped")

		assert.Equal(t, []int32{20}, idxvFindIDs(t, db, "items", bson.D{{Key: "name", Value: "november"}}))
		wp := idxvWinningPlan(t, db, "items", bson.D{{Key: "name", Value: "november"}})
		assert.Equal(t, "COLLSCAN", wp["stage"], "no index left; plan must scan: %v", wp)
	})

	t.Run("Scenario4_UniqueCollisionIsConflict", func(t *testing.T) {
		dbName := fmt.Sprintf("idxmrg4v%d", suffix)
		db := env.Client.Database(dbName)
		mainDB := env.Client.Database(dbName + "@main")
		require.NoError(t, db.Drop(ctx))
		items := db.Collection("items")

		_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "sku", Value: "SEED"}})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "sku", Value: int32(1)}},
			Options: options.Index().SetName("by_sku").SetUnique(true),
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "seed + unique index", "alice <alice@acme.com>")
		idxvBranch(t, env, dbName, "feature")

		_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(10)}, {Key: "sku", Value: "S-1"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: doc 10 sku S-1", "alice <alice@acme.com>")

		feat := env.Client.Database(dbName + "@feature")
		_, err = feat.Collection("items").InsertOne(ctx,
			bson.D{{Key: "_id", Value: int32(20)}, {Key: "sku", Value: "S-1"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature: doc 20 sku S-1", "bob <bob@widgets.io>")

		raw := idxvMerge(t, env, dbName, "feature")
		assert.EqualValues(t, 0, raw["ok"], "colliding merge must surface conflicts: %v", raw)

		// Ours owns the key during the in-progress merge.
		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "sku", Value: "S-1"}}))
		assert.Equal(t, []int32{10}, idxvFindIDs(t, db, "items", bson.D{{Key: "sku", Value: "S-1"}}))

		var rc bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltConflicts", Value: int32(1)}}).Decode(&rc))
		colls := rc["collections"].(bson.A)
		require.Len(t, colls, 1)
		conflicts := colls[0].(bson.M)["conflicts"].(bson.A)
		require.Len(t, conflicts, 1)
		entry := conflicts[0].(bson.M)
		assert.Equal(t, "uniqueKeyCollision", entry["type"])
		reason := entry["reason"].(bson.M)
		assert.Equal(t, "by_sku", reason["index"], "reason names the offending index")
		assert.Equal(t, "S-1", reason["key"].(bson.M)["sku"], "reason carries the colliding key")
		assert.Equal(t, `unique index "by_sku": branch 'main' (ours) and branch 'feature' (theirs) both have sku = "S-1"`,
			reason["message"], "message names the index and both branches")

		// Both contenders are present with their own _ids: ours is the
		// surviving doc 10 (main), theirs is the evicted doc 20 (feature).
		ours := entry["ours"].(bson.M)
		assert.EqualValues(t, 10, ours["_id"])
		assert.Equal(t, "S-1", ours["doc"].(bson.M)["sku"])
		assert.Equal(t, "added", ours["diffType"])
		theirs := entry["theirs"].(bson.M)
		assert.EqualValues(t, 20, theirs["_id"])
		assert.Equal(t, "S-1", theirs["doc"].(bson.M)["sku"])
		assert.Equal(t, "added", theirs["diffType"])
		assert.Nil(t, entry["base"], "no document held the key in the ancestor")
		conflictID := entry["conflictId"].(string)

		// "theirs" is a key-ownership swap: evict ours's doc 10, install
		// theirs's doc 20 under the key. It succeeds (no duplicate error).
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "items"},
			{Key: "conflictId", Value: conflictID},
			{Key: "resolution", Value: "theirs"},
		}).Err(), "theirs swap must not be rejected")

		var contRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		}).Decode(&contRaw))
		assert.EqualValues(t, 1, contRaw["ok"])

		// Theirs now owns the key; ours's doc 10 is gone.
		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "sku", Value: "S-1"}}))
		assert.Equal(t, []int32{20}, idxvFindIDs(t, db, "items", bson.D{{Key: "sku", Value: "S-1"}}))
	})

	t.Run("Scenario5_ResolutionReindexesChosenDoc", func(t *testing.T) {
		dbName := fmt.Sprintf("idxmrg5v%d", suffix)
		db := env.Client.Database(dbName)
		mainDB := env.Client.Database(dbName + "@main")
		require.NoError(t, db.Drop(ctx))
		items := db.Collection("items")

		_, err := items.InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "alpha"}},
			bson.D{{Key: "_id", Value: int32(2)}, {Key: "name", Value: "bravo"}},
		})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "name", Value: int32(1)}}, Options: options.Index().SetName("by_name"),
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "seed + index", "alice <alice@acme.com>")
		idxvBranch(t, env, dbName, "feature")

		for i, v := range map[int32]string{1: "ours-1", 2: "ours-2"} {
			_, err = items.UpdateOne(ctx,
				bson.D{{Key: "_id", Value: i}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: v}}}})
			require.NoError(t, err)
		}
		dumboDBCommit(t, env, dbName, "main: ours", "alice <alice@acme.com>")

		feat := env.Client.Database(dbName + "@feature")
		for i, v := range map[int32]string{1: "theirs-1", 2: "theirs-2"} {
			_, err = feat.Collection("items").UpdateOne(ctx,
				bson.D{{Key: "_id", Value: i}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: v}}}})
			require.NoError(t, err)
		}
		dumboDBCommit(t, env, dbName+"@feature", "feature: theirs", "bob <bob@widgets.io>")

		raw := idxvMerge(t, env, dbName, "feature")
		assert.EqualValues(t, 0, raw["ok"], "divergent edits must surface conflicts: %v", raw)

		var rc bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltConflicts", Value: int32(1)}}).Decode(&rc))
		conflicts := rc["collections"].(bson.A)[0].(bson.M)["conflicts"].(bson.A)
		require.Len(t, conflicts, 2)
		byDocID := map[int32]string{}
		for _, c := range conflicts {
			e := c.(bson.M)
			assert.Equal(t, "documentEdit", e["type"])
			assert.Equal(t, "bothModified", e["reason"].(bson.M)["code"])
			// A documentEdit shares one _id across base/ours/theirs.
			side := e["ours"]
			if side == nil {
				side = e["theirs"]
			}
			byDocID[side.(bson.M)["_id"].(int32)] = e["conflictId"].(string)
		}

		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "items"},
			{Key: "conflictId", Value: byDocID[1]},
			{Key: "resolution", Value: "theirs"},
		}).Err())
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "items"},
			{Key: "conflictId", Value: byDocID[2]},
			{Key: "resolution", Value: "custom"},
			{Key: "value", Value: bson.D{{Key: "_id", Value: int32(2)}, {Key: "name", Value: "custom-2"}}},
		}).Err())

		var contRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		}).Decode(&contRaw))
		assert.EqualValues(t, 1, contRaw["ok"])

		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "name", Value: "theirs-1"}}))
		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "name", Value: "custom-2"}}))
		for _, stale := range []string{"alpha", "bravo", "ours-1", "ours-2", "theirs-2"} {
			assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "name", Value: stale}}),
				"stale value %q must not be findable", stale)
		}
	})

	t.Run("Scenario6_TwoIndexesTwoCollisions", func(t *testing.T) {
		dbName := fmt.Sprintf("idxmrg6v%d", suffix)
		db := env.Client.Database(dbName)
		mainDB := env.Client.Database(dbName + "@main")
		require.NoError(t, db.Drop(ctx))
		items := db.Collection("items")

		_, err := items.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(1)}, {Key: "sku", Value: "SEED"}, {Key: "code", Value: "SEED"}})
		require.NoError(t, err)
		for _, m := range []mongo.IndexModel{
			{Keys: bson.D{{Key: "sku", Value: int32(1)}}, Options: options.Index().SetName("by_sku").SetUnique(true)},
			{Keys: bson.D{{Key: "code", Value: int32(1)}}, Options: options.Index().SetName("by_code").SetUnique(true)},
		} {
			_, err = items.Indexes().CreateOne(ctx, m)
			require.NoError(t, err)
		}
		dumboDBCommit(t, env, dbName, "seed + two unique indexes", "alice <alice@acme.com>")
		idxvBranch(t, env, dbName, "feature")

		// One pair collides on by_sku, a separate pair on by_code.
		_, err = items.InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(10)}, {Key: "sku", Value: "S-1"}, {Key: "code", Value: "K-10"}},
			bson.D{{Key: "_id", Value: int32(11)}, {Key: "sku", Value: "S-11"}, {Key: "code", Value: "C-1"}},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: docs 10,11", "alice <alice@acme.com>")

		feat := env.Client.Database(dbName + "@feature")
		_, err = feat.Collection("items").InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(20)}, {Key: "sku", Value: "S-1"}, {Key: "code", Value: "K-20"}},
			bson.D{{Key: "_id", Value: int32(21)}, {Key: "sku", Value: "S-21"}, {Key: "code", Value: "C-1"}},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature: docs 20,21", "bob <bob@widgets.io>")

		raw := idxvMerge(t, env, dbName, "feature")
		assert.EqualValues(t, 0, raw["ok"], "two collisions must surface conflicts: %v", raw)

		var rc bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltConflicts", Value: int32(1)}}).Decode(&rc))
		conflicts := rc["collections"].(bson.A)[0].(bson.M)["conflicts"].(bson.A)
		require.Len(t, conflicts, 2, "one conflict per colliding index")

		byIndex := map[string]bson.M{}
		for _, c := range conflicts {
			e := c.(bson.M)
			assert.Equal(t, "uniqueKeyCollision", e["type"])
			byIndex[e["reason"].(bson.M)["index"].(string)] = e
		}
		require.Contains(t, byIndex, "by_sku")
		require.Contains(t, byIndex, "by_code")
		assert.EqualValues(t, 10, byIndex["by_sku"]["ours"].(bson.M)["_id"])
		assert.EqualValues(t, 20, byIndex["by_sku"]["theirs"].(bson.M)["_id"])
		assert.EqualValues(t, 11, byIndex["by_code"]["ours"].(bson.M)["_id"])
		assert.EqualValues(t, 21, byIndex["by_code"]["theirs"].(bson.M)["_id"])

		// Each collision resolves independently.
		for _, e := range byIndex {
			require.NoError(t, mainDB.RunCommand(ctx, bson.D{
				{Key: "doltResolveConflict", Value: int32(1)},
				{Key: "collection", Value: "items"},
				{Key: "conflictId", Value: e["conflictId"].(string)},
				{Key: "resolution", Value: "ours"},
			}).Err())
		}

		var contRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		}).Decode(&contRaw))
		assert.EqualValues(t, 1, contRaw["ok"])

		assert.Equal(t, []int32{10}, idxvFindIDs(t, db, "items", bson.D{{Key: "sku", Value: "S-1"}}))
		assert.Equal(t, []int32{11}, idxvFindIDs(t, db, "items", bson.D{{Key: "code", Value: "C-1"}}))
	})

	// Scenario 7: a cherry-pick that applies a doc colliding on a unique key is
	// a uniqueKeyCollision conflict, just like a merge. ours = the branch, theirs
	// = the cherry-picked commit.
	t.Run("Scenario7_CherryPickCollisionIsConflict", func(t *testing.T) {
		dbName := fmt.Sprintf("idxmrg7v%d", suffix)
		db := env.Client.Database(dbName)
		mainDB := env.Client.Database(dbName + "@main")
		require.NoError(t, db.Drop(ctx))
		items := db.Collection("items")

		_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "sku", Value: "SEED"}})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "sku", Value: int32(1)}}, Options: options.Index().SetName("by_sku").SetUnique(true)})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "seed + unique index", "alice <alice@acme.com>")
		idxvBranch(t, env, dbName, "feature")

		_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(10)}, {Key: "sku", Value: "S-1"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: doc 10 sku S-1", "alice <alice@acme.com>")

		feat := env.Client.Database(dbName + "@feature")
		_, err = feat.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(20)}, {Key: "sku", Value: "S-1"}})
		require.NoError(t, err)
		pickHash := dumboDBCommit(t, env, dbName+"@feature", "feature: doc 20 sku S-1", "bob <bob@widgets.io>")

		raw := runCommandRaw(t, mainDB, bson.D{{Key: "dumboCherryPick", Value: int32(1)}, {Key: "commit", Value: pickHash}})
		assert.EqualValues(t, 0, raw["ok"], "colliding cherry-pick must surface conflicts: %v", raw)

		var rc bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltConflicts", Value: int32(1)}}).Decode(&rc))
		entry := rc["collections"].(bson.A)[0].(bson.M)["conflicts"].(bson.A)[0].(bson.M)
		assert.Equal(t, "uniqueKeyCollision", entry["type"])
		reason := entry["reason"].(bson.M)
		assert.Equal(t, "by_sku", reason["index"])
		assert.Equal(t, "S-1", reason["key"].(bson.M)["sku"])
		assert.Equal(t, fmt.Sprintf(`unique index "by_sku": branch 'main' (ours) and commit '%s' (theirs) both have sku = "S-1"`, pickHash),
			reason["message"], "names the branch and the cherry-picked commit")
		assert.EqualValues(t, 10, entry["ours"].(bson.M)["_id"], "ours = main's surviving doc 10")
		assert.EqualValues(t, 20, entry["theirs"].(bson.M)["_id"], "theirs = the cherry-picked commit's doc 20")
		assert.Equal(t, "added", entry["ours"].(bson.M)["diffType"])
		assert.Equal(t, "added", entry["theirs"].(bson.M)["diffType"])

		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)}, {Key: "collection", Value: "items"},
			{Key: "conflictId", Value: entry["conflictId"].(string)}, {Key: "resolution", Value: "ours"}}).Err())
		var contRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltCherryPick", Value: int32(1)}, {Key: "continue", Value: int32(1)}}).Decode(&contRaw))
		assert.EqualValues(t, 1, contRaw["ok"])
		assert.Equal(t, []int32{10}, idxvFindIDs(t, db, "items", bson.D{{Key: "sku", Value: "S-1"}}), "ours (doc 10) keeps the key")
	})

	// Scenario 8: a rebase that replays a commit colliding on a unique key is a
	// uniqueKeyCollision. ours = the replayed commit, theirs = the onto branch.
	t.Run("Scenario8_RebaseCollisionIsConflict", func(t *testing.T) {
		dbName := fmt.Sprintf("idxmrg8v%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		items := db.Collection("items")

		_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "sku", Value: "SEED"}})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "sku", Value: int32(1)}}, Options: options.Index().SetName("by_sku").SetUnique(true)})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "seed + unique index", "alice <alice@acme.com>")
		idxvBranch(t, env, dbName, "feature")

		feat := env.Client.Database(dbName + "@feature")
		_, err = feat.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(20)}, {Key: "sku", Value: "S-1"}})
		require.NoError(t, err)
		pickHash := dumboDBCommit(t, env, dbName+"@feature", "feature: doc 20 sku S-1", "bob <bob@widgets.io>")

		_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(10)}, {Key: "sku", Value: "S-1"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: doc 10 sku S-1", "alice <alice@acme.com>")

		featureDB := env.Client.Database(dbName + "@feature")
		raw := runCommandRaw(t, featureDB, bson.D{{Key: "dumboRebase", Value: int32(1)}, {Key: "onto", Value: "main"}})
		assert.EqualValues(t, 0, raw["ok"], "colliding rebase must surface conflicts: %v", raw)

		var rc bson.M
		require.NoError(t, featureDB.RunCommand(ctx, bson.D{{Key: "doltConflicts", Value: int32(1)}}).Decode(&rc))
		entry := rc["collections"].(bson.A)[0].(bson.M)["conflicts"].(bson.A)[0].(bson.M)
		assert.Equal(t, "uniqueKeyCollision", entry["type"])
		reason := entry["reason"].(bson.M)
		assert.Equal(t, "by_sku", reason["index"])
		assert.Equal(t, "S-1", reason["key"].(bson.M)["sku"])
		assert.Equal(t, fmt.Sprintf(`unique index "by_sku": commit '%s' (ours) and branch 'main' (theirs) both have sku = "S-1"`, pickHash),
			reason["message"], "names the replayed commit and the onto branch")
		assert.EqualValues(t, 20, entry["ours"].(bson.M)["_id"], "ours = the replayed commit's doc 20")
		assert.EqualValues(t, 10, entry["theirs"].(bson.M)["_id"], "theirs = onto/main's doc 10")
		assert.Equal(t, "added", entry["ours"].(bson.M)["diffType"])
		assert.Equal(t, "added", entry["theirs"].(bson.M)["diffType"])

		require.NoError(t, featureDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)}, {Key: "collection", Value: "items"},
			{Key: "conflictId", Value: entry["conflictId"].(string)}, {Key: "resolution", Value: "ours"}}).Err())
		var contRaw bson.M
		require.NoError(t, featureDB.RunCommand(ctx, bson.D{{Key: "dumboRebase", Value: int32(1)}, {Key: "continue", Value: int32(1)}}).Decode(&contRaw))
		assert.EqualValues(t, 1, contRaw["ok"])
	})

	// Scenario 9: reverting a delete re-adds a document whose unique key is now
	// held by a different one: a uniqueKeyCollision. ours = the branch, theirs =
	// the reverted commit.
	t.Run("Scenario9_RevertCollisionIsConflict", func(t *testing.T) {
		dbName := fmt.Sprintf("idxmrg9v%d", suffix)
		db := env.Client.Database(dbName)
		mainDB := env.Client.Database(dbName + "@main")
		require.NoError(t, db.Drop(ctx))
		items := db.Collection("items")

		_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "sku", Value: "SEED"}})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "sku", Value: int32(1)}}, Options: options.Index().SetName("by_sku").SetUnique(true)})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "seed + unique index", "alice <alice@acme.com>")

		_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(10)}, {Key: "sku", Value: "S-1"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "add doc 10 sku S-1", "alice <alice@acme.com>")

		_, err = items.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(10)}})
		require.NoError(t, err)
		delHash := dumboDBCommit(t, env, dbName, "delete doc 10", "alice <alice@acme.com>")

		_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(20)}, {Key: "sku", Value: "S-1"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "add doc 20 sku S-1", "alice <alice@acme.com>")

		// Reverting the delete re-adds doc 10 (sku S-1), colliding with doc 20.
		raw := runCommandRaw(t, mainDB, bson.D{{Key: "dumboRevert", Value: int32(1)}, {Key: "commit", Value: delHash}})
		assert.EqualValues(t, 0, raw["ok"], "colliding revert must surface conflicts: %v", raw)

		var rc bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltConflicts", Value: int32(1)}}).Decode(&rc))
		entry := rc["collections"].(bson.A)[0].(bson.M)["conflicts"].(bson.A)[0].(bson.M)
		assert.Equal(t, "uniqueKeyCollision", entry["type"])
		reason := entry["reason"].(bson.M)
		assert.Equal(t, "by_sku", reason["index"])
		assert.Equal(t, "S-1", reason["key"].(bson.M)["sku"])
		assert.Equal(t, fmt.Sprintf(`unique index "by_sku": branch 'main' (ours) and commit '%s' (theirs) both have sku = "S-1"`, delHash),
			reason["message"], "names the branch and the reverted commit")
		assert.EqualValues(t, 20, entry["ours"].(bson.M)["_id"], "ours = main's doc 20")
		assert.EqualValues(t, 10, entry["theirs"].(bson.M)["_id"], "theirs = the reverted commit re-adding doc 10")
		assert.Equal(t, "added", entry["ours"].(bson.M)["diffType"])
		assert.Equal(t, "added", entry["theirs"].(bson.M)["diffType"])

		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)}, {Key: "collection", Value: "items"},
			{Key: "conflictId", Value: entry["conflictId"].(string)}, {Key: "resolution", Value: "ours"}}).Err())
		var contRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "dumboRevert", Value: int32(1)}, {Key: "continue", Value: int32(1)}}).Decode(&contRaw))
		assert.EqualValues(t, 1, contRaw["ok"])
		assert.Equal(t, []int32{20}, idxvFindIDs(t, db, "items", bson.D{{Key: "sku", Value: "S-1"}}), "ours (doc 20) keeps the key")
	})
}
