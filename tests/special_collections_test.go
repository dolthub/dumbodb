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

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestCapped_SmallSize_ManyInserts verifies that a capped collection enforces its
// size limit by evicting the oldest documents when the limit is exceeded.
//
// Regression for do-i7xc: inserts into a capped collection did not trigger
// eviction; the periodic background cleanup was the only path that enforced
// the size limit. The fix calls cleanupCappedCollection after each successful
// insert into a capped collection.
func TestCapped_SmallSize_ManyInserts(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("testdb_capped_smallsize_%d", rand.Int64())
	db := env.client.Database(dbName)
	t.Cleanup(func() {
		db.Drop(context.Background()) //nolint:errcheck
	})

	// Create a capped collection with 1024 bytes.
	// The dolt backend estimates avgDocSize=64 bytes, so 20 documents occupy
	// 1280 bytes, which exceeds the 1024-byte cap and must trigger eviction.
	const cappedSize = int64(1024)
	err := db.CreateCollection(ctx, "cappedcoll", options.CreateCollection().SetCapped(true).SetSizeInBytes(cappedSize))
	require.NoError(t, err, "creating capped collection must succeed")

	coll := db.Collection("cappedcoll")

	// Insert 20 documents.  At 64 estimated bytes each, total size (1280 bytes)
	// exceeds the 1024-byte cap, so the collection must evict old documents.
	docs := make([]interface{}, 20)
	for i := range docs {
		docs[i] = bson.D{{Key: "i", Value: int32(i)}}
	}
	_, err = coll.InsertMany(ctx, docs)
	require.NoError(t, err, "InsertMany into capped collection must succeed")

	count, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err, "CountDocuments must not error")
	require.Less(t, count, int64(20), "capped collection must evict old documents when size exceeded")
}

// TestView_WithLookupPipeline verifies that a view defined with a $lookup stage
// in its pipeline can be created and queried correctly.
//
// Regression for do-djjm: view pipeline stages containing $lookup were built
// with stages.NewStage() which returns ErrNotImplemented for $lookup (it is in
// unsupportedStages). The fix adds special handling for $lookup/$graphLookup
// when materialising view pipeline stages in msg_aggregate.go, mirroring the
// existing logic for user-supplied aggregate pipeline stages.
func TestView_WithLookupPipeline(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()

	// Use a unique database per test to avoid cross-test collection name collisions.
	dbName := fmt.Sprintf("testdb_view_lookup_%d", rand.Int64())
	db := env.client.Database(dbName)
	t.Cleanup(func() {
		db.Drop(context.Background()) //nolint:errcheck
	})

	// Create the "orders" collection and insert two orders.
	orders := db.Collection("orders")
	_, err := orders.InsertMany(ctx, []interface{}{
		bson.D{{Key: "order_id", Value: 1}, {Key: "item", Value: "apple"}},
		bson.D{{Key: "order_id", Value: 2}, {Key: "item", Value: "banana"}},
	})
	require.NoError(t, err)

	// Create the "inventory" collection with matching items.
	inventory := db.Collection("inventory")
	_, err = inventory.InsertMany(ctx, []interface{}{
		bson.D{{Key: "sku", Value: "apple"}, {Key: "qty", Value: 100}},
		bson.D{{Key: "sku", Value: "banana"}, {Key: "qty", Value: 50}},
	})
	require.NoError(t, err)

	// Create a view "orders_enriched" that joins orders with inventory via $lookup.
	// The view pipeline: [{ $lookup: { from: "inventory", localField: "item", foreignField: "sku", as: "stock" } }]
	err = db.CreateView(ctx, "orders_enriched", "orders", bson.A{
		bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "inventory"},
			{Key: "localField", Value: "item"},
			{Key: "foreignField", Value: "sku"},
			{Key: "as", Value: "stock"},
		}}},
	})
	require.NoError(t, err, "creating view with $lookup pipeline must succeed")

	// CountDocuments on the view should return 2 (one result per order, each
	// enriched with the matching inventory document via the $lookup join).
	view := db.Collection("orders_enriched")
	count, err := view.CountDocuments(ctx, bson.D{})
	require.NoError(t, err, "CountDocuments on $lookup view must not error")
	require.EqualValues(t, 2, count, "view with $lookup should return one document per order")
}
