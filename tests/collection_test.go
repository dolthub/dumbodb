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

// assertExplainResponse checks that the explain response contains the expected
// fields for the given verbosity level.
func assertExplainResponse(t *testing.T, res bson.D, verbosity string) {
	t.Helper()

	m := res.Map()

	assert.Equal(t, float64(1), m["ok"], "ok must be 1")
	assert.NotEmpty(t, m["queryPlanner"], "queryPlanner must be present and non-empty")
	assert.IsType(t, bson.D{}, m["queryPlanner"], "queryPlanner must be a document")
	assert.NotNil(t, m["serverInfo"], "serverInfo must be present")
	assert.NotNil(t, m["command"], "command must be present")

	switch verbosity {
	case "executionStats", "allPlansExecution":
		assert.NotNil(t, m["executionStats"], "executionStats must be present for verbosity %q", verbosity)
	default:
		// queryPlanner verbosity: executionStats may or may not be present; don't assert its absence
	}

	if verbosity == "allPlansExecution" {
		assert.NotNil(t, m["allPlansExecution"], "allPlansExecution must be present for verbosity %q", verbosity)
	}
}

// TestExplain_Find_QueryPlanner tests explain for find with queryPlanner verbosity. (DumboDBFull)
func TestExplain_Find_QueryPlanner(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{{Key: "find", Value: coll.Name()}}},
		{Key: "verbosity", Value: "queryPlanner"},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "queryPlanner")
}

// TestExplain_Find_ExecutionStats tests explain for find with executionStats verbosity. (DumboDBFull)
func TestExplain_Find_ExecutionStats(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{{Key: "find", Value: coll.Name()}}},
		{Key: "verbosity", Value: "executionStats"},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "executionStats")
}

// TestExplain_Find_AllPlansExecution tests explain for find with allPlansExecution verbosity. (DumboDBFull)
func TestExplain_Find_AllPlansExecution(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{{Key: "find", Value: coll.Name()}}},
		{Key: "verbosity", Value: "allPlansExecution"},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "allPlansExecution")
}

// TestExplain_Aggregate tests explain for aggregate command. (DumboDBFull)
func TestExplain_Aggregate(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{
			{Key: "aggregate", Value: coll.Name()},
			{Key: "pipeline", Value: bson.A{}},
			{Key: "cursor", Value: bson.D{}},
		}},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "queryPlanner")
}

// TestExplain_Count tests explain for count command. (DumboDBFull)
func TestExplain_Count(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{{Key: "count", Value: coll.Name()}}},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "queryPlanner")
}

// TestExplain_Update tests explain for update command. (DumboDBFull)
func TestExplain_Update(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{
			{Key: "update", Value: coll.Name()},
			{Key: "updates", Value: bson.A{
				bson.D{
					{Key: "q", Value: bson.D{{Key: "v", Value: int32(1)}}},
					{Key: "u", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(2)}}}}},
				},
			}},
		}},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "queryPlanner")
}

// TestExplain_Delete tests explain for delete command. (DumboDBFull)
func TestExplain_Delete(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{
			{Key: "delete", Value: coll.Name()},
			{Key: "deletes", Value: bson.A{
				bson.D{
					{Key: "q", Value: bson.D{{Key: "v", Value: int32(1)}}},
					{Key: "limit", Value: int32(0)},
				},
			}},
		}},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "queryPlanner")
}

// TestExplain_Distinct tests explain for distinct command. (DumboDBFull)
func TestExplain_Distinct(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{
			{Key: "distinct", Value: coll.Name()},
			{Key: "key", Value: "v"},
		}},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "queryPlanner")
}

