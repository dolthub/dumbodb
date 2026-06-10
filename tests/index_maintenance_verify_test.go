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

// TestIndexMaintenanceVerify is the automated analog of
// docs/verify/index-maintenance.md. Each top-level subtest corresponds
// to one scenario in that document; they run sequentially against one
// database so side effects carry forward exactly as the manual steps
// do.

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

// idxvCount runs the count command (the indexed-count fast path).
func idxvCount(t *testing.T, db *mongo.Database, coll string, query bson.D) int32 {
	t.Helper()
	var res bson.M
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "count", Value: coll},
		{Key: "query", Value: query},
	}).Decode(&res))
	switch n := res["n"].(type) {
	case int32:
		return n
	case int64:
		return int32(n)
	}
	t.Fatalf("count response missing n: %v", res)
	return 0
}

func idxvFindIDs(t *testing.T, db *mongo.Database, coll string, filter bson.D) []int32 {
	t.Helper()
	ctx := context.Background()
	cur, err := db.Collection(coll).Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "_id", Value: int32(1)}}))
	require.NoError(t, err)
	var docs []bson.M
	require.NoError(t, cur.All(ctx, &docs))
	ids := []int32{}
	for _, d := range docs {
		ids = append(ids, d["_id"].(int32))
	}
	return ids
}

func TestIndexMaintenanceVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("idxmntvrfy%d", rand.Int64N(1_000_000))
	db := env.client.Database(dbName)
	require.NoError(t, db.Drop(ctx))

	// Setup block from the doc.
	items := db.Collection("items")
	_, err := items.InsertMany(ctx, []interface{}{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "name", Value: "alpha"}, {Key: "city", Value: "NYC"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "name", Value: "bravo"}, {Key: "city", Value: "LA"}},
		bson.D{{Key: "_id", Value: int32(3)}, {Key: "name", Value: "charlie"}, {Key: "city", Value: "NYC"}},
	})
	require.NoError(t, err)
	_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "name", Value: int32(1)}}, Options: options.Index().SetName("by_name"),
	})
	require.NoError(t, err)
	_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "city", Value: int32(1)}}, Options: options.Index().SetName("by_city"),
	})
	require.NoError(t, err)

	t.Run("Scenario1_UpdateReindexesChangedField", func(t *testing.T) {
		_, err := items.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: "zulu"}}}})
		require.NoError(t, err)

		assert.Equal(t, []int32{1}, idxvFindIDs(t, db, "items", bson.D{{Key: "name", Value: "zulu"}}))
		assert.Empty(t, idxvFindIDs(t, db, "items", bson.D{{Key: "name", Value: "alpha"}}))
		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "name", Value: "zulu"}}))
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "name", Value: "alpha"}}))

		wp := idxvWinningPlan(t, db, "items", bson.D{{Key: "name", Value: "zulu"}})
		assert.Equal(t, "by_name", idxvIxscanName(wp), "re-indexed value must be served by the index: %v", wp)
	})

	t.Run("Scenario2_UpdateManyReindexesEveryDoc", func(t *testing.T) {
		_, err := items.UpdateMany(ctx,
			bson.D{{Key: "city", Value: "NYC"}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "city", Value: "SF"}}}})
		require.NoError(t, err)

		assert.EqualValues(t, 2, idxvCount(t, db, "items", bson.D{{Key: "city", Value: "SF"}}))
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "city", Value: "NYC"}}))
	})

	t.Run("Scenario3_DeleteRemovesIndexEntries", func(t *testing.T) {
		_, err := items.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
		require.NoError(t, err)
		assert.Empty(t, idxvFindIDs(t, db, "items", bson.D{{Key: "name", Value: "bravo"}}))
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "name", Value: "bravo"}}))

		_, err = items.DeleteMany(ctx, bson.D{{Key: "city", Value: "SF"}})
		require.NoError(t, err)
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "city", Value: "SF"}}))
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{}))
	})

	t.Run("Scenario4_MultikeyUpdateAdjustsPerElement", func(t *testing.T) {
		_, err := items.InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(10)}, {Key: "tags", Value: bson.A{"red", "green", "blue"}}},
			bson.D{{Key: "_id", Value: int32(11)}, {Key: "tags", Value: bson.A{"red"}}},
		})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "tags", Value: int32(1)}}, Options: options.Index().SetName("by_tags"),
		})
		require.NoError(t, err)

		_, err = items.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(10)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "tags.1", Value: "yellow"}}}})
		require.NoError(t, err)

		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "tags", Value: "yellow"}}))
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "tags", Value: "green"}}))
		assert.EqualValues(t, 2, idxvCount(t, db, "items", bson.D{{Key: "tags", Value: "red"}}))

		rangeFilter := bson.D{{Key: "tags", Value: bson.D{{Key: "$gt", Value: "a"}}}}
		assert.Equal(t, []int32{10, 11}, idxvFindIDs(t, db, "items", rangeFilter),
			"multi-element doc must be returned exactly once")
		assert.EqualValues(t, 2, idxvCount(t, db, "items", rangeFilter))

		eqPlan := idxvWinningPlan(t, db, "items", bson.D{{Key: "tags", Value: "yellow"}})
		assert.Equal(t, "by_tags", idxvIxscanName(eqPlan), "equality lookup must use by_tags: %v", eqPlan)
		rangePlan := idxvWinningPlan(t, db, "items", rangeFilter)
		assert.Equal(t, "by_tags", idxvIxscanName(rangePlan), "range lookup must use by_tags: %v", rangePlan)
	})

	t.Run("Scenario5_SparseIndexTracksFieldPresence", func(t *testing.T) {
		_, err := items.InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(20)}, {Key: "phone", Value: "555-0100"}},
			bson.D{{Key: "_id", Value: int32(21)}},
		})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "phone", Value: int32(1)}},
			Options: options.Index().SetName("by_phone").SetSparse(true),
		})
		require.NoError(t, err)

		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "phone", Value: "555-0100"}}))

		_, err = items.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(21)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "phone", Value: "555-0200"}}}})
		require.NoError(t, err)
		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "phone", Value: "555-0200"}}))

		_, err = items.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(20)}},
			bson.D{{Key: "$unset", Value: bson.D{{Key: "phone", Value: ""}}}})
		require.NoError(t, err)
		assert.Empty(t, idxvFindIDs(t, db, "items", bson.D{{Key: "phone", Value: "555-0100"}}))
		assert.EqualValues(t, 0, idxvCount(t, db, "items", bson.D{{Key: "phone", Value: "555-0100"}}))

		wp := idxvWinningPlan(t, db, "items", bson.D{{Key: "phone", Value: "555-0200"}})
		assert.Equal(t, "by_phone", idxvIxscanName(wp), "sparse index must serve equality lookups: %v", wp)
	})

	t.Run("Scenario6_PartialIndexTracksFilter", func(t *testing.T) {
		_, err := items.InsertMany(ctx, []interface{}{
			bson.D{{Key: "_id", Value: int32(30)}, {Key: "sku", Value: "A-1"}, {Key: "status", Value: "active"}},
			bson.D{{Key: "_id", Value: int32(31)}, {Key: "sku", Value: "B-2"}, {Key: "status", Value: "inactive"}},
		})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "sku", Value: int32(1)}},
			Options: options.Index().SetName("by_sku_partial").
				SetPartialFilterExpression(bson.D{{Key: "status", Value: "active"}}),
		})
		require.NoError(t, err)

		_, err = items.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(30)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "inactive"}}}})
		require.NoError(t, err)
		_, err = items.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(31)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "active"}}}})
		require.NoError(t, err)

		assert.Equal(t, []int32{30}, idxvFindIDs(t, db, "items", bson.D{{Key: "sku", Value: "A-1"}}))
		assert.Equal(t, []int32{31}, idxvFindIDs(t, db, "items", bson.D{{Key: "sku", Value: "B-2"}}))
		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "sku", Value: "A-1"}}))
		assert.EqualValues(t, 1, idxvCount(t, db, "items", bson.D{{Key: "sku", Value: "B-2"}}))

		// The planner must decline the partial index for a general sku
		// query (it omits inactive docs); the plan is a collection scan.
		wp := idxvWinningPlan(t, db, "items", bson.D{{Key: "sku", Value: "A-1"}})
		assert.Equal(t, "COLLSCAN", wp["stage"], "partial index must not be chosen for an uncovered query: %v", wp)
	})
}
