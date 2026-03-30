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

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// TestAgg_Lookup_Basic verifies $lookup joins two collections by matching
// a local field to a foreign field. (DongoFull)
func TestAgg_Lookup_Basic(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("testdb_lookup_%d", rand.Int64())
	db := env.client.Database(dbName)
	t.Cleanup(func() { db.Drop(context.Background()) }) //nolint:errcheck

	orders := db.Collection("orders")
	items := db.Collection("items")

	_, err := orders.InsertMany(ctx, []interface{}{
		bson.D{{Key: "order_id", Value: int32(1)}, {Key: "item_id", Value: int32(10)}},
		bson.D{{Key: "order_id", Value: int32(2)}, {Key: "item_id", Value: int32(20)}},
		bson.D{{Key: "order_id", Value: int32(3)}, {Key: "item_id", Value: int32(99)}}, // no matching item
	})
	require.NoError(t, err)

	_, err = items.InsertMany(ctx, []interface{}{
		bson.D{{Key: "item_id", Value: int32(10)}, {Key: "name", Value: "apple"}},
		bson.D{{Key: "item_id", Value: int32(20)}, {Key: "name", Value: "banana"}},
	})
	require.NoError(t, err)

	pipeline := bson.A{
		bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "items"},
			{Key: "localField", Value: "item_id"},
			{Key: "foreignField", Value: "item_id"},
			{Key: "as", Value: "item_details"},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "order_id", Value: 1}}}},
	}

	cursor, err := orders.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// First order has a matching item.
	first := results[0].Map()
	itemDetails := first["item_details"].(bson.A)
	assert.Len(t, itemDetails, 1, "order 1 must have 1 matching item")
	assert.Equal(t, "apple", itemDetails[0].(bson.D).Map()["name"])

	// Third order has no matching item — item_details must be an empty array.
	third := results[2].Map()
	noMatch := third["item_details"].(bson.A)
	assert.Len(t, noMatch, 0, "order with no match must have empty item_details array")
}

// TestAgg_Lookup_Pipeline verifies $lookup with a sub-pipeline (correlated lookup). (DongoFull)
func TestAgg_Lookup_Pipeline(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("testdb_lookup_pipe_%d", rand.Int64())
	db := env.client.Database(dbName)
	t.Cleanup(func() { db.Drop(context.Background()) }) //nolint:errcheck

	customers := db.Collection("customers")
	orders := db.Collection("orders")

	_, err := customers.InsertMany(ctx, []interface{}{
		bson.D{{Key: "cust_id", Value: int32(1)}, {Key: "name", Value: "Alice"}},
	})
	require.NoError(t, err)

	_, err = orders.InsertMany(ctx, []interface{}{
		bson.D{{Key: "cust_id", Value: int32(1)}, {Key: "amount", Value: int32(50)}},
		bson.D{{Key: "cust_id", Value: int32(1)}, {Key: "amount", Value: int32(150)}},
	})
	require.NoError(t, err)

	// Pipeline lookup: join only orders with amount > 100.
	pipeline := bson.A{
		bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "orders"},
			{Key: "let", Value: bson.D{{Key: "cid", Value: "$cust_id"}}},
			{Key: "pipeline", Value: bson.A{
				bson.D{{Key: "$match", Value: bson.D{
					{Key: "$expr", Value: bson.D{
						{Key: "$and", Value: bson.A{
							bson.D{{Key: "$eq", Value: bson.A{"$cust_id", "$$cid"}}},
							bson.D{{Key: "$gt", Value: bson.A{"$amount", int32(100)}}},
						}},
					}},
				}}},
			}},
			{Key: "as", Value: "big_orders"},
		}}},
	}

	cursor, err := customers.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	bigOrders := results[0].Map()["big_orders"].(bson.A)
	assert.Len(t, bigOrders, 1, "only 1 order has amount > 100")
	assert.Equal(t, int32(150), bigOrders[0].(bson.D).Map()["amount"])
}

