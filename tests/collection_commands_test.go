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
	"go.mongodb.org/mongo-driver/mongo"
)

// TestCollMod_NonExistentCollection verifies that collMod on a collection that
// does not exist returns NamespaceNotFound (code 26) rather than an internal error.
// Regression for do-kaja: when the database had never been created, the handler
// received ErrorCodeDatabaseDoesNotExist from the backend but did not map it to
// NamespaceNotFound, causing an unexpected internal error response.
func TestCollMod_NonExistentCollection(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	ctx := context.Background()

	// Use a collection handle that was never inserted into — neither the database
	// nor the collection have been created in the storage engine.
	coll := env.collection(t)

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "collMod", Value: coll.Name()},
	}).Decode(&res)

	require.Error(t, err)
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 26, cmdErr.Code, "expected NamespaceNotFound (26), got code %d: %s", cmdErr.Code, cmdErr.Message)
}

// TestCollMod_InvalidOption verifies that collMod rejects unknown fields with
// IDLUnknownField (code 40415).
func TestCollMod_InvalidOption(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	ctx := context.Background()

	// First create the collection so the command reaches field-validation logic.
	coll := env.collection(t)
	_, err := coll.InsertOne(ctx, bson.D{{Key: "x", Value: 1}})
	require.NoError(t, err)

	var res bson.D
	err = coll.Database().RunCommand(ctx, bson.D{
		{Key: "collMod", Value: coll.Name()},
		{Key: "unknownOption", Value: 1},
	}).Decode(&res)

	require.Error(t, err)
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 40415, cmdErr.Code, "expected IDLUnknownField (40415), got code %d: %s", cmdErr.Code, cmdErr.Message)
}

// TestCompact_EmptyCollection verifies that compact on an existing but empty
// collection succeeds and returns bytesFreed=0 with ok=1.
func TestCompact_EmptyCollection(t *testing.T) {
	// Do not run in parallel — compact acquires broad locks internally.

	env := startDongo(t)
	ctx := context.Background()

	// Explicitly create the collection so it exists but has no documents.
	dbName := "testdb_compact_empty"
	collName := "compact_empty_col"
	err := env.client.Database(dbName).CreateCollection(ctx, collName)
	require.NoError(t, err)
	t.Cleanup(func() {
		env.client.Database(dbName).Drop(context.Background()) //nolint:errcheck
	})

	var res bson.D
	err = env.client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "compact", Value: collName},
	}).Decode(&res)
	require.NoError(t, err, "compact on empty collection must succeed")

	// Extract bytesFreed and verify the rest of the document is {ok: 1}.
	var bytesFreed interface{}
	filtered := make(bson.D, 0, len(res))
	for _, el := range res {
		if el.Key == "bytesFreed" {
			bytesFreed = el.Value
		} else {
			filtered = append(filtered, el)
		}
	}
	assert.NotNil(t, bytesFreed, "response must contain bytesFreed field")
	assert.Equal(t, bson.D{{Key: "ok", Value: float64(1)}}, filtered)
}

// TestCompact_NonExistentCollection verifies that compact on a collection that
// does not exist returns NamespaceNotFound (code 26).
func TestCompact_NonExistentCollection(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	ctx := context.Background()

	// Use a collection handle that was never inserted into.
	coll := env.collection(t)

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "compact", Value: coll.Name()},
	}).Decode(&res)

	require.Error(t, err)
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 26, cmdErr.Code, "expected NamespaceNotFound (26), got code %d: %s", cmdErr.Code, cmdErr.Message)
}

