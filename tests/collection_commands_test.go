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
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// TestCollMod_NonExistentCollection verifies that collMod on a collection that
// does not exist returns NamespaceNotFound (code 26) rather than an internal error.
// Regression for do-kaja: when the database had never been created, the handler
// received ErrorCodeDatabaseDoesNotExist from the backend but did not map it to
// NamespaceNotFound, causing an unexpected internal error response.
func TestCollMod_NonExistentCollection(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()

	// Use a collection handle that was never inserted into  -- neither the database
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

	env := startDumboDB(t)
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
	// Do not run in parallel  -- compact acquires broad locks internally.

	env := startDumboDB(t)
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

	env := startDumboDB(t)
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

	env := startDumboDB(t)
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

	m := dmap(res)

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

	_, ok = m["keysPerIndex"].(bson.M)
	assert.True(t, ok, "keysPerIndex must be a document, got %T", m["keysPerIndex"])

	_, ok = m["indexDetails"].(bson.M)
	assert.True(t, ok, "indexDetails must be a document, got %T", m["indexDetails"])

	for _, field := range []string{"warnings", "errors", "extraIndexEntries", "missingIndexEntries", "corruptRecords"} {
		_, ok := m[field].(bson.A)
		assert.True(t, ok, "%s must be an array, got %T", field, m[field])
	}
}

// TestValidate_Full verifies that validate with full:true performs a deeper collection
// check and returns a response document structurally compatible with MongoDB's format.
//
// Parity test for do-h7a9: validate command options diverge from MongoDB. (DumboDBFull)
func TestValidate_Full(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
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

	// repaired must be false  -- full:true is a read-only deep scan, not a repair.
	m := dmap(res)
	assert.Equal(t, false, m["repaired"], "full:true must not set repaired:true")
}

// TestValidate_Repair verifies that validate with repair:true attempts to fix
// inconsistencies and returns repaired:false for a collection that needs no repairs.
//
// Parity test for do-h7a9: validate command options diverge from MongoDB. (DumboDBFull)
func TestValidate_Repair(t *testing.T) {
	// Do not run in parallel  -- repair acquires exclusive collection locks.

	env := startDumboDB(t)
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

	// A fresh, consistent collection requires no repairs  -- repaired must be false.
	m := dmap(res)
	assert.Equal(t, false, m["repaired"], "repaired must be false when no repairs were needed")
}

// TestConvertToCapped_VerifyCapped verifies that after convertToCapped succeeds,
// a subsequent listCollections call reports options.capped=true and options.size
// equal to the requested capped size.
//
// Parity test for do-tfox: convertToCapped should mark collection as capped in listCollections.
func TestConvertToCapped_VerifyCapped(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Insert a document so the collection exists.
	insertDocs(t, coll, bson.D{{Key: "x", Value: int32(1)}})

	const cappedSize = int64(1024 * 1024) // 1 MiB

	// Convert the collection to a capped collection.
	var convertRes bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "convertToCapped", Value: coll.Name()},
		{Key: "size", Value: cappedSize},
	}).Decode(&convertRes)
	require.NoError(t, err, "convertToCapped must succeed")
	assert.Equal(t, float64(1), dmap(convertRes)["ok"], "convertToCapped ok must be 1")

	// Verify listCollections shows capped=true and size=cappedSize for this collection.
	cursor, err := coll.Database().ListCollections(ctx, bson.D{
		{Key: "name", Value: coll.Name()},
	})
	require.NoError(t, err, "listCollections must not error")
	defer cursor.Close(ctx) //nolint:errcheck

	var results []bson.Raw
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1, "listCollections must return exactly one entry for the collection")

	entry := results[0]
	optionsDoc, ok := entry.Lookup("options").DocumentOK()
	require.True(t, ok, "listCollections entry must have an 'options' document, got %T", entry.Lookup("options"))

	// options.capped must be true.
	cappedVal, ok := optionsDoc.Lookup("capped").BooleanOK()
	require.True(t, ok, "options.capped must be a boolean, got type %s", optionsDoc.Lookup("capped").Type)
	assert.True(t, cappedVal, "options.capped must be true after convertToCapped")

	// options.size must equal the requested capped size.
	sizeVal := optionsDoc.Lookup("size")
	var gotSize int64
	switch sizeVal.Type {
	case bson.TypeInt32:
		gotSize = int64(sizeVal.Int32())
	case bson.TypeInt64:
		gotSize = sizeVal.Int64()
	case bson.TypeDouble:
		gotSize = int64(sizeVal.Double())
	default:
		t.Fatalf("options.size must be a numeric type, got %s", sizeVal.Type)
	}
	assert.Equal(t, cappedSize, gotSize, "options.size must equal the convertToCapped size parameter")
}