// TestAgg_ReplaceRoot verifies $replaceRoot promotes a nested document to top-level. (DongoFull)
func TestAgg_ReplaceRoot(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{
			{Key: "outer", Value: "ignore"},
			{Key: "inner", Value: bson.D{
				{Key: "x", Value: int32(1)},
				{Key: "y", Value: int32(2)},
			}},
		},
	)

	pipeline := bson.A{
		bson.D{{Key: "$replaceRoot", Value: bson.D{{Key: "newRoot", Value: "$inner"}}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, int32(1), m["x"])
	assert.Equal(t, int32(2), m["y"])
	assert.Nil(t, m["outer"], "$replaceRoot must not include outer fields")
}

// TestAgg_ReplaceWith verifies $replaceWith (alias for $replaceRoot) works. (DongoFull)
func TestAgg_ReplaceWith(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{
			{Key: "meta", Value: "drop"},
			{Key: "payload", Value: bson.D{{Key: "value", Value: int32(42)}}},
		},
	)

	pipeline := bson.A{
		bson.D{{Key: "$replaceWith", Value: "$payload"}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, int32(42), results[0].Map()["value"])
}

// TestAgg_UnwindBasic verifies $unwind deconstructs an array field. (DongoFull)
func TestAgg_UnwindBasic(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{
			{Key: "name", Value: "doc1"},
			{Key: "tags", Value: bson.A{"a", "b", "c"}},
		},
	)

	pipeline := bson.A{
		bson.D{{Key: "$unwind", Value: "$tags"}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "tags", Value: 1}}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 3, "$unwind must produce one doc per array element")
	assert.Equal(t, "a", results[0].Map()["tags"])
	assert.Equal(t, "b", results[1].Map()["tags"])
	assert.Equal(t, "c", results[2].Map()["tags"])
}

// TestAgg_UnwindPreserveNullAndEmpty verifies $unwind with preserveNullAndEmptyArrays
// includes documents with null or missing array fields. (DongoFull)
func TestAgg_UnwindPreserveNullAndEmpty(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "has-tags"}, {Key: "tags", Value: bson.A{"x"}}},
		bson.D{{Key: "name", Value: "empty-tags"}, {Key: "tags", Value: bson.A{}}},
		bson.D{{Key: "name", Value: "no-tags"}}, // missing field
	)

	pipeline := bson.A{
		bson.D{{Key: "$unwind", Value: bson.D{
			{Key: "path", Value: "$tags"},
			{Key: "preserveNullAndEmptyArrays", Value: true},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "name", Value: 1}}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// has-tags → 1 doc, empty-tags → 1 doc (preserved), no-tags → 1 doc (preserved)
	assert.Len(t, results, 3, "preserveNullAndEmptyArrays must keep docs with missing/empty arrays")
}

// TestAgg_UnwindIncludeArrayIndex verifies $unwind with includeArrayIndex adds
// the array position as a new field. (DongoFull)
func TestAgg_UnwindIncludeArrayIndex(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "tags", Value: bson.A{"first", "second", "third"}}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$unwind", Value: bson.D{
			{Key: "path", Value: "$tags"},
			{Key: "includeArrayIndex", Value: "idx"},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "idx", Value: 1}}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)
	assert.Equal(t, int64(0), results[0].Map()["idx"])
	assert.Equal(t, int64(1), results[1].Map()["idx"])
	assert.Equal(t, int64(2), results[2].Map()["idx"])
}

// TestAgg_Bucket verifies $bucket groups documents into ranges. (DongoFull)
func TestAgg_Bucket(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "score", Value: int32(5)}},
		bson.D{{Key: "score", Value: int32(15)}},
		bson.D{{Key: "score", Value: int32(25)}},
		bson.D{{Key: "score", Value: int32(35)}},
		bson.D{{Key: "score", Value: int32(45)}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$bucket", Value: bson.D{
			{Key: "groupBy", Value: "$score"},
			{Key: "boundaries", Value: bson.A{int32(0), int32(20), int32(40), int32(60)}},
			{Key: "default", Value: "other"},
			{Key: "output", Value: bson.D{
				{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
			}},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "_id", Value: 1}}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3, "must have 3 buckets: [0,20), [20,40), [40,60)")

	m0 := results[0].Map()
	assert.Equal(t, int32(0), m0["_id"])
	assert.Equal(t, int32(2), m0["count"], "bucket [0,20) must contain scores 5 and 15")

	m1 := results[1].Map()
	assert.Equal(t, int32(20), m1["_id"])
	assert.Equal(t, int32(2), m1["count"], "bucket [20,40) must contain scores 25 and 35")

	m2 := results[2].Map()
	assert.Equal(t, int32(40), m2["_id"])
	assert.Equal(t, int32(1), m2["count"], "bucket [40,60) must contain score 45")
}