// TestAutoCompact_Enable_Disable_FreeSpaceTargetMB verifies that the autoCompact
// command accepts enable/disable and freeSpaceTargetMB parameters when run
// against the admin database.
func TestAutoCompact_Enable_Disable_FreeSpaceTargetMB(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	ctx := context.Background()
	admin := env.client.Database("admin")

	subtests := []struct {
		name    string
		command bson.D
	}{
		{
			name: "Enable",
			command: bson.D{
				{Key: "autoCompact", Value: 1},
				{Key: "enable", Value: true},
			},
		},
		{
			name: "Disable",
			command: bson.D{
				{Key: "autoCompact", Value: 1},
				{Key: "enable", Value: false},
			},
		},
		{
			name: "FreeSpaceTargetMB",
			command: bson.D{
				{Key: "autoCompact", Value: 1},
				{Key: "enable", Value: true},
				{Key: "freeSpaceTargetMB", Value: int32(500)},
			},
		},
	}

	for _, tc := range subtests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var res bson.D
			err := admin.RunCommand(ctx, tc.command).Decode(&res)
			require.NoError(t, err, "autoCompact %s against admin must succeed", tc.name)

			assert.Equal(t, bson.D{{Key: "ok", Value: float64(1)}}, res)
		})
	}
}

// assertValidateResponse checks the structural correctness of a validate command response,
// verifying that all MongoDB-required fields are present with appropriate types.
func assertValidateResponse(t *testing.T, res bson.D, coll *mongo.Collection) {
	t.Helper()

	m := res.Map()

	assert.Equal(t, float64(1), m["ok"], "ok must be 1")
	assert.Equal(t, true, m["valid"], "valid must be true for a healthy collection")

	ns, ok := m["ns"].(string)
	require.True(t, ok, "ns must be a string, got %T", m["ns"])
	assert.Equal(t, coll.Database().Name()+"."+coll.Name(), ns, "ns must be db.collection")

	_, ok = m["nrecords"].(int32)
	assert.True(t, ok, "nrecords must be int32, got %T", m["nrecords"])

	nIndexes, ok := m["nIndexes"].(int32)
	require.True(t, ok, "nIndexes must be int32, got %T", m["nIndexes"])
	assert.GreaterOrEqual(t, nIndexes, int32(1), "nIndexes must be >= 1")

	_, ok = m["keysPerIndex"].(bson.D)
	assert.True(t, ok, "keysPerIndex must be a document, got %T", m["keysPerIndex"])

	_, ok = m["indexDetails"].(bson.D)
	assert.True(t, ok, "indexDetails must be a document, got %T", m["indexDetails"])

	for _, field := range []string{"warnings", "errors", "extraIndexEntries", "missingIndexEntries", "corruptRecords"} {
		_, ok := m[field].(bson.A)
		assert.True(t, ok, "%s must be an array, got %T", field, m[field])
	}
}

// TestValidate_Full verifies that validate with full:true performs a deeper collection
// check and returns a response document structurally compatible with MongoDB's format.
//
// Parity test for do-h7a9: validate command options diverge from MongoDB. (DongoFull)
func TestValidate_Full(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "a", Value: int32(1)}},
		bson.D{{Key: "a", Value: int32(2)}},
	)

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "validate", Value: coll.Name()},
		{Key: "full", Value: true},
	}).Decode(&res)
	require.NoError(t, err, "validate with full:true must not error")

	assertValidateResponse(t, res, coll)

	// repaired must be false — full:true is a read-only deep scan, not a repair.
	m := res.Map()
	assert.Equal(t, false, m["repaired"], "full:true must not set repaired:true")
}

// TestValidate_Repair verifies that validate with repair:true attempts to fix
// inconsistencies and returns repaired:false for a collection that needs no repairs.
//
// Parity test for do-h7a9: validate command options diverge from MongoDB. (DongoFull)
func TestValidate_Repair(t *testing.T) {
	// Do not run in parallel — repair acquires exclusive collection locks.

	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "b", Value: int32(1)}},
		bson.D{{Key: "b", Value: int32(2)}},
	)

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "validate", Value: coll.Name()},
		{Key: "repair", Value: true},
	}).Decode(&res)
	require.NoError(t, err, "validate with repair:true must not error")

	assertValidateResponse(t, res, coll)

	// A fresh, consistent collection requires no repairs — repaired must be false.
	m := res.Map()
	assert.Equal(t, false, m["repaired"], "repaired must be false when no repairs were needed")
}