// TestDB_RunCommand_DbStats verifies that the dbStats command issued via RunCommand
// returns a response document structurally compatible with MongoDB, including the
// expected field names and types (int32 for collection/index counts, float64 for sizes).
//
// Parity test for do-3bws: dbStats scale option and storage metric responses diverge. (DumboDBFull)
func TestDB_RunCommand_DbStats(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Insert documents so counts and sizes are non-zero.
	insertDocs(t, coll,
		bson.D{{Key: "x", Value: int32(1)}},
		bson.D{{Key: "x", Value: int32(2)}},
	)

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "dbStats", Value: int32(1)},
	}).Decode(&res)
	require.NoError(t, err, "dbStats via RunCommand must not error")

	m := res.Map()

	assert.Equal(t, float64(1), m["ok"], "ok must be 1")

	// db must be the database name.
	db, ok := m["db"].(string)
	require.True(t, ok, "db must be a string, got %T", m["db"])
	assert.Equal(t, coll.Database().Name(), db, "db must match collection database name")

	// collections must be int64 and >= 1.
	// MongoDB returns int64 for dbStats count fields (verified against MongoDB 7.0 via FerretDB integration tests).
	collections, ok := m["collections"].(int64)
	require.True(t, ok, "collections must be int64, got %T", m["collections"])
	assert.GreaterOrEqual(t, collections, int64(1), "collections must be >= 1")

	// views must be int64.
	_, ok = m["views"].(int64)
	assert.True(t, ok, "views must be int64, got %T", m["views"])

	// objects must reflect the inserted document count.
	assert.EqualValues(t, 2, m["objects"], "objects must equal inserted document count")

	// avgObjSize must be float64 (not scaled).
	_, ok = m["avgObjSize"].(float64)
	assert.True(t, ok, "avgObjSize must be float64, got %T", m["avgObjSize"])

	// Size fields must be float64.
	for _, field := range []string{"dataSize", "storageSize", "indexSize", "totalSize"} {
		_, ok := m[field].(float64)
		assert.True(t, ok, "%s must be float64, got %T", field, m[field])
	}

	// indexes must be int64 and >= 1 (at least the _id index).
	indexes, ok := m["indexes"].(int64)
	require.True(t, ok, "indexes must be int64, got %T", m["indexes"])
	assert.GreaterOrEqual(t, indexes, int64(1), "indexes must be >= 1")

	// scaleFactor must be int64 with default value 1.
	scaleFactor, ok := m["scaleFactor"].(int64)
	require.True(t, ok, "scaleFactor must be int64, got %T", m["scaleFactor"])
	assert.EqualValues(t, 1, scaleFactor, "default scaleFactor must be 1")

	// Filesystem size fields must be float64.
	_, ok = m["fsUsedSize"].(float64)
	assert.True(t, ok, "fsUsedSize must be float64, got %T", m["fsUsedSize"])
	_, ok = m["fsTotalSize"].(float64)
	assert.True(t, ok, "fsTotalSize must be float64, got %T", m["fsTotalSize"])
}