// TestAgg_BucketAuto verifies $bucketAuto distributes documents into N evenly-sized
// buckets based on value distribution. (DongoFull)
func TestAgg_BucketAuto(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	for i := 1; i <= 10; i++ {
		insertDocs(t, coll, bson.D{{Key: "n", Value: int32(i)}})
	}

	pipeline := bson.A{
		bson.D{{Key: "$bucketAuto", Value: bson.D{
			{Key: "groupBy", Value: "$n"},
			{Key: "buckets", Value: int32(2)},
			{Key: "output", Value: bson.D{
				{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
			}},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 2, "$bucketAuto with 2 buckets must produce 2 results")

	// Each bucket must have roughly 5 documents.
	for _, r := range results {
		count := r.Map()["count"].(int32)
		assert.True(t, count >= 4 && count <= 6, "each bucket must have ~5 documents, got %d", count)
	}
}

// TestAgg_Facet verifies $facet runs multiple aggregation pipelines simultaneously
// within a single stage. (DongoFull)
func TestAgg_Facet(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "category", Value: "A"}, {Key: "price", Value: int32(10)}},
		bson.D{{Key: "category", Value: "A"}, {Key: "price", Value: int32(20)}},
		bson.D{{Key: "category", Value: "B"}, {Key: "price", Value: int32(30)}},
		bson.D{{Key: "category", Value: "B"}, {Key: "price", Value: int32(40)}},
		bson.D{{Key: "category", Value: "C"}, {Key: "price", Value: int32(50)}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$facet", Value: bson.D{
			{Key: "by_category", Value: bson.A{
				bson.D{{Key: "$group", Value: bson.D{
					{Key: "_id", Value: "$category"},
					{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
				}}},
				bson.D{{Key: "$sort", Value: bson.D{{Key: "_id", Value: 1}}}},
			}},
			{Key: "price_stats", Value: bson.A{
				bson.D{{Key: "$group", Value: bson.D{
					{Key: "_id", Value: nil},
					{Key: "total", Value: bson.D{{Key: "$sum", Value: "$price"}}},
					{Key: "avg", Value: bson.D{{Key: "$avg", Value: "$price"}}},
				}}},
			}},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1, "$facet produces exactly one output document")

	facetDoc := results[0].Map()

	byCategory := facetDoc["by_category"].(bson.A)
	assert.Len(t, byCategory, 3, "by_category must have 3 groups: A, B, C")

	priceStats := facetDoc["price_stats"].(bson.A)
	require.Len(t, priceStats, 1)
	statsMap := priceStats[0].(bson.D).Map()
	assert.Equal(t, int32(150), statsMap["total"])
}

// TestAgg_AddFields verifies $addFields appends new computed fields without
// removing existing ones. (DongoFull)
func TestAgg_AddFields(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "a", Value: int32(3)}, {Key: "b", Value: int32(4)}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$addFields", Value: bson.D{
			{Key: "sum", Value: bson.D{{Key: "$add", Value: bson.A{"$a", "$b"}}}},
			{Key: "label", Value: "computed"},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, int32(3), m["a"], "$addFields must preserve existing field 'a'")
	assert.Equal(t, int32(7), m["sum"], "$addFields must compute sum of a+b")
	assert.Equal(t, "computed", m["label"])
}

// TestAgg_Count verifies $count returns the total number of documents as a field. (DongoFull)
func TestAgg_Count(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "x", Value: int32(1)}},
		bson.D{{Key: "x", Value: int32(2)}},
		bson.D{{Key: "x", Value: int32(3)}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$count", Value: "total"}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, int32(3), results[0].Map()["total"])
}

// TestAgg_SortByCount verifies $sortByCount groups by a field and sorts by count
// descending. (DongoFull)
func TestAgg_SortByCount(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "tag", Value: "go"}},
		bson.D{{Key: "tag", Value: "go"}},
		bson.D{{Key: "tag", Value: "go"}},
		bson.D{{Key: "tag", Value: "rust"}},
		bson.D{{Key: "tag", Value: "rust"}},
		bson.D{{Key: "tag", Value: "python"}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$sortByCount", Value: "$tag"}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// Must be sorted descending by count.
	assert.Equal(t, "go", results[0].Map()["_id"])
	assert.Equal(t, int32(3), results[0].Map()["count"])
	assert.Equal(t, "rust", results[1].Map()["_id"])
	assert.Equal(t, int32(2), results[1].Map()["count"])
	assert.Equal(t, "python", results[2].Map()["_id"])
	assert.Equal(t, int32(1), results[2].Map()["count"])
}

