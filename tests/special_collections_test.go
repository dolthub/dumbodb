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
	"go.mongodb.org/mongo-driver/v2/bson"
)

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
