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

package verify

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
)

func vmMatchPipeline(status string) bson.A {
	return bson.A{bson.D{{Key: "$match", Value: bson.D{{Key: "status", Value: status}}}}}
}

func vmCreateView(t *testing.T, db *mongo.Database, name, viewOn string, pipeline bson.A) {
	t.Helper()
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "create", Value: name},
		{Key: "viewOn", Value: viewOn},
		{Key: "pipeline", Value: pipeline},
	}).Err())
}

func vmRedefineView(t *testing.T, db *mongo.Database, name, viewOn string, pipeline bson.A) {
	t.Helper()
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "collMod", Value: name},
		{Key: "viewOn", Value: viewOn},
		{Key: "pipeline", Value: pipeline},
	}).Err())
}

func vmViewIDs(t *testing.T, db *mongo.Database, view string) []int32 {
	t.Helper()
	cur, err := db.Collection(view).Find(context.Background(), bson.D{})
	require.NoError(t, err)
	var docs []bson.M
	require.NoError(t, cur.All(context.Background(), &docs))
	ids := make([]int32, 0, len(docs))
	for _, d := range docs {
		if v, ok := d["_id"].(int32); ok {
			ids = append(ids, v)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func vmSeedItems(t *testing.T, db *mongo.Database) {
	t.Helper()
	_, err := db.Collection("items").InsertMany(context.Background(), []interface{}{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "status", Value: "active"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "status", Value: "inactive"}},
		bson.D{{Key: "_id", Value: int32(3)}, {Key: "status", Value: "pending"}},
	})
	require.NoError(t, err)
}

func vmBranch(t *testing.T, env *dumboDBTestEnv, dbName, branch string) {
	t.Helper()
	require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(context.Background(), bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: branch},
	}).Err())
}

func vmMerge(t *testing.T, env *dumboDBTestEnv, dbName, branch string) bson.M {
	t.Helper()
	return runCommandRaw(t, env.Client.Database(dbName+"@main"), bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "mergeIn", Value: branch},
	})
}

func vmViewConflict(t *testing.T, mainDB *mongo.Database) bson.M {
	t.Helper()
	var rc bson.M
	require.NoError(t, mainDB.RunCommand(context.Background(), bson.D{{Key: "doltConflicts", Value: int32(1)}}).Decode(&rc))
	all, ok := rc["conflicts"].(bson.A)
	require.True(t, ok, "doltConflicts response missing conflicts array: %v", rc)
	var views []bson.M
	for _, c := range all {
		entry := c.(bson.M)
		if entry["type"] == "view" {
			views = append(views, entry)
		}
	}
	require.Len(t, views, 1, "expected exactly one view conflict: %v", all)
	return views[0]
}

func vmResolve(t *testing.T, mainDB *mongo.Database, view, conflictID, resolution string, value bson.D) {
	t.Helper()
	cmd := bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "collection", Value: view},
		{Key: "conflictId", Value: conflictID},
		{Key: "resolution", Value: resolution},
	}
	if value != nil {
		cmd = append(cmd, bson.E{Key: "value", Value: value})
	}
	require.NoError(t, mainDB.RunCommand(context.Background(), cmd).Err())
}

func vmContinue(t *testing.T, mainDB *mongo.Database) {
	t.Helper()
	require.NoError(t, mainDB.RunCommand(context.Background(), bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "continue", Value: int32(1)},
	}).Err())
}

func setupRedefineConflict(t *testing.T, env *dumboDBTestEnv, dbName string) (*mongo.Database, string) {
	t.Helper()
	db := env.Client.Database(dbName)
	require.NoError(t, db.Drop(context.Background()))
	vmSeedItems(t, db)
	vmCreateView(t, db, "cv", "items", vmMatchPipeline("active"))
	dumboDBCommit(t, env, dbName, "seed items + view cv", "alice <alice@acme.com>")
	vmBranch(t, env, dbName, "feature")

	vmRedefineView(t, env.Client.Database(dbName+"@feature"), "cv", "items", vmMatchPipeline("inactive"))
	dumboDBCommit(t, env, dbName+"@feature", "feature: cv -> inactive", "bob <bob@widgets.io>")

	vmRedefineView(t, db, "cv", "items", vmMatchPipeline("pending"))
	dumboDBCommit(t, env, dbName, "main: cv -> pending", "alice <alice@acme.com>")

	raw := vmMerge(t, env, dbName, "feature")
	assert.EqualValues(t, 0, raw["ok"], "divergent view redefine must surface a conflict: %v", raw)

	mainDB := env.Client.Database(dbName + "@main")
	entry := vmViewConflict(t, mainDB)
	assert.Equal(t, "cv", entry["collection"])
	assert.Equal(t, "modified", entry["ours"].(bson.M)["diffType"])
	assert.Equal(t, "modified", entry["theirs"].(bson.M)["diffType"])
	return mainDB, entry["conflictId"].(string)
}