// TestAgg_Limit_Skip verifies $limit and $skip work in combination. (DongoFull)
func TestAgg_Limit_Skip(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	for i := 1; i <= 10; i++ {
		insertDocs(t, coll, bson.D{{Key: "n", Value: int32(i)}})
	}

	pipeline := bson.A{
		bson.D{{Key: "$sort", Value: bson.D{{Key: "n", Value: 1}}}},
		bson.D{{Key: "$skip", Value: int32(3)}},
		bson.D{{Key: "$limit", Value: int32(3)}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)
	assert.Equal(t, int32(4), results[0].Map()["n"])
	assert.Equal(t, int32(5), results[1].Map()["n"])
	assert.Equal(t, int32(6), results[2].Map()["n"])
}

// TestAgg_Group_MultipleAccumulators verifies $group with multiple accumulator
// operators produces correct results. (DongoFull)
func TestAgg_Group_MultipleAccumulators(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "cat", Value: "x"}, {Key: "v", Value: int32(10)}},
		bson.D{{Key: "cat", Value: "x"}, {Key: "v", Value: int32(20)}},
		bson.D{{Key: "cat", Value: "x"}, {Key: "v", Value: int32(30)}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$cat"},
			{Key: "total", Value: bson.D{{Key: "$sum", Value: "$v"}}},
			{Key: "min_val", Value: bson.D{{Key: "$min", Value: "$v"}}},
			{Key: "max_val", Value: bson.D{{Key: "$max", Value: "$v"}}},
			{Key: "avg_val", Value: bson.D{{Key: "$avg", Value: "$v"}}},
			{Key: "cnt", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, int32(60), m["total"])
	assert.Equal(t, int32(10), m["min_val"])
	assert.Equal(t, int32(30), m["max_val"])
	assert.Equal(t, float64(20), m["avg_val"])
	assert.Equal(t, int32(3), m["cnt"])
}

// TestAgg_Group_Push verifies $push accumulator collects all values into an array. (DongoFull)
func TestAgg_Group_Push(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "dept", Value: "eng"}, {Key: "name", Value: "alice"}},
		bson.D{{Key: "dept", Value: "eng"}, {Key: "name", Value: "bob"}},
		bson.D{{Key: "dept", Value: "eng"}, {Key: "name", Value: "carol"}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$dept"},
			{Key: "members", Value: bson.D{{Key: "$push", Value: "$name"}}},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	members := results[0].Map()["members"].(bson.A)
	assert.Len(t, members, 3, "$push must collect all names")
}

// TestAgg_Group_AddToSet verifies $addToSet accumulator returns unique values. (DongoFull)
func TestAgg_Group_AddToSet(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "tag", Value: "go"}},
		bson.D{{Key: "tag", Value: "rust"}},
		bson.D{{Key: "tag", Value: "go"}},   // duplicate
		bson.D{{Key: "tag", Value: "rust"}},  // duplicate
		bson.D{{Key: "tag", Value: "python"}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "unique_tags", Value: bson.D{{Key: "$addToSet", Value: "$tag"}}},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	uniqueTags := results[0].Map()["unique_tags"].(bson.A)
	assert.Len(t, uniqueTags, 3, "$addToSet must deduplicate: go, rust, python")
}

// TestAgg_SetWindowFields_Sum verifies $setWindowFields computes a running sum
// (cumulative window). (DongoFull)
func TestAgg_SetWindowFields_Sum(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "date", Value: int32(1)}, {Key: "amount", Value: int32(100)}},
		bson.D{{Key: "date", Value: int32(2)}, {Key: "amount", Value: int32(200)}},
		bson.D{{Key: "date", Value: int32(3)}, {Key: "amount", Value: int32(150)}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$setWindowFields", Value: bson.D{
			{Key: "sortBy", Value: bson.D{{Key: "date", Value: 1}}},
			{Key: "output", Value: bson.D{
				{Key: "cumulative_sum", Value: bson.D{
					{Key: "$sum", Value: "$amount"},
					{Key: "window", Value: bson.D{
						{Key: "documents", Value: bson.A{"unbounded", "current"}},
					}},
				}},
			}},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "date", Value: 1}}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	assert.Equal(t, int32(100), results[0].Map()["cumulative_sum"])
	assert.Equal(t, int32(300), results[1].Map()["cumulative_sum"])
	assert.Equal(t, int32(450), results[2].Map()["cumulative_sum"])
}

