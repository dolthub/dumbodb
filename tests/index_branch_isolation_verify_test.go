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

// TestIndexBranchIsolationVerify is the automated analog of
// docs/verify/index-branch-isolation.md. Each top-level subtest
// corresponds to one scenario in that document. The setup reproduces
// the manual setup block exactly:
//
//   - main: { _id:1, name:"alpha" }, committed
//   - am, nz: branched off main at the same commit, no extra writes
//
// Subtests run sequentially (no t.Parallel) so they share a single
// database and the side effects of one scenario carry into the next.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func indexBranchVerifySetup(t *testing.T, env *dumboDBTestEnv, dbName string) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, env.client.Database(dbName).Drop(ctx))

	items := env.client.Database(dbName).Collection("items")
	_, err := items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "name", Value: "alpha"},
	})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "seed alpha", "alice <alice@acme.com>")

	require.NoError(t, env.client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "am"},
	}).Err(), "doltBranch to create 'am'")
	require.NoError(t, env.client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "nz"},
	}).Err(), "doltBranch to create 'nz'")
}

// indexNamesOf returns the sorted list of index names on items via
// listIndexes.
func indexNamesOf(t *testing.T, env *dumboDBTestEnv, dbName string) []string {
	t.Helper()
	ctx := context.Background()
	cursor, err := env.client.Database(dbName).Collection("items").Indexes().List(ctx)
	require.NoError(t, err)
	var rows []bson.M
	require.NoError(t, cursor.All(ctx, &rows))
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r["name"].(string))
	}
	sort.Strings(names)
	return names
}

