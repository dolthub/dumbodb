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

// TestIndexMergeVerify is the automated analog of
// docs/verify/index-merge.md. Each subtest corresponds to one scenario
// in that document and uses its own database, exactly as the manual
// steps do.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func idxvBranch(t *testing.T, env *dumboDBTestEnv, dbName, branch string) {
	t.Helper()
	require.NoError(t, env.client.Database(dbName+"@main").RunCommand(context.Background(), bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: branch},
	}).Err())
}

func idxvMerge(t *testing.T, env *dumboDBTestEnv, dbName, branch string) bson.M {
	t.Helper()
	return runCommandRaw(t, env.client.Database(dbName+"@main"), bson.D{
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
		db := env.client.Database(dbName)
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

		feat := env.client.Database(dbName + "@feature")
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
		db := env.client.Database(dbName)
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

		feat := env.client.Database(dbName + "@feature")
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

	t.Run("Scenario3_DropWins", func(t *testing.T) {
		dbName := fmt.Sprintf("idxmrg3v%d", suffix)
		db := env.client.Database(dbName)
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

		feat := env.client.Database(dbName + "@feature")
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
		db := env.client.Database(dbName)
		mainDB := env.client.Database(dbName + "@main")
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

		feat := env.client.Database(dbName + "@feature")
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
		assert.EqualValues(t, 20, entry["_id"])
		assert.Nil(t, entry["ours"], "evicted doc must not exist on ours")
		require.NotNil(t, entry["theirs"])
		assert.Equal(t, "S-1", entry["theirs"].(bson.M)["sku"])
		conflictID := entry["conflictId"].(string)

		// "theirs" would re-create the collision: rejected.
		theirsErr := mainDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "items"},
			{Key: "conflictId", Value: conflictID},
			{Key: "resolution", Value: "theirs"},
		}).Err()
		require.Error(t, theirsErr, "colliding theirs resolution must be rejected")
		assert.True(t, strings.Contains(strings.ToLower(theirsErr.Error()), "duplicate"),
			"rejection must be a duplicate-key error: %v", theirsErr)

		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: "items"},
			{Key: "conflictId", Value: conflictID},
			{Key: "resolution", Value: "ours"},
		}).Err())

		var contRaw bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltMerge", Value: int32(1)},
			{Key: "continue", Value: int32(1)},
		}).Decode(&contRaw))
		assert.EqualValues(t, 1, contRaw["ok"])

		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "sku", Value: "S-1"}}))
		assert.Equal(t, []int32{10}, idxvFindIDs(t, db, "items", bson.D{{Key: "sku", Value: "S-1"}}))
	})

	t.Run("Scenario5_ResolutionReindexesChosenDoc", func(t *testing.T) {
		dbName := fmt.Sprintf("idxmrg5v%d", suffix)
		db := env.client.Database(dbName)
		mainDB := env.client.Database(dbName + "@main")
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

		feat := env.client.Database(dbName + "@feature")
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
			byDocID[e["_id"].(int32)] = e["conflictId"].(string)
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
}