// TestAgg_SetWindowFields_Rank verifies $rank window function assigns ranks to
// documents within a partition. (DongoFull)
func TestAgg_SetWindowFields_Rank(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "name", Value: "alice"}, {Key: "score", Value: int32(90)}},
		bson.D{{Key: "name", Value: "bob"}, {Key: "score", Value: int32(85)}},
		bson.D{{Key: "name", Value: "carol"}, {Key: "score", Value: int32(90)}}, // tie with alice
		bson.D{{Key: "name", Value: "dave"}, {Key: "score", Value: int32(75)}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$setWindowFields", Value: bson.D{
			{Key: "sortBy", Value: bson.D{{Key: "score", Value: -1}}},
			{Key: "output", Value: bson.D{
				{Key: "rank", Value: bson.D{{Key: "$rank", Value: bson.D{}}}},
			}},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "score", Value: -1}, {Key: "name", Value: 1}}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 4)

	// alice and carol both score 90, both get rank 1.
	// bob scores 85, gets rank 3 (gap after two rank-1s).
	// dave scores 75, gets rank 4.
	byName := map[string]bson.D{}
	for _, r := range results {
		byName[r.Map()["name"].(string)] = r
	}
	assert.Equal(t, int64(1), byName["alice"].Map()["rank"])
	assert.Equal(t, int64(1), byName["carol"].Map()["rank"])
	assert.Equal(t, int64(3), byName["bob"].Map()["rank"])
	assert.Equal(t, int64(4), byName["dave"].Map()["rank"])
}

// TestAgg_SetWindowFields_DenseRank verifies $denseRank assigns consecutive ranks
// without gaps for ties. (DongoFull)
func TestAgg_SetWindowFields_DenseRank(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "score", Value: int32(90)}},
		bson.D{{Key: "score", Value: int32(90)}}, // tie
		bson.D{{Key: "score", Value: int32(80)}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$setWindowFields", Value: bson.D{
			{Key: "sortBy", Value: bson.D{{Key: "score", Value: -1}}},
			{Key: "output", Value: bson.D{
				{Key: "dr", Value: bson.D{{Key: "$denseRank", Value: bson.D{}}}},
			}},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "score", Value: -1}}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// Both 90s get dense rank 1; 80 gets dense rank 2 (no gap).
	assert.Equal(t, int64(1), results[0].Map()["dr"])
	assert.Equal(t, int64(1), results[1].Map()["dr"])
	assert.Equal(t, int64(2), results[2].Map()["dr"])
}

// TestAgg_SetWindowFields_DocumentNumber verifies $documentNumber assigns a
// sequential position (1-based) to each document in the window. (DongoFull)
func TestAgg_SetWindowFields_DocumentNumber(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "v", Value: int32(30)}},
		bson.D{{Key: "v", Value: int32(10)}},
		bson.D{{Key: "v", Value: int32(20)}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$setWindowFields", Value: bson.D{
			{Key: "sortBy", Value: bson.D{{Key: "v", Value: 1}}},
			{Key: "output", Value: bson.D{
				{Key: "pos", Value: bson.D{{Key: "$documentNumber", Value: bson.D{}}}},
			}},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "v", Value: 1}}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	assert.Equal(t, int64(1), results[0].Map()["pos"])
	assert.Equal(t, int64(2), results[1].Map()["pos"])
	assert.Equal(t, int64(3), results[2].Map()["pos"])
}