// TestDB_RunCommand_CollStats verifies that the collStats command issued via RunCommand
// returns a response document structurally compatible with MongoDB, including all
// required fields with correct types, and that storageSize >= size (storage allocation
// is always at least as large as logical data size).
//
// Parity test for do-3bws: dbStats scale option and storage metric responses diverge. (DumboDBFull)
func TestDB_RunCommand_CollStats(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Insert documents so count and size fields are non-zero.
	insertDocs(t, coll,
		bson.D{{Key: "x", Value: int32(1)}},
		bson.D{{Key: "x", Value: int32(2)}},
	)

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "collStats", Value: coll.Name()},
	}).Decode(&res)
	require.NoError(t, err, "collStats via RunCommand must not error")

	m := res.Map()

	assert.Equal(t, float64(1), m["ok"], "ok must be 1")

	// ns must be db.collection.
	ns, ok := m["ns"].(string)
	require.True(t, ok, "ns must be a string, got %T", m["ns"])
	assert.Equal(t, coll.Database().Name()+"."+coll.Name(), ns, "ns must be db.collection")

	// size must be int32.
	size, ok := m["size"].(int32)
	require.True(t, ok, "size must be int32, got %T", m["size"])

	// count must be int32 reflecting the inserted document count.
	count, ok := m["count"].(int32)
	require.True(t, ok, "count must be int32, got %T", m["count"])
	assert.EqualValues(t, 2, count, "count must equal inserted document count")

	// avgObjSize must be int32 (present when count > 0).
	_, ok = m["avgObjSize"].(int32)
	assert.True(t, ok, "avgObjSize must be int32, got %T", m["avgObjSize"])

	// numOrphanDocs must be int32.
	_, ok = m["numOrphanDocs"].(int32)
	assert.True(t, ok, "numOrphanDocs must be int32, got %T", m["numOrphanDocs"])

	// storageSize must be int32 and >= size (storage allocation >= logical data size).
	storageSize, ok := m["storageSize"].(int32)
	require.True(t, ok, "storageSize must be int32, got %T", m["storageSize"])
	assert.GreaterOrEqual(t, storageSize, size,
		"storageSize (%d) must be >= size (%d)", storageSize, size)

	// capped must be bool.
	_, ok = m["capped"].(bool)
	assert.True(t, ok, "capped must be bool, got %T", m["capped"])

	// nindexes must be int32 and >= 1 (at least the _id index).
	nindexes, ok := m["nindexes"].(int32)
	require.True(t, ok, "nindexes must be int32, got %T", m["nindexes"])
	assert.GreaterOrEqual(t, nindexes, int32(1), "nindexes must be >= 1")

	// indexDetails must be a document.
	_, ok = m["indexDetails"].(bson.D)
	assert.True(t, ok, "indexDetails must be a document, got %T", m["indexDetails"])

	// indexBuilds must be an array.
	_, ok = m["indexBuilds"].(bson.A)
	assert.True(t, ok, "indexBuilds must be an array, got %T", m["indexBuilds"])

	// totalIndexSize must be int32.
	_, ok = m["totalIndexSize"].(int32)
	assert.True(t, ok, "totalIndexSize must be int32, got %T", m["totalIndexSize"])

	// indexSizes must be a document.
	_, ok = m["indexSizes"].(bson.D)
	assert.True(t, ok, "indexSizes must be a document, got %T", m["indexSizes"])

	// totalSize must be int32 and > 0.
	totalSize, ok := m["totalSize"].(int32)
	require.True(t, ok, "totalSize must be int32, got %T", m["totalSize"])
	assert.Greater(t, totalSize, int32(0), "totalSize must be > 0")

	// scaleFactor must be int32 with default value 1.
	scaleFactor, ok := m["scaleFactor"].(int32)
	require.True(t, ok, "scaleFactor must be int32, got %T", m["scaleFactor"])
	assert.EqualValues(t, 1, scaleFactor, "default scaleFactor must be 1")
}

// TestDB_RunCommand_ListCollections verifies that listCollections issued via
// RunCommand returns a proper cursor response document — not a raw array —
// matching the MongoDB wire protocol:
//
//	{cursor: {id: 0, ns: "<db>.$cmd.listCollections", firstBatch: [...]}, ok: 1}
//
// Regression for do-3n5p: the cursor wrapper and/or the ns field were missing
// or incorrectly formatted when listCollections was invoked via RunCommand. (DumboDBFull)
func TestDB_RunCommand_ListCollections(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Insert a document so the collection exists.
	insertDocs(t, coll, bson.D{{Key: "x", Value: int32(1)}})

	dbName := coll.Database().Name()
	expectedNS := dbName + ".$cmd.listCollections"

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "listCollections", Value: int32(1)},
	}).Decode(&res)
	require.NoError(t, err, "listCollections via RunCommand must not error")

	m := res.Map()

	// Top-level ok must be 1.
	assert.Equal(t, float64(1), m["ok"], "ok must be 1")

	// cursor must be present and be a document.
	cursorRaw, ok := m["cursor"]
	require.True(t, ok, "response must contain a 'cursor' field")
	cursor, ok := cursorRaw.(bson.D)
	require.True(t, ok, "cursor must be a document (bson.D), got %T", cursorRaw)

	cm := cursor.Map()

	// cursor.id must be 0 (no server-side cursor for listCollections).
	assert.EqualValues(t, int64(0), cm["id"], "cursor.id must be 0")

	// cursor.ns must match <db>.$cmd.listCollections.
	assert.Equal(t, expectedNS, cm["ns"], "cursor.ns must be %q", expectedNS)

	// cursor.firstBatch must be present and contain the collection we created.
	firstBatchRaw, ok := cm["firstBatch"]
	require.True(t, ok, "cursor must contain 'firstBatch'")
	firstBatch, ok := firstBatchRaw.(bson.A)
	require.True(t, ok, "cursor.firstBatch must be an array (bson.A), got %T", firstBatchRaw)

	// Find the collection entry by name.
	found := false
	for _, item := range firstBatch {
		entry, ok := item.(bson.D)
		if !ok {
			continue
		}
		if entry.Map()["name"] == coll.Name() {
			found = true
			break
		}
	}
	assert.True(t, found, "firstBatch must contain an entry for collection %q", coll.Name())
}