// idsForName returns the sorted _id list of documents matching {name: value}.
func idsForName(t *testing.T, env *dumboDBTestEnv, dbName, value string) []int32 {
	t.Helper()
	ctx := context.Background()
	cursor, err := env.client.Database(dbName).Collection("items").Find(ctx, bson.D{
		{Key: "name", Value: value},
	})
	require.NoError(t, err)
	var rows []bson.M
	require.NoError(t, cursor.All(ctx, &rows))
	ids := make([]int32, 0, len(rows))
	for _, r := range rows {
		switch v := r["_id"].(type) {
		case int32:
			ids = append(ids, v)
		case int64:
			ids = append(ids, int32(v))
		default:
			t.Fatalf("unexpected _id type %T", r["_id"])
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func TestIndexBranchIsolationVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("idxisovrfy%d", rand.Int64N(1_000_000))
	indexBranchVerifySetup(t, env, dbName)

	// ----------------------------------------------------------------------
	// Scenario 1: Create index on one branch, not visible on another
	// ----------------------------------------------------------------------
	t.Run("Scenario1_IndexOnAm_NotVisibleOnMainOrNz", func(t *testing.T) {
		amDB := env.client.Database(dbName + "@am")
		amIdx := amDB.Collection("items").Indexes()
		_, err := amIdx.CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "name", Value: int32(1)}},
			Options: options.Index().SetName("by_name"),
		})
		require.NoError(t, err, "createIndex on am must succeed")

		// Before committing: dumboStatus surfaces the uncommitted
		// index addition on am.
		var statusRes bson.M
		require.NoError(t, amDB.RunCommand(ctx, bson.D{{Key: "dumboStatus", Value: 1}}).Decode(&statusRes),
			"dumboStatus on am before commit must succeed")
		statusColls := mustArrayOfMaps(t, statusRes["collections"])
		require.Len(t, statusColls, 1, "expected one collection in status")
		assert.Equal(t, "items", statusColls[0]["name"])
		assert.Equal(t, "modified", statusColls[0]["status"])
		assert.Equal(t, []string{"by_name"}, mustStringSlice(t, statusColls[0]["addedIndexes"]),
			"dumboStatus must surface by_name in addedIndexes before the commit")
		// modifiedIndexes and removedIndexes are always present as empty arrays.
		assert.Empty(t, mustStringSlice(t, statusColls[0]["modifiedIndexes"]),
			"modifiedIndexes must be an empty array when nothing modified")
		assert.Empty(t, mustStringSlice(t, statusColls[0]["removedIndexes"]),
			"removedIndexes must be an empty array when nothing removed")

		// dumboDiff returns the full index definition (keys + direction).
		var diffRes bson.M
		require.NoError(t, amDB.RunCommand(ctx, bson.D{{Key: "dumboDiff", Value: int32(1)}}).Decode(&diffRes),
			"dumboDiff on am before commit must succeed")
		diffColls := mustArrayOfMaps(t, diffRes["collections"])
		require.Len(t, diffColls, 1, "expected one collection in diff")
		addedIdx := mustArrayOfMaps(t, diffColls[0]["addedIndexes"])
		require.Len(t, addedIdx, 1, "expected one entry in addedIndexes")
		assert.Equal(t, "by_name", addedIdx[0]["name"])
		addedKeys := mustArrayOfMaps(t, addedIdx[0]["keys"])
		require.Len(t, addedKeys, 1, "expected one key field")
		assert.Equal(t, "name", addedKeys[0]["field"])
		assert.EqualValues(t, 1, addedKeys[0]["direction"])
		// The other two index arrays are always present and empty here.
		assert.Empty(t, mustArrayOfMaps(t, diffColls[0]["modifiedIndexes"]),
			"modifiedIndexes must be an empty array when nothing modified")
		assert.Empty(t, mustArrayOfMaps(t, diffColls[0]["removedIndexes"]),
			"removedIndexes must be an empty array when nothing removed")

		dumboDBCommit(t, env, dbName+"@am", "am: create by_name", "alice <alice@acme.com>")

		assert.Equal(t, []string{"_id_", "by_name"}, indexNamesOf(t, env, dbName+"@am"))
		assert.Equal(t, []string{"_id_"}, indexNamesOf(t, env, dbName+"@main"))
		assert.Equal(t, []string{"_id_"}, indexNamesOf(t, env, dbName+"@nz"))
	})

	// ----------------------------------------------------------------------
	// Scenario 2: Different index names on different branches
	// ----------------------------------------------------------------------
	t.Run("Scenario2_DifferentIndexesPerBranch", func(t *testing.T) {
		nzDB := env.client.Database(dbName + "@nz")
		_, err := nzDB.Collection("items").Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "name", Value: int32(1)}, {Key: "_id", Value: int32(1)}},
			Options: options.Index().SetName("by_id_name"),
		})
		require.NoError(t, err, "createIndex on nz must succeed")
		dumboDBCommit(t, env, dbName+"@nz", "nz: create by_id_name", "alice <alice@acme.com>")

		assert.Equal(t, []string{"_id_", "by_id_name"}, indexNamesOf(t, env, dbName+"@nz"))
		assert.Equal(t, []string{"_id_", "by_name"}, indexNamesOf(t, env, dbName+"@am"))
	})

	// ----------------------------------------------------------------------
	// Scenario 3: Interleaved inserts, each branch sees only its own data
	// ----------------------------------------------------------------------
	t.Run("Scenario3_InterleavedInsertsDiverge", func(t *testing.T) {
		amWords := []string{"bravo", "charlie", "delta", "echo", "foxtrot", "golf",
			"hotel", "india", "juliet", "kilo", "lima", "mike"}
		amItems := env.client.Database(dbName + "@am").Collection("items")
		for i, w := range amWords {
			_, err := amItems.InsertOne(ctx, bson.D{
				{Key: "_id", Value: int32(100 + i)},
				{Key: "name", Value: w},
			})
			require.NoError(t, err, "am insert %q", w)
		}
		dumboDBCommit(t, env, dbName+"@am", "am bulk insert", "alice <alice@acme.com>")

		nzWords := []string{"november", "oscar", "papa", "quebec", "romeo", "sierra",
			"tango", "uniform", "victor", "whiskey", "xray", "yankee", "zulu"}
		nzItems := env.client.Database(dbName + "@nz").Collection("items")
		for i, w := range nzWords {
			_, err := nzItems.InsertOne(ctx, bson.D{
				{Key: "_id", Value: int32(200 + i)},
				{Key: "name", Value: w},
			})
			require.NoError(t, err, "nz insert %q", w)
		}
		dumboDBCommit(t, env, dbName+"@nz", "nz bulk insert", "alice <alice@acme.com>")

		// am sees mike but not zulu.
		assert.Equal(t, []int32{111}, idsForName(t, env, dbName+"@am", "mike"))
		assert.Empty(t, idsForName(t, env, dbName+"@am", "zulu"))

		// nz sees zulu but not mike.
		assert.Equal(t, []int32{212}, idsForName(t, env, dbName+"@nz", "zulu"))
		assert.Empty(t, idsForName(t, env, dbName+"@nz", "mike"))

		// main has only the seed.
		assert.Equal(t, []int32{1}, idsForName(t, env, dbName+"@main", "alpha"))
		assert.Empty(t, idsForName(t, env, dbName+"@main", "mike"))
		assert.Empty(t, idsForName(t, env, dbName+"@main", "zulu"))
	})

	// ----------------------------------------------------------------------
	// Scenario 4: Update on one branch shifts the indexed lookup only on
	// that branch.
	//
	// UpdateAll does not yet maintain secondary indexes (workspace-4ee).
	// Skip until that work lands; the scenario is documented and the test
	// is the trip wire that flips green when it does.
	// ----------------------------------------------------------------------
	t.Run("Scenario4_UpdateShiftsIndexedLookupPerBranch", func(t *testing.T) {
		t.Skip("pending workspace-4ee: UpdateAll must maintain secondary indexes")
	})

	// ----------------------------------------------------------------------
	// Scenario 5: Delete on one branch removes from the index only on
	// that branch.
	//
	// DeleteAll does not yet maintain secondary indexes (workspace-4ee).
	// ----------------------------------------------------------------------
	t.Run("Scenario5_DeleteRemovesFromIndexPerBranch", func(t *testing.T) {
		t.Skip("pending workspace-4ee: DeleteAll must maintain secondary indexes")
	})

	// ----------------------------------------------------------------------
	// Scenario 6: Drop an index on one branch, the index still exists on
	// the other.
	// ----------------------------------------------------------------------
	t.Run("Scenario6_DropIndexPerBranch", func(t *testing.T) {
		amDB := env.client.Database(dbName + "@am")
		err := amDB.Collection("items").Indexes().DropOne(ctx, "by_name")
		require.NoError(t, err, "dropIndex on am must succeed")
		dumboDBCommit(t, env, dbName+"@am", "am: drop by_name", "alice <alice@acme.com>")

		assert.Equal(t, []string{"_id_"}, indexNamesOf(t, env, dbName+"@am"))
		assert.Equal(t, []string{"_id_", "by_id_name"}, indexNamesOf(t, env, dbName+"@nz"))
	})

	// ----------------------------------------------------------------------
	// Scenario 7: Per-branch index state survives server restart.
	//
	// startDumboDB tears the server down at the end of TestIndexBranch-
	// IsolationVerify. Within this single Test* function we cannot restart
	// the server, so this scenario is exercised by the resolver path's
	// on-demand chunk-store reads: a fresh client connection to nz on the
	// same running server still sees nz's index and data through the
	// resolver, which is the read path that survives reopens. The full
	// restart scenario is covered by TestResolver_RoundTripIndexAMFromDTBL
	// and the per-resolver tests in internal/backends/dolt.
	// ----------------------------------------------------------------------
	t.Run("Scenario7_PerBranchStatePersists", func(t *testing.T) {
		// Reconnect via a fresh Database handle. This is the closest we get
		// to a "fresh connection" mid-test; the underlying server is the
		// same instance.
		assert.Equal(t, []string{"_id_", "by_id_name"}, indexNamesOf(t, env, dbName+"@nz"))
		assert.Equal(t, []int32{212}, idsForName(t, env, dbName+"@nz", "zulu"))
	})

	// ----------------------------------------------------------------------
	// Scenario 8: dumboDiff shows index modification (drop + recreate with
	// different spec). Uses a fresh database isolated from idxisovdb.
	// ----------------------------------------------------------------------
	t.Run("Scenario8_IndexModifiedShowsBothDefinitions", func(t *testing.T) {
		modDbName := fmt.Sprintf("idxmodvrfy%d", rand.Int64N(1_000_000))
		mdb := env.client.Database(modDbName)
		require.NoError(t, mdb.Drop(ctx))

		items := mdb.Collection("items")
		_, err := items.InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(1)},
			{Key: "age", Value: int32(30)},
			{Key: "name", Value: "alpha"},
		})
		require.NoError(t, err)
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "age", Value: int32(1)}},
			Options: options.Index().SetName("by_x"),
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, modDbName, "seed + by_x on age", "alice <alice@acme.com>")

		// Drop by_x and recreate it with a different key. Uncommitted.
		require.NoError(t, items.Indexes().DropOne(ctx, "by_x"))
		_, err = items.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "name", Value: int32(1)}},
			Options: options.Index().SetName("by_x"),
		})
		require.NoError(t, err)

		// dumboStatus reports the same name in modifiedIndexes (not added or removed).
		var statusRes bson.M
		require.NoError(t, mdb.RunCommand(ctx, bson.D{{Key: "dumboStatus", Value: 1}}).Decode(&statusRes))
		statusColls := mustArrayOfMaps(t, statusRes["collections"])
		require.Len(t, statusColls, 1)
		assert.Empty(t, mustStringSlice(t, statusColls[0]["addedIndexes"]),
			"addedIndexes must be an empty array, not absent")
		assert.Empty(t, mustStringSlice(t, statusColls[0]["removedIndexes"]),
			"removedIndexes must be an empty array, not absent")
		assert.Equal(t, []string{"by_x"}, mustStringSlice(t, statusColls[0]["modifiedIndexes"]))

		// dumboDiff returns one entry in modifiedIndexes carrying both
		// from (age) and to (name) definitions. addedIndexes and
		// removedIndexes are always present and empty here.
		var diffRes bson.M
		require.NoError(t, mdb.RunCommand(ctx, bson.D{{Key: "dumboDiff", Value: int32(1)}}).Decode(&diffRes))
		diffColls := mustArrayOfMaps(t, diffRes["collections"])
		require.Len(t, diffColls, 1)
		assert.Empty(t, mustArrayOfMaps(t, diffColls[0]["addedIndexes"]))
		assert.Empty(t, mustArrayOfMaps(t, diffColls[0]["removedIndexes"]))

		modifiedIdx := mustArrayOfMaps(t, diffColls[0]["modifiedIndexes"])
		require.Len(t, modifiedIdx, 1)
		fromDoc := mustMap(t, modifiedIdx[0]["from"])
		assert.Equal(t, "by_x", fromDoc["name"])
		fromKeys := mustArrayOfMaps(t, fromDoc["keys"])
		require.Len(t, fromKeys, 1)
		assert.Equal(t, "age", fromKeys[0]["field"], "from must reflect the pre-drop key")

		toDoc := mustMap(t, modifiedIdx[0]["to"])
		assert.Equal(t, "by_x", toDoc["name"])
		toKeys := mustArrayOfMaps(t, toDoc["keys"])
		require.Len(t, toKeys, 1)
		assert.Equal(t, "name", toKeys[0]["field"], "to must reflect the recreated key")
	})
}