// TestConvertToCapped_Basic verifies that convertToCapped succeeds on an existing collection
// and returns ok=1.
//
// Parity test for do-ncv8: convertToCapped, validate, dataSize, renameCollection parity.
func TestConvertToCapped_Basic(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Insert a document so the collection exists.
	insertDocs(t, coll, bson.D{{Key: "x", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "convertToCapped", Value: coll.Name()},
		{Key: "size", Value: int64(1024 * 1024)},
	}).Decode(&res)
	require.NoError(t, err, "convertToCapped on existing collection must succeed")
	assert.Equal(t, float64(1), dmap(res)["ok"], "ok must be 1")
}

// TestConvertToCapped_NonExistentCollection verifies that convertToCapped on a collection
// that does not exist returns NamespaceNotFound (code 26).
//
// Parity test for do-ncv8.
func TestConvertToCapped_NonExistentCollection(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Do not insert  -- collection was never created.
	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "convertToCapped", Value: coll.Name()},
		{Key: "size", Value: int64(1024 * 1024)},
	}).Decode(&res)

	require.Error(t, err)
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 26, cmdErr.Code, "expected NamespaceNotFound (26), got code %d: %s", cmdErr.Code, cmdErr.Message)
}

// TestConvertToCapped_ZeroSize verifies that convertToCapped with size=0 returns
// an error indicating the capped size must be greater than zero.
//
// Parity test for do-ncv8.
func TestConvertToCapped_ZeroSize(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "x", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "convertToCapped", Value: coll.Name()},
		{Key: "size", Value: int64(0)},
	}).Decode(&res)

	require.Error(t, err, "convertToCapped with size=0 must fail")
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 72, cmdErr.Code, "expected InvalidOptions (72), got code %d: %s", cmdErr.Code, cmdErr.Message)
}

// TestConvertToCapped_MissingSize verifies that convertToCapped without a size field
// returns an error indicating the capped size must be greater than zero.
//
// Parity test for do-ncv8.
func TestConvertToCapped_MissingSize(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "x", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "convertToCapped", Value: coll.Name()},
		// size field intentionally omitted
	}).Decode(&res)

	require.Error(t, err, "convertToCapped without size must fail")
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 72, cmdErr.Code, "expected InvalidOptions (72), got code %d: %s", cmdErr.Code, cmdErr.Message)
}

// TestDataSize_BasicCollection verifies that the dataSize command returns a valid
// response for an existing collection with documents.
//
// Parity test for do-ncv8.
func TestDataSize_BasicCollection(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll,
		bson.D{{Key: "a", Value: int32(1)}},
		bson.D{{Key: "a", Value: int32(2)}},
		bson.D{{Key: "a", Value: int32(3)}},
	)

	namespace := coll.Database().Name() + "." + coll.Name()
	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "dataSize", Value: namespace},
	}).Decode(&res)
	require.NoError(t, err, "dataSize on existing collection must not error")

	m := dmap(res)
	assert.Equal(t, float64(1), m["ok"], "ok must be 1")

	size, ok := m["size"].(int64)
	require.True(t, ok, "size must be int64, got %T", m["size"])
	assert.Greater(t, size, int64(0), "size must be > 0 for non-empty collection")

	numObjects, ok := m["numObjects"].(int64)
	require.True(t, ok, "numObjects must be int64, got %T", m["numObjects"])
	assert.EqualValues(t, 3, numObjects, "numObjects must equal the number of inserted documents")

	_, ok = m["millis"]
	assert.True(t, ok, "millis field must be present")
}