// TestDB_RunCommand_BuildInfo verifies that the buildInfo command returns
// version strings and fields structurally compatible with MongoDB, including
// allocator, javascriptEngine, openssl, and storageEngines fields.
//
// Regression for do-87bd: missing fields and wrong version (DumboDBFull)
func TestDB_RunCommand_BuildInfo(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "buildInfo", Value: int32(1)},
	}).Decode(&res)
	require.NoError(t, err, "buildInfo must not error")

	m := res.Map()

	// ok must be 1.
	assert.Equal(t, float64(1), m["ok"], "ok must be 1")

	// version must be a non-empty string matching major.minor.patch format.
	version, ok := m["version"].(string)
	require.True(t, ok, "version must be a string, got %T", m["version"])
	assert.Regexp(t, `^\d+\.\d+\.\d+$`, version, "version must match major.minor.patch")

	// versionArray must be an array of 4 int32 elements.
	versionArray, ok := m["versionArray"].(bson.A)
	require.True(t, ok, "versionArray must be an array, got %T", m["versionArray"])
	assert.Len(t, versionArray, 4, "versionArray must have 4 elements")
	for i, elem := range versionArray {
		assert.IsType(t, int32(0), elem, "versionArray[%d] must be int32", i)
	}

	// gitVersion must be a non-empty string.
	gitVersion, ok := m["gitVersion"].(string)
	require.True(t, ok, "gitVersion must be a string, got %T", m["gitVersion"])
	assert.NotEmpty(t, gitVersion, "gitVersion must not be empty")

	// bits must be int32.
	assert.IsType(t, int32(0), m["bits"], "bits must be int32")

	// debug must be bool.
	assert.IsType(t, false, m["debug"], "debug must be bool")

	// allocator must be a non-empty string.
	allocator, ok := m["allocator"].(string)
	require.True(t, ok, "allocator must be a string, got %T", m["allocator"])
	assert.NotEmpty(t, allocator, "allocator must not be empty")

	// javascriptEngine must be a non-empty string.
	jsEngine, ok := m["javascriptEngine"].(string)
	require.True(t, ok, "javascriptEngine must be a string, got %T", m["javascriptEngine"])
	assert.NotEmpty(t, jsEngine, "javascriptEngine must not be empty")

	// openssl must be a document with compiled and running fields.
	opensslRaw, ok := m["openssl"]
	require.True(t, ok, "openssl must be present in buildInfo response")
	openssl, ok := opensslRaw.(bson.D)
	require.True(t, ok, "openssl must be a document, got %T", opensslRaw)
	opensslMap := openssl.Map()
	_, ok = opensslMap["compiled"].(string)
	assert.True(t, ok, "openssl.compiled must be a string, got %T", opensslMap["compiled"])
	_, ok = opensslMap["running"].(string)
	assert.True(t, ok, "openssl.running must be a string, got %T", opensslMap["running"])

	// storageEngines must be present and be a non-empty array.
	storageEnginesRaw, ok := m["storageEngines"]
	require.True(t, ok, "storageEngines must be present in buildInfo response")
	storageEngines, ok := storageEnginesRaw.(bson.A)
	require.True(t, ok, "storageEngines must be an array, got %T", storageEnginesRaw)
	assert.NotEmpty(t, storageEngines, "storageEngines must not be empty")
	for i, engine := range storageEngines {
		assert.IsType(t, "", engine, "storageEngines[%d] must be a string", i)
	}
}