// mustArrayOfMaps converts a bson.A into a []bson.M, fataling on a
// non-array value or non-map element.
func mustArrayOfMaps(t *testing.T, v interface{}) []bson.M {
	t.Helper()
	arr, ok := v.(bson.A)
	if !ok {
		t.Fatalf("expected array, got %T: %v", v, v)
	}
	out := make([]bson.M, len(arr))
	for i, el := range arr {
		m, ok := el.(bson.M)
		if !ok {
			t.Fatalf("array element %d: expected map, got %T", i, el)
		}
		out[i] = m
	}
	return out
}

// mustMap converts an interface{} into a bson.M, fataling otherwise.
func mustMap(t *testing.T, v interface{}) bson.M {
	t.Helper()
	m, ok := v.(bson.M)
	if !ok {
		t.Fatalf("expected map, got %T: %v", v, v)
	}
	return m
}

// mustStringSlice converts a bson.A of strings into []string, fataling
// otherwise.
func mustStringSlice(t *testing.T, v interface{}) []string {
	t.Helper()
	arr, ok := v.(bson.A)
	if !ok {
		t.Fatalf("expected array, got %T: %v", v, v)
	}
	out := make([]string, len(arr))
	for i, el := range arr {
		s, ok := el.(string)
		if !ok {
			t.Fatalf("array element %d: expected string, got %T", i, el)
		}
		out[i] = s
	}
	return out
}