// TestAgg_SetWindowFields_Partition verifies $setWindowFields with a partitionBy
// clause applies windows separately per partition. (DongoFull)
func TestAgg_SetWindowFields_Partition(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "dept", Value: "eng"}, {Key: "v", Value: int32(100)}},
		bson.D{{Key: "dept", Value: "eng"}, {Key: "v", Value: int32(200)}},
		bson.D{{Key: "dept", Value: "sales"}, {Key: "v", Value: int32(50)}},
		bson.D{{Key: "dept", Value: "sales"}, {Key: "v", Value: int32(75)}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$setWindowFields", Value: bson.D{
			{Key: "partitionBy", Value: "$dept"},
			{Key: "sortBy", Value: bson.D{{Key: "v", Value: 1}}},
			{Key: "output", Value: bson.D{
				{Key: "dept_rank", Value: bson.D{{Key: "$rank", Value: bson.D{}}}},
				{Key: "dept_total", Value: bson.D{
					{Key: "$sum", Value: "$v"},
					{Key: "window", Value: bson.D{
						{Key: "documents", Value: bson.A{"unbounded", "unbounded"}},
					}},
				}},
			}},
		}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 4)

	// Verify partition totals.
	for _, r := range results {
		m := r.Map()
		dept := m["dept"].(string)
		total := m["dept_total"].(int32)
		switch dept {
		case "eng":
			assert.Equal(t, int32(300), total, "eng dept total must be 300")
		case "sales":
			assert.Equal(t, int32(125), total, "sales dept total must be 125")
		}
	}
}

// TestAgg_SetWindowFields_Avg verifies $avg window function over a sliding range. (DongoFull)
func TestAgg_SetWindowFields_Avg(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "day", Value: int32(1)}, {Key: "sales", Value: int32(10)}},
		bson.D{{Key: "day", Value: int32(2)}, {Key: "sales", Value: int32(20)}},
		bson.D{{Key: "day", Value: int32(3)}, {Key: "sales", Value: int32(30)}},
		bson.D{{Key: "day", Value: int32(4)}, {Key: "sales", Value: int32(40)}},
	)

	// 2-document window: current + 1 preceding.
	pipeline := bson.A{
		bson.D{{Key: "$setWindowFields", Value: bson.D{
			{Key: "sortBy", Value: bson.D{{Key: "day", Value: 1}}},
			{Key: "output", Value: bson.D{
				{Key: "moving_avg", Value: bson.D{
					{Key: "$avg", Value: "$sales"},
					{Key: "window", Value: bson.D{
						{Key: "documents", Value: bson.A{int32(-1), "current"}},
					}},
				}},
			}},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "day", Value: 1}}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 4)

	// day 1: only itself → avg 10
	assert.InDelta(t, float64(10), results[0].Map()["moving_avg"], 0.001)
	// day 2: days 1+2 → avg 15
	assert.InDelta(t, float64(15), results[1].Map()["moving_avg"], 0.001)
	// day 3: days 2+3 → avg 25
	assert.InDelta(t, float64(25), results[2].Map()["moving_avg"], 0.001)
	// day 4: days 3+4 → avg 35
	assert.InDelta(t, float64(35), results[3].Map()["moving_avg"], 0.001)
}

// TestAgg_SetWindowFields_Min_Max verifies $min and $max window functions over
// an unbounded range. (DongoFull)
func TestAgg_SetWindowFields_Min_Max(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "pos", Value: int32(1)}, {Key: "v", Value: int32(50)}},
		bson.D{{Key: "pos", Value: int32(2)}, {Key: "v", Value: int32(10)}},
		bson.D{{Key: "pos", Value: int32(3)}, {Key: "v", Value: int32(80)}},
	)

	pipeline := bson.A{
		bson.D{{Key: "$setWindowFields", Value: bson.D{
			{Key: "sortBy", Value: bson.D{{Key: "pos", Value: 1}}},
			{Key: "output", Value: bson.D{
				{Key: "global_min", Value: bson.D{
					{Key: "$min", Value: "$v"},
					{Key: "window", Value: bson.D{
						{Key: "documents", Value: bson.A{"unbounded", "unbounded"}},
					}},
				}},
				{Key: "global_max", Value: bson.D{
					{Key: "$max", Value: "$v"},
					{Key: "window", Value: bson.D{
						{Key: "documents", Value: bson.A{"unbounded", "unbounded"}},
					}},
				}},
			}},
		}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "pos", Value: 1}}}},
	}

	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// All documents should see the global min=10 and max=80.
	for _, r := range results {
		m := r.Map()
		assert.Equal(t, int32(10), m["global_min"])
		assert.Equal(t, int32(80), m["global_max"])
	}
}