// TestDB_RunCommand_Validate verifies that the validate command issued via RunCommand
// returns a collection integrity report matching MongoDB's response format:
// valid, nrecords, nIndexes, keysPerIndex, repaired, and related arrays.
//
// Parity test for do-h7a9: validate command options and RunCommand response. (DumboDBFull)
func TestDB_RunCommand_Validate(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Insert two documents so nrecords is non-zero.
	insertDocs(t, coll,
		bson.D{{Key: "x", Value: int32(1)}},
		bson.D{{Key: "x", Value: int32(2)}},
	)

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "validate", Value: coll.Name()},
	}).Decode(&res)
	require.NoError(t, err, "validate via RunCommand must not error")

	m := res.Map()

	// ok must be 1.
	assert.Equal(t, float64(1), m["ok"], "ok must be 1")

	// valid must be true for a healthy collection.
	assert.Equal(t, true, m["valid"], "valid must be true")

	// ns must match the collection namespace.
	ns, ok := m["ns"].(string)
	require.True(t, ok, "ns must be a string, got %T", m["ns"])
	assert.Equal(t, coll.Database().Name()+"."+coll.Name(), ns, "ns must be db.collection")

	// nrecords must be a positive int32.
	nrecords, ok := m["nrecords"].(int32)
	require.True(t, ok, "nrecords must be int32, got %T", m["nrecords"])
	assert.EqualValues(t, 2, nrecords, "nrecords must reflect inserted document count")

	// nIndexes must be at least 1 (the _id index).
	nIndexes, ok := m["nIndexes"].(int32)
	require.True(t, ok, "nIndexes must be int32, got %T", m["nIndexes"])
	assert.GreaterOrEqual(t, nIndexes, int32(1), "nIndexes must be >= 1")

	// keysPerIndex must be a document with at least one entry.
	keysPerIndex, ok := m["keysPerIndex"].(bson.D)
	require.True(t, ok, "keysPerIndex must be a document, got %T", m["keysPerIndex"])
	assert.NotEmpty(t, keysPerIndex, "keysPerIndex must have at least one entry")

	// indexDetails must be a document.
	_, ok = m["indexDetails"].(bson.D)
	assert.True(t, ok, "indexDetails must be a document, got %T", m["indexDetails"])

	// repaired must be false (no repairs needed for a fresh collection).
	assert.Equal(t, false, m["repaired"], "repaired must be false for a healthy collection")

	// warnings, errors, extraIndexEntries, missingIndexEntries, corruptRecords must be arrays.
	for _, field := range []string{"warnings", "errors", "extraIndexEntries", "missingIndexEntries", "corruptRecords"} {
		_, ok := m[field].(bson.A)
		assert.True(t, ok, "%s must be an array, got %T", field, m[field])
	}
}