// View merge has no MongoDB counterpart, so it is verified against DumboDB alone.
func TestViewMergeVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	suffix := rand.Int64N(1_000_000)

	t.Run("Scenario1_CleanMerge_ViewAddedOnBranch", func(t *testing.T) {
		dbName := fmt.Sprintf("vwmrg1v%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		vmSeedItems(t, db)
		dumboDBCommit(t, env, dbName, "seed items", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

		vmCreateView(t, env.Client.Database(dbName+"@feature"), "cv", "items", vmMatchPipeline("active"))
		dumboDBCommit(t, env, dbName+"@feature", "feature: add view cv", "bob <bob@widgets.io>")

		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 4}, {Key: "status", Value: "active"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: add item 4", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		assert.EqualValues(t, 1, raw["ok"], "adding a view on one branch must merge cleanly: %v", raw)
		assert.NotEqual(t, "fast-forward", raw["message"], "diverged branches must produce a real merge commit, not a fast-forward: %v", raw)

		assert.Equal(t, []int32{1, 4}, vmViewIDs(t, db, "cv"), "merged view resolves the active docs from both sides")
	})

	t.Run("Scenario2_RedefineConflict_ResolveTheirs", func(t *testing.T) {
		dbName := fmt.Sprintf("vwmrg2v%d", suffix)
		mainDB, conflictID := setupRedefineConflict(t, env, dbName)
		vmResolve(t, mainDB, "cv", conflictID, "theirs", nil)
		vmContinue(t, mainDB)
		assert.Equal(t, []int32{2}, vmViewIDs(t, env.Client.Database(dbName), "cv"),
			"resolving theirs applies feature's inactive definition")
	})

	t.Run("Scenario3_RedefineConflict_ResolveOurs", func(t *testing.T) {
		dbName := fmt.Sprintf("vwmrg3v%d", suffix)
		mainDB, conflictID := setupRedefineConflict(t, env, dbName)
		vmResolve(t, mainDB, "cv", conflictID, "ours", nil)
		vmContinue(t, mainDB)
		assert.Equal(t, []int32{3}, vmViewIDs(t, env.Client.Database(dbName), "cv"),
			"resolving ours keeps main's pending definition")
	})

	t.Run("Scenario4_RedefineConflict_ResolveCustom", func(t *testing.T) {
		dbName := fmt.Sprintf("vwmrg4v%d", suffix)
		mainDB, conflictID := setupRedefineConflict(t, env, dbName)
		custom := bson.D{
			{Key: "viewOn", Value: "items"},
			{Key: "pipeline", Value: vmMatchPipeline("active")},
		}
		vmResolve(t, mainDB, "cv", conflictID, "custom", custom)
		vmContinue(t, mainDB)
		assert.Equal(t, []int32{1}, vmViewIDs(t, env.Client.Database(dbName), "cv"),
			"resolving custom applies the supplied definition")
	})

	t.Run("Scenario5_RedefineDropConflict_ResolveTheirsDeletes", func(t *testing.T) {
		dbName := fmt.Sprintf("vwmrg5v%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		vmSeedItems(t, db)
		vmCreateView(t, db, "cv", "items", vmMatchPipeline("active"))
		dumboDBCommit(t, env, dbName, "seed items + view cv", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

		require.NoError(t, env.Client.Database(dbName+"@feature").Collection("cv").Drop(ctx))
		dumboDBCommit(t, env, dbName+"@feature", "feature: drop cv", "bob <bob@widgets.io>")
		vmRedefineView(t, db, "cv", "items", vmMatchPipeline("pending"))
		dumboDBCommit(t, env, dbName, "main: cv -> pending", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		assert.EqualValues(t, 0, raw["ok"], "redefine/drop must surface a conflict: %v", raw)

		mainDB := env.Client.Database(dbName + "@main")
		entry := vmViewConflict(t, mainDB)
		assert.Equal(t, "modified", entry["ours"].(bson.M)["diffType"])
		assert.Nil(t, entry["theirs"], "theirs deleted the view")

		vmResolve(t, mainDB, "cv", entry["conflictId"].(string), "theirs", nil)
		vmContinue(t, mainDB)

		var listed bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "listCollections", Value: int32(1)},
			{Key: "filter", Value: bson.D{{Key: "name", Value: "cv"}}},
		}).Decode(&listed))
		batch := listed["cursor"].(bson.M)["firstBatch"].(bson.A)
		assert.Len(t, batch, 0, "resolving theirs deleted the view")
	})
}