// TestDataSize_WithKeyRange verifies that the dataSize command with keyPattern, min, and
// max parameters returns only the size of documents within the specified range.
//
// Parity test for do-ncv8.
func TestDataSize_WithKeyRange(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Insert 5 documents with values 1-5 on field "v".
	for i := int32(1); i <= 5; i++ {
		insertDocs(t, coll, bson.D{{Key: "v", Value: i}})
	}

	namespace := coll.Database().Name() + "." + coll.Name()

	// Retrieve total size (all 5 docs).
	var fullRes bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "dataSize", Value: namespace},
	}).Decode(&fullRes)
	require.NoError(t, err)
	fullNum := dmap(fullRes)["numObjects"].(int64)
	assert.EqualValues(t, 5, fullNum, "full dataSize must count all 5 documents")

	// Retrieve size for range v in [2, 4)  -- expects documents with v=2 and v=3.
	var rangeRes bson.D
	err = coll.Database().RunCommand(ctx, bson.D{
		{Key: "dataSize", Value: namespace},
		{Key: "keyPattern", Value: bson.D{{Key: "v", Value: int32(1)}}},
		{Key: "min", Value: bson.D{{Key: "v", Value: int32(2)}}},
		{Key: "max", Value: bson.D{{Key: "v", Value: int32(4)}}},
	}).Decode(&rangeRes)
	require.NoError(t, err, "dataSize with key range must not error")

	rm := dmap(rangeRes)
	assert.Equal(t, float64(1), rm["ok"], "ok must be 1")

	rangeNum := rm["numObjects"].(int64)
	assert.EqualValues(t, 2, rangeNum, "dataSize with range [2,4) must count 2 documents (v=2, v=3)")

	rangeSize := rm["size"].(int64)
	fullSize := dmap(fullRes)["size"].(int64)
	assert.Less(t, rangeSize, fullSize, "range size must be less than full collection size")
}

// TestRenameCollection_NonExistentSource verifies that renameCollection returns
// NamespaceNotFound (code 26) when the source collection does not exist.
//
// Parity test for do-ncv8.
func TestRenameCollection_NonExistentSource(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// coll was never inserted into  -- it does not exist.
	dbName := coll.Database().Name()
	from := dbName + "." + coll.Name()
	to := dbName + "." + coll.Name() + "_renamed"

	var res bson.D
	err := env.client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "renameCollection", Value: from},
		{Key: "to", Value: to},
	}).Decode(&res)

	require.Error(t, err)
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 26, cmdErr.Code, "expected NamespaceNotFound (26), got code %d: %s", cmdErr.Code, cmdErr.Message)
}

// TestRenameCollection_DropTarget verifies that renameCollection with dropTarget:true
// succeeds even when the target collection already exists by dropping it first.
//
// Parity test for do-ncv8.
func TestRenameCollection_DropTarget(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()

	dbName := "testdb_rename_droptarget"
	srcName := "src_col"
	dstName := "dst_col"

	db := env.client.Database(dbName)
	t.Cleanup(func() { db.Drop(context.Background()) }) //nolint:errcheck

	// Create source collection.
	src := db.Collection(srcName)
	insertDocs(t, src, bson.D{{Key: "x", Value: int32(1)}})

	// Create target collection that will be dropped.
	dst := db.Collection(dstName)
	insertDocs(t, dst, bson.D{{Key: "y", Value: int32(99)}})

	from := dbName + "." + srcName
	to := dbName + "." + dstName

	var res bson.D
	err := env.client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "renameCollection", Value: from},
		{Key: "to", Value: to},
		{Key: "dropTarget", Value: true},
	}).Decode(&res)
	require.NoError(t, err, "renameCollection with dropTarget:true must succeed when target exists")
	assert.Equal(t, float64(1), dmap(res)["ok"], "ok must be 1")

	// Verify the renamed collection has the source document (not the old target doc).
	var doc bson.D
	require.NoError(t, db.Collection(dstName).FindOne(ctx, bson.D{}).Decode(&doc))
	m := dmap(doc)
	_, hasX := m["x"]
	assert.True(t, hasX, "renamed collection must contain source document with field 'x'")
}