// TestDB_ListDatabases verifies that the listDatabases command returns a
// response structure compatible with MongoDB, including the admin system
// database and correct field types for each database entry.
//
// Regression for do-27zw: listDatabases result structure diverges from MongoDB (DumboDBFull)
// Regression for do-ma7c: listDatabases crashes dumbodb connection with EOF
func TestDB_ListDatabases(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Insert a document so the user database (testdb) exists and is non-empty.
	insertDocs(t, coll, bson.D{{Key: "x", Value: int32(1)}})

	dbName := coll.Database().Name()

	// Issue listDatabases via RunCommand against the admin database (the
	// canonical target for admin commands in MongoDB).
	var res bson.D
	err := env.client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "listDatabases", Value: int32(1)},
	}).Decode(&res)
	require.NoError(t, err, "listDatabases via RunCommand must not error")

	m := res.Map()

	// Top-level ok must be 1 (float64).
	assert.Equal(t, float64(1), m["ok"], "ok must be 1")

	// totalSize and totalSizeMb must be present and int64.
	totalSizeRaw, ok := m["totalSize"]
	require.True(t, ok, "response must contain 'totalSize'")
	assert.IsType(t, int64(0), totalSizeRaw, "totalSize must be int64")

	totalSizeMbRaw, ok := m["totalSizeMb"]
	require.True(t, ok, "response must contain 'totalSizeMb'")
	assert.IsType(t, int64(0), totalSizeMbRaw, "totalSizeMb must be int64")

	// databases must be a non-empty array.
	databasesRaw, ok := m["databases"]
	require.True(t, ok, "response must contain 'databases'")
	databases, ok := databasesRaw.(bson.A)
	require.True(t, ok, "databases must be an array (bson.A), got %T", databasesRaw)
	assert.NotEmpty(t, databases, "databases array must not be empty")

	// Collect database names and verify each entry's field types.
	dbNames := make(map[string]bool)
	for i, item := range databases {
		entry, ok := item.(bson.D)
		require.True(t, ok, "databases[%d] must be a document (bson.D), got %T", i, item)

		em := entry.Map()

		name, ok := em["name"].(string)
		require.True(t, ok, "databases[%d].name must be a string, got %T", i, em["name"])
		dbNames[name] = true

		assert.IsType(t, int64(0), em["sizeOnDisk"],
			"databases[%d].sizeOnDisk (%q) must be int64, got %T", i, name, em["sizeOnDisk"])
		assert.IsType(t, false, em["empty"],
			"databases[%d].empty (%q) must be bool, got %T", i, name, em["empty"])
	}

	// The user database must appear.
	assert.True(t, dbNames[dbName], "databases must include user database %q", dbName)

	// The admin system database must appear, matching MongoDB's behavior where
	// admin is always present in listDatabases regardless of whether it has
	// user collections.
	assert.True(t, dbNames["admin"], "databases must include the 'admin' system database")

	// Regression for do-ma7c: verify the connection is still alive after listDatabases.
	// Before the fix, the handler could crash the TCP connection (EOF) during listDatabases,
	// leaving the driver unable to issue subsequent commands.
	var pingRes bson.D
	err = env.client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "ping", Value: int32(1)},
	}).Decode(&pingRes)
	require.NoError(t, err, "connection must remain alive after listDatabases")
}

// TestDB_RunCommand_Hello verifies that the hello command returns the expected
// topology and capability fields matching MongoDB's response format.
// Parity test: hello command via RunCommand must include isWritablePrimary,
// wire version bounds, session timeout, and ok=1. (DumboDBFull)
func TestDB_RunCommand_Hello(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "hello", Value: int32(1)},
	}).Decode(&res)
	require.NoError(t, err, "hello via RunCommand must not error")

	m := res.Map()

	// ok must be 1.
	assert.Equal(t, float64(1), m["ok"], "ok must be 1")

	// isWritablePrimary must be true (standalone/primary node).
	assert.Equal(t, true, m["isWritablePrimary"], "isWritablePrimary must be true")

	// readOnly must be false.
	assert.Equal(t, false, m["readOnly"], "readOnly must be false")

	// maxBsonObjectSize must be int32.
	assert.IsType(t, int32(0), m["maxBsonObjectSize"], "maxBsonObjectSize must be int32")

	// maxMessageSizeBytes must be int32.
	assert.IsType(t, int32(0), m["maxMessageSizeBytes"], "maxMessageSizeBytes must be int32")

	// maxWriteBatchSize must be int32.
	assert.IsType(t, int32(0), m["maxWriteBatchSize"], "maxWriteBatchSize must be int32")

	// minWireVersion and maxWireVersion must be int32, with maxWireVersion > minWireVersion.
	minWireVersion, ok := m["minWireVersion"].(int32)
	require.True(t, ok, "minWireVersion must be int32, got %T", m["minWireVersion"])
	maxWireVersion, ok := m["maxWireVersion"].(int32)
	require.True(t, ok, "maxWireVersion must be int32, got %T", m["maxWireVersion"])
	assert.GreaterOrEqual(t, maxWireVersion, minWireVersion, "maxWireVersion must be >= minWireVersion")

	// logicalSessionTimeoutMinutes must be int32.
	assert.IsType(t, int32(0), m["logicalSessionTimeoutMinutes"], "logicalSessionTimeoutMinutes must be int32")

	// connectionId must be int32.
	assert.IsType(t, int32(0), m["connectionId"], "connectionId must be int32")

	// localTime must be present (primitive.DateTime is an int64 alias decoded from BSON UTC datetime).
	_, ok = m["localTime"]
	assert.True(t, ok, "localTime must be present in hello response")
}

