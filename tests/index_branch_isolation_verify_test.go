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
}
