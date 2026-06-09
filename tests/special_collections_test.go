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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Capped collections cannot be implemented coherently in DumboDB: FIFO
// eviction needs a single global insertion order, but with branches and
// merges there is none. The handler rejects every entry point that would
// create one. These tests pin that contract  -- code 72 (InvalidOptions)
// plus an error message that mentions "capped" so clients can branch on it.

const cappedRejectionFragment = "capped collections are not supported"

func assertCappedRejection(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err, "command must fail when requesting capped semantics")
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 72, cmdErr.Code,
		"expected InvalidOptions (72), got %d: %s", cmdErr.Code, cmdErr.Message)
	assert.Contains(t, strings.ToLower(cmdErr.Message), cappedRejectionFragment,
		"rejection message must mention %q so clients can branch on it; got: %s",
		cappedRejectionFragment, cmdErr.Message)
}

// TestCappedCreate_Rejected verifies that the create command with capped:true
// is rejected with InvalidOptions before any storage-layer work.
func TestCappedCreate_Rejected(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "create", Value: coll.Name()},
		{Key: "capped", Value: true},
		{Key: "size", Value: int64(4096)},
	}).Err()

	assertCappedRejection(t, err)
}

// TestCappedCreate_RejectedBeforeSizeValidation verifies that the capped check
// runs before size validation: a request with capped:true but no size must
// surface the capped-rejection error, not a "size is required" error.
//
// This pins the order of checks: clients receive a stable error keyed on the
// real problem (capped is unsupported) rather than a misleading complaint about
// a field that would never have mattered.
func TestCappedCreate_RejectedBeforeSizeValidation(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "create", Value: coll.Name()},
		{Key: "capped", Value: true},
		// size intentionally omitted
	}).Err()

	assertCappedRejection(t, err)
}

// TestCappedCreate_NoOptionsStillWorks guards the happy path: createCollection
// without any capped option must still succeed. Regression guard against the
// rejection logic accidentally widening to all creates.
func TestCappedCreate_NoOptionsStillWorks(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("testdb_create_nocapped_%d", rand.Int64())
	db := env.client.Database(dbName)
	t.Cleanup(func() {
		db.Drop(context.Background()) //nolint:errcheck
	})

	require.NoError(t, db.CreateCollection(ctx, "plain_col"),
		"createCollection without capped option must still succeed")

	names, err := db.ListCollectionNames(ctx, bson.D{{Key: "name", Value: "plain_col"}})
	require.NoError(t, err)
	require.Equal(t, []string{"plain_col"}, names)
}

// TestConvertToCapped_Rejected verifies that convertToCapped is rejected even
// when the target collection exists and the size argument is well-formed.
func TestConvertToCapped_Rejected(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Make sure the collection exists so the rejection cannot be confused with
	// NamespaceNotFound.
	_, err := coll.InsertOne(ctx, bson.D{{Key: "x", Value: int32(1)}})
	require.NoError(t, err)

	err = coll.Database().RunCommand(ctx, bson.D{
		{Key: "convertToCapped", Value: coll.Name()},
		{Key: "size", Value: int64(1024 * 1024)},
	}).Err()

	assertCappedRejection(t, err)
}

// TestConvertToCapped_RejectedOnMissingCollection verifies that convertToCapped
// on a non-existent collection still reports the capped rejection, not
// NamespaceNotFound. The command never works regardless of inputs, so the
// caller should learn the real reason.
func TestConvertToCapped_RejectedOnMissingCollection(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Do not create the collection.
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "convertToCapped", Value: coll.Name()},
		{Key: "size", Value: int64(1024 * 1024)},
	}).Err()

	assertCappedRejection(t, err)
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
