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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// TestAdminBulkWrite_InsertUpdateDelete exercises the MongoDB 8.0 server-side
// bulkWrite command against two collections in the same database. The driver
// exposes this as `admin.runCommand({bulkWrite: 1, ...})` — note that this is
// distinct from the driver-level Collection.BulkWrite, which decomposes into
// individual write commands and is covered by TestCRUD_BulkWrite_*.
func TestAdminBulkWrite_InsertUpdateDelete(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := "bulkwrite_admin_test"
	db := env.client.Database(dbName)
	require.NoError(t, db.Drop(ctx))
	defer db.Drop(ctx) //nolint:errcheck // best-effort cleanup

	// Seed collectionA and collectionB so update/delete have something to hit.
	collA := db.Collection("collectionA")
	collB := db.Collection("collectionB")
	_, err := collA.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "y", Value: int32(1)}})
	require.NoError(t, err)
	_, err = collB.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "z", Value: int32(1)}})
	require.NoError(t, err)

	admin := env.client.Database("admin")
	cmd := bson.D{
		{Key: "bulkWrite", Value: int32(1)},
		{Key: "ops", Value: bson.A{
			bson.D{{Key: "insert", Value: int32(0)}, {Key: "document", Value: bson.D{{Key: "_id", Value: int32(1)}, {Key: "x", Value: int32(1)}}}},
			bson.D{{Key: "update", Value: int32(0)}, {Key: "filter", Value: bson.D{{Key: "_id", Value: int32(2)}}}, {Key: "updateMods", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "y", Value: int32(2)}}}}}},
			bson.D{{Key: "delete", Value: int32(1)}, {Key: "filter", Value: bson.D{{Key: "_id", Value: int32(3)}}}},
		}},
		{Key: "nsInfo", Value: bson.A{
			bson.D{{Key: "ns", Value: dbName + ".collectionA"}},
			bson.D{{Key: "ns", Value: dbName + ".collectionB"}},
		}},
	}

	res := runCommandRaw(t, admin, cmd)

	assert.InDelta(t, 1.0, res["ok"], 0.0001, "bulkWrite should return ok:1")
	assert.Equal(t, int32(1), res["nInserted"])
	assert.Equal(t, int32(1), res["nMatched"])
	assert.Equal(t, int32(1), res["nModified"])
	assert.Equal(t, int32(1), res["nDeleted"])
	assert.Equal(t, int32(0), res["nErrors"])

	// Verify the side effects actually landed, not just the reported counts.
	var got bson.M
	require.NoError(t, collA.FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&got))
	assert.Equal(t, int32(1), got["x"])

	require.NoError(t, collA.FindOne(ctx, bson.D{{Key: "_id", Value: int32(2)}}).Decode(&got))
	assert.Equal(t, int32(2), got["y"])

	err = collB.FindOne(ctx, bson.D{{Key: "_id", Value: int32(3)}}).Err()
	assert.Error(t, err, "_id=3 should be deleted from collectionB")
}

// TestAdminBulkWrite_OrderedStopsOnError verifies that the ordered flag causes
// bulkWrite to stop after the first failing op — the third op (a delete) must
// not execute when the second op (duplicate insert) fails.
func TestAdminBulkWrite_OrderedStopsOnError(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := "bulkwrite_admin_ordered_test"
	db := env.client.Database(dbName)
	require.NoError(t, db.Drop(ctx))
	defer db.Drop(ctx) //nolint:errcheck // best-effort cleanup

	coll := db.Collection("things")
	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(42)}})
	require.NoError(t, err)

	admin := env.client.Database("admin")
	cmd := bson.D{
		{Key: "bulkWrite", Value: int32(1)},
		{Key: "ordered", Value: true},
		{Key: "ops", Value: bson.A{
			bson.D{{Key: "insert", Value: int32(0)}, {Key: "document", Value: bson.D{{Key: "_id", Value: int32(7)}}}},
			// duplicate _id triggers an error
			bson.D{{Key: "insert", Value: int32(0)}, {Key: "document", Value: bson.D{{Key: "_id", Value: int32(42)}}}},
			// third op would remove _id=7 if we got this far
			bson.D{{Key: "delete", Value: int32(0)}, {Key: "filter", Value: bson.D{{Key: "_id", Value: int32(7)}}}},
		}},
		{Key: "nsInfo", Value: bson.A{
			bson.D{{Key: "ns", Value: dbName + ".things"}},
		}},
	}

	res := runCommandRaw(t, admin, cmd)

	assert.Equal(t, int32(1), res["nInserted"], "only first insert should have run")
	assert.Equal(t, int32(1), res["nErrors"])
	assert.Equal(t, int32(0), res["nDeleted"], "third op must not have executed after ordered break")

	// _id=7 survived because the delete never ran.
	var got bson.M
	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "_id", Value: int32(7)}}).Decode(&got))
}