// TestServerStatus_ReplicationField verifies that serverStatus on a standalone
// server does not include a repl field (replication is not configured).
//
// Parity test for do-ncv8.
func TestServerStatus_ReplicationField(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()

	var res bson.D
	err := env.client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "serverStatus", Value: int32(1)},
	}).Decode(&res)
	require.NoError(t, err, "serverStatus must not error")
	assert.Equal(t, float64(1), dmap(res)["ok"], "ok must be 1")

	// On a standalone server, the repl field must be absent.
	// MongoDB only includes repl when the server is part of a replica set.
	_, hasRepl := dmap(res)["repl"]
	assert.False(t, hasRepl, "standalone server must not include a 'repl' field in serverStatus")
}

// TestDbStats_ScaleOption verifies that the dbStats scale parameter correctly divides all
// size fields (dataSize, storageSize, indexSize, totalSize) by the scale factor, while
// leaving document counts and avgObjSize unaffected.
//
// Parity test for do-3bws: dbStats scale option and storage metric responses diverge. (DumboDBFull)
func TestDbStats_ScaleOption(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Insert documents so that size fields are non-zero.
	for i := 0; i < 10; i++ {
		insertDocs(t, coll, bson.D{
			{Key: "idx", Value: int32(i)},
			{Key: "payload", Value: "test-data-for-sizing"},
		})
	}

	// Retrieve dbStats with default scale (1).
	var unscaled bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "dbStats", Value: int32(1)},
	}).Decode(&unscaled)
	require.NoError(t, err, "dbStats without scale must not error")
	um := dmap(unscaled)

	// Retrieve dbStats with scale=1024.
	const scale = int32(1024)
	var scaled bson.D
	err = coll.Database().RunCommand(ctx, bson.D{
		{Key: "dbStats", Value: int32(1)},
		{Key: "scale", Value: scale},
	}).Decode(&scaled)
	require.NoError(t, err, "dbStats with scale must not error")
	sm := dmap(scaled)

	assert.Equal(t, float64(1), um["ok"], "unscaled ok must be 1")
	assert.Equal(t, float64(1), sm["ok"], "scaled ok must be 1")

	// scaleFactor must reflect the requested scale.
	assert.EqualValues(t, 1, um["scaleFactor"], "default scaleFactor must be 1")
	assert.EqualValues(t, scale, sm["scaleFactor"], "scaleFactor must equal requested scale")

	// Document count must not be affected by scale.
	assert.Equal(t, um["objects"], sm["objects"], "objects must not change with scale")

	// avgObjSize must not be affected by scale.
	assert.Equal(t, um["avgObjSize"], sm["avgObjSize"], "avgObjSize must not change with scale")

	// Size fields must be divided by scale.  We verify by checking that
	// scaledValue * 1024 ~= unscaledValue (within 1.0 due to float truncation).
	for _, field := range []string{"dataSize", "storageSize", "indexSize", "totalSize"} {
		uv, ok := um[field].(float64)
		require.True(t, ok, "unscaled %s must be float64, got %T", field, um[field])
		sv, ok := sm[field].(float64)
		require.True(t, ok, "scaled %s must be float64, got %T", field, sm[field])

		assert.InDelta(t, uv, sv*float64(scale), 1.0,
			"scaled %s * %d must approximate unscaled %s (got unscaled=%.2f, scaled=%.4f)",
			field, scale, field, uv, sv)
	}
}