// TestDB_RunCommand_IsMaster verifies that the deprecated isMaster command
// returns the expected topology fields matching MongoDB's response format.
// Parity test: isMaster command via RunCommand must include ismaster=true,
// wire version bounds, session timeout, and ok=1. (DumboDBFull)
func TestDB_RunCommand_IsMaster(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "isMaster", Value: int32(1)},
	}).Decode(&res)
	require.NoError(t, err, "isMaster via RunCommand must not error")

	m := res.Map()

	// ok must be 1.
	assert.Equal(t, float64(1), m["ok"], "ok must be 1")

	// isMaster uses the legacy "ismaster" field (lowercase), not "isWritablePrimary".
	assert.Equal(t, true, m["ismaster"], "ismaster must be true")

	// readOnly must be false.
	assert.Equal(t, false, m["readOnly"], "readOnly must be false")

	// maxBsonObjectSize must be int32.
	assert.IsType(t, int32(0), m["maxBsonObjectSize"], "maxBsonObjectSize must be int32")

	// maxMessageSizeBytes must be int32.
	assert.IsType(t, int32(0), m["maxMessageSizeBytes"], "maxMessageSizeBytes must be int32")

	// maxWriteBatchSize must be int32.
	assert.IsType(t, int32(0), m["maxWriteBatchSize"], "maxWriteBatchSize must be int32")

	// minWireVersion and maxWireVersion must be int32.
	minWireVersion, ok := m["minWireVersion"].(int32)
	require.True(t, ok, "minWireVersion must be int32, got %T", m["minWireVersion"])
	maxWireVersion, ok := m["maxWireVersion"].(int32)
	require.True(t, ok, "maxWireVersion must be int32, got %T", m["maxWireVersion"])
	assert.GreaterOrEqual(t, maxWireVersion, minWireVersion, "maxWireVersion must be >= minWireVersion")

	// logicalSessionTimeoutMinutes must be int32.
	assert.IsType(t, int32(0), m["logicalSessionTimeoutMinutes"], "logicalSessionTimeoutMinutes must be int32")

	// connectionId must be int32.
	assert.IsType(t, int32(0), m["connectionId"], "connectionId must be int32")

	// localTime must be present.
	_, ok = m["localTime"]
	assert.True(t, ok, "localTime must be present in isMaster response")
}

// TestDB_RunCommand_ServerStatus verifies that the serverStatus command returns
// process identity fields and timing metrics matching MongoDB's response format.
// Parity test: serverStatus must include host, version, process, pid, uptime
// variants, localTime, and ok=1. (DumboDBFull)
func TestDB_RunCommand_ServerStatus(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "serverStatus", Value: int32(1)},
	}).Decode(&res)
	require.NoError(t, err, "serverStatus via RunCommand must not error")

	m := res.Map()

	// ok must be 1.
	assert.Equal(t, float64(1), m["ok"], "ok must be 1")

	// host must be a non-empty string.
	host, ok := m["host"].(string)
	require.True(t, ok, "host must be a string, got %T", m["host"])
	assert.NotEmpty(t, host, "host must not be empty")

	// version must be a non-empty string.
	version, ok := m["version"].(string)
	require.True(t, ok, "version must be a string, got %T", m["version"])
	assert.NotEmpty(t, version, "version must not be empty")

	// process must be a non-empty string.
	process, ok := m["process"].(string)
	require.True(t, ok, "process must be a string, got %T", m["process"])
	assert.NotEmpty(t, process, "process must not be empty")

	// pid must be int64.
	assert.IsType(t, int64(0), m["pid"], "pid must be int64")

	// uptime must be float64 and non-negative.
	uptime, ok := m["uptime"].(float64)
	require.True(t, ok, "uptime must be float64, got %T", m["uptime"])
	assert.GreaterOrEqual(t, uptime, float64(0), "uptime must be non-negative")

	// uptimeMillis must be int64.
	assert.IsType(t, int64(0), m["uptimeMillis"], "uptimeMillis must be int64")

	// uptimeEstimate must be int64.
	assert.IsType(t, int64(0), m["uptimeEstimate"], "uptimeEstimate must be int64")

	// localTime must be present.
	_, ok = m["localTime"]
	assert.True(t, ok, "localTime must be present in serverStatus response")
}
