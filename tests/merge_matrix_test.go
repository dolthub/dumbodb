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

// Comprehensive merge conflict matrix tests. These exercise the full
// add/modify/delete cross-product across single and multiple collections.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// mergeMatrixSetup creates a database with baseline docs committed on main,
// then creates a "feature" branch. Returns mainDB and featDB handles.
func mergeMatrixSetup(
	t *testing.T,
	env *dumboDBTestEnv,
	dbName string,
	collections map[string][]interface{},
) (mainDB, featDB *mongo.Database) {
	t.Helper()
	ctx := context.Background()

	db := env.Client.Database(dbName)
	require.NoError(t, db.Drop(ctx))

	for coll, docs := range collections {
		if len(docs) > 0 {
			_, err := db.Collection(coll).InsertMany(ctx, docs)
			require.NoError(t, err, "inserting baseline docs into %s", coll)
		}
	}
	dumboDBCommit(t, env, dbName, "baseline", "alice <alice@acme.com>")

	mainDB = env.Client.Database(dbName + "@main")
	featDB = env.Client.Database(dbName + "@feature")

	var branchRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "feature"},
	}).Decode(&branchRaw))

	return mainDB, featDB
}

// resolveAllConflicts resolves every conflict in every collection with the
// given resolution strategy ("ours", "theirs", or "custom").
func resolveAllConflicts(t *testing.T, db *mongo.Database, resolution string) {
	t.Helper()
	ctx := context.Background()

	var conflictsRaw bson.M
	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "doltConflicts", Value: int32(1)},
	}).Decode(&conflictsRaw))

	conflicts, ok := conflictsRaw["conflicts"].(bson.A)
	if !ok || len(conflicts) == 0 {
		return
	}

	for _, c := range conflicts {
		cf := c.(bson.M)
		collName := cf["name"].(string)
		cid := cf["conflictId"].(string)
		var raw bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: int32(1)},
			{Key: "collection", Value: collName},
			{Key: "conflictId", Value: cid},
			{Key: "resolution", Value: resolution},
		}).Decode(&raw))
		assert.EqualValues(t, 1, raw["ok"])
	}
}

// mergeContinue finalizes the merge.
func mergeContinue(t *testing.T, db *mongo.Database) {
	t.Helper()
	var raw bson.M
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "continue", Value: int32(1)},
		{Key: "message", Value: "merge resolved"},
		{Key: "author", Value: "tester"},
	}).Decode(&raw))
	assert.EqualValues(t, 1, raw["ok"])
}

// getConflictsByCollection returns a map of collection -> []conflictId.
func getConflictsByCollection(t *testing.T, db *mongo.Database) map[string][]bson.M {
	t.Helper()
	var raw bson.M
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "doltConflicts", Value: int32(1)},
	}).Decode(&raw))

	result := make(map[string][]bson.M)
	conflicts, ok := raw["conflicts"].(bson.A)
	if !ok {
		return result
	}
	for _, c := range conflicts {
		cf := c.(bson.M)
		name := cf["name"].(string)
		result[name] = append(result[name], cf)
	}
	return result
}

// docExists returns whether a document with the given _id exists.
func docExists(t *testing.T, col *mongo.Collection, id int32) bool {
	t.Helper()
	err := col.FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Err()
	if err == mongo.ErrNoDocuments {
		return false
	}
	require.NoError(t, err)
	return true
}

// getDocField returns the value of field "v" for the given _id.
func getDocField(t *testing.T, col *mongo.Collection, id int32, field string) interface{} {
	t.Helper()
	var doc bson.M
	require.NoError(t, col.FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Decode(&doc))
	return doc[field]
}

func TestMergeMatrix_MixedChanges_SingleCollection(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mm1_%d", rand.Int64N(1_000_000))

	mainDB, featDB := mergeMatrixSetup(t, env, dbName, map[string][]interface{}{
		"items": {
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "orig1"}},
			bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "orig2"}},
			bson.D{{Key: "_id", Value: int32(3)}, {Key: "v", Value: "orig3"}},
			bson.D{{Key: "_id", Value: int32(4)}, {Key: "v", Value: "orig4"}},
			bson.D{{Key: "_id", Value: int32(5)}, {Key: "v", Value: "orig5"}},
		},
	})

	// Main: modify _id:1, modify _id:2, delete _id:4
	_, err := mainDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "main1"}}}})
	require.NoError(t, err)
	_, err = mainDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(2)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "main2"}}}})
	require.NoError(t, err)
	_, err = mainDB.Collection("items").DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(4)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main changes", "alice")

	// Feature: modify _id:1 (conflict), modify _id:3, delete _id:5
	_, err = featDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat1"}}}})
	require.NoError(t, err)
	_, err = featDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(3)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat3"}}}})
	require.NoError(t, err)
	_, err = featDB.Collection("items").DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(5)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature changes", "bob")

	// Merge: _id:1 conflicts, everything else auto-merges.
	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "merge_in", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"], "merge must conflict on _id:1")

	conflicts := getConflictsByCollection(t, mainDB)
	require.Len(t, conflicts["items"], 1, "exactly one conflict")
	assert.EqualValues(t, int32(1), conflicts["items"][0]["ours"].(bson.M)["_id"])

	// During merge: verify auto-merged state.
	col := mainDB.Collection("items")
	assert.Equal(t, "main2", getDocField(t, col, 2, "v"), "_id:2 must have main's value")
	assert.Equal(t, "feat3", getDocField(t, col, 3, "v"), "_id:3 must have feature's value")
	assert.False(t, docExists(t, col, 4), "_id:4 must be deleted (main deleted)")
	assert.False(t, docExists(t, col, 5), "_id:5 must be deleted (feature deleted)")

	// Resolve _id:1 with "theirs", continue.
	resolveAllConflicts(t, mainDB, "theirs")
	mergeContinue(t, mainDB)

	assert.Equal(t, "feat1", getDocField(t, col, 1, "v"), "_id:1 must have theirs (feat1)")
	assert.Equal(t, "main2", getDocField(t, col, 2, "v"), "_id:2 must have main2")
	assert.Equal(t, "feat3", getDocField(t, col, 3, "v"), "_id:3 must have feat3")
	assert.False(t, docExists(t, col, 4), "_id:4 must stay deleted")
	assert.False(t, docExists(t, col, 5), "_id:5 must stay deleted")
}

func TestMergeMatrix_DeleteModifyConflict(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mm2_%d", rand.Int64N(1_000_000))

	mainDB, featDB := mergeMatrixSetup(t, env, dbName, map[string][]interface{}{
		"items": {
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "orig"}},
		},
	})

	// Main: delete _id:1
	_, err := mainDB.Collection("items").DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main deletes", "alice")

	// Feature: modify _id:1
	_, err = featDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat1"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature modifies", "bob")

	// Merge: conflict (delete vs modify)
	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "merge_in", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"])

	conflicts := getConflictsByCollection(t, mainDB)
	require.Len(t, conflicts["items"], 1)
	cf := conflicts["items"][0]
	assert.Equal(t, "document", cf["type"])
	assert.Equal(t, "deleteModify", cf["reason"].(bson.M)["code"])
	assert.Nil(t, cf["ours"], "ours deleted the doc")
	assert.Equal(t, "modified", cf["theirs"].(bson.M)["diffType"])

	// Resolve with "theirs" -- doc reappears.
	resolveAllConflicts(t, mainDB, "theirs")
	mergeContinue(t, mainDB)

	col := mainDB.Collection("items")
	assert.True(t, docExists(t, col, 1), "_id:1 must exist after theirs resolution")
	assert.Equal(t, "feat1", getDocField(t, col, 1, "v"))
}

func TestMergeMatrix_ModifyDeleteConflict(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mm3_%d", rand.Int64N(1_000_000))

	mainDB, featDB := mergeMatrixSetup(t, env, dbName, map[string][]interface{}{
		"items": {
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "orig"}},
		},
	})

	// Main: modify _id:1
	_, err := mainDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "main1"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main modifies", "alice")

	// Feature: delete _id:1
	_, err = featDB.Collection("items").DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature deletes", "bob")

	// Merge: conflict (modify vs delete)
	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "merge_in", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"])

	conflicts := getConflictsByCollection(t, mainDB)
	require.Len(t, conflicts["items"], 1)
	cf := conflicts["items"][0]
	assert.Equal(t, "document", cf["type"])
	assert.Equal(t, "modifyDelete", cf["reason"].(bson.M)["code"])
	assert.Equal(t, "modified", cf["ours"].(bson.M)["diffType"])
	assert.Nil(t, cf["theirs"], "theirs deleted the doc")

	// Resolve with "theirs" -- doc is deleted.
	resolveAllConflicts(t, mainDB, "theirs")
	mergeContinue(t, mainDB)

	col := mainDB.Collection("items")
	assert.False(t, docExists(t, col, 1), "_id:1 must be deleted after theirs resolution")
}

func TestMergeMatrix_IndependentFieldAdds_NoConflict(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mm4_%d", rand.Int64N(1_000_000))

	mainDB, featDB := mergeMatrixSetup(t, env, dbName, map[string][]interface{}{
		"items": {
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "anchor"}},
		},
	})

	// Main: add _id:10 with {foo: 1}
	_, err := mainDB.Collection("items").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(10)}, {Key: "foo", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main adds foo", "alice")

	// Feature: add _id:10 with {bar: 1}
	_, err = featDB.Collection("items").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(10)}, {Key: "bar", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature adds bar", "bob")

	// Merge: non-overlapping fields on same _id should merge cleanly.
	var mergeRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "merge_in", Value: "feature"},
	}).Decode(&mergeRaw), "merge with independent fields must not conflict")
	assert.EqualValues(t, 1, mergeRaw["ok"])

	// Merged document should have both fields.
	var doc bson.M
	col := mainDB.Collection("items")
	require.NoError(t, col.FindOne(ctx, bson.D{{Key: "_id", Value: int32(10)}}).Decode(&doc))
	assert.EqualValues(t, 1, doc["foo"], "merged doc must have foo:1")
	assert.EqualValues(t, 1, doc["bar"], "merged doc must have bar:1")
}

func TestMergeMatrix_ConflictingAdds(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mm5_%d", rand.Int64N(1_000_000))

	mainDB, featDB := mergeMatrixSetup(t, env, dbName, map[string][]interface{}{
		"items": {
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "anchor"}},
		},
	})

	// Main: add _id:10 with {v: "main"}
	_, err := mainDB.Collection("items").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(10)}, {Key: "v", Value: "main"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main adds", "alice")

	// Feature: add _id:10 with {v: "feat"}
	_, err = featDB.Collection("items").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(10)}, {Key: "v", Value: "feat"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature adds", "bob")

	// Merge: same _id, same field, different values -- conflict.
	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "merge_in", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"], "conflicting adds must produce conflict")

	conflicts := getConflictsByCollection(t, mainDB)
	require.Len(t, conflicts["items"], 1)

	resolveAllConflicts(t, mainDB, "ours")
	mergeContinue(t, mainDB)

	col := mainDB.Collection("items")
	assert.Equal(t, "main", getDocField(t, col, 10, "v"), "ours resolution keeps main's value")
}

func TestMergeMatrix_ConvergentModify_NoConflict(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mm6_%d", rand.Int64N(1_000_000))

	mainDB, featDB := mergeMatrixSetup(t, env, dbName, map[string][]interface{}{
		"items": {
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "orig"}},
		},
	})

	// Both branches modify _id:1 to the same value.
	_, err := mainDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "same"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main same", "alice")

	_, err = featDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "same"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature same", "bob")

	// Merge: convergent edit -- no conflict.
	var mergeRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "merge_in", Value: "feature"},
	}).Decode(&mergeRaw), "convergent edits must not conflict")
	assert.EqualValues(t, 1, mergeRaw["ok"])

	col := mainDB.Collection("items")
	assert.Equal(t, "same", getDocField(t, col, 1, "v"))
}

func TestMergeMatrix_MultiCollection_MixedConflicts(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mm7_%d", rand.Int64N(1_000_000))

	mainDB, featDB := mergeMatrixSetup(t, env, dbName, map[string][]interface{}{
		"orders": {
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "o1"}},
			bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "o2"}},
		},
		"users": {
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "u1"}},
			bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "u2"}},
		},
	})

	// Main: modify orders._id:1, modify users._id:1 (clean), modify users._id:2
	_, err := mainDB.Collection("orders").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "main-o1"}}}})
	require.NoError(t, err)
	_, err = mainDB.Collection("users").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "main-u1"}}}})
	require.NoError(t, err)
	_, err = mainDB.Collection("users").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(2)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "main-u2"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main changes", "alice")

	// Feature: modify orders._id:1 (conflict), modify orders._id:2 (clean),
	//          modify users._id:2 (conflict -- both sides modified)
	_, err = featDB.Collection("orders").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat-o1"}}}})
	require.NoError(t, err)
	_, err = featDB.Collection("orders").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(2)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat-o2"}}}})
	require.NoError(t, err)
	_, err = featDB.Collection("users").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(2)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat-u2"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature changes", "bob")

	// Merge: orders._id:1 conflicts, users._id:2 conflicts
	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "merge_in", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"])

	conflicts := getConflictsByCollection(t, mainDB)
	require.Len(t, conflicts["orders"], 1, "one conflict in orders")
	require.Len(t, conflicts["users"], 1, "one conflict in users")

	// Clean merges should already be visible.
	assert.Equal(t, "feat-o2", getDocField(t, mainDB.Collection("orders"), 2, "v"),
		"orders._id:2 must have feature's value (clean merge)")
	assert.Equal(t, "main-u1", getDocField(t, mainDB.Collection("users"), 1, "v"),
		"users._id:1 must have main's value (clean merge)")

	// Resolve all with "theirs" and continue.
	resolveAllConflicts(t, mainDB, "theirs")
	mergeContinue(t, mainDB)

	assert.Equal(t, "feat-o1", getDocField(t, mainDB.Collection("orders"), 1, "v"))
	assert.Equal(t, "feat-o2", getDocField(t, mainDB.Collection("orders"), 2, "v"))
	assert.Equal(t, "main-u1", getDocField(t, mainDB.Collection("users"), 1, "v"))
	assert.Equal(t, "feat-u2", getDocField(t, mainDB.Collection("users"), 2, "v"))
}

func TestMergeMatrix_MultiCollection_OneClean(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mm8_%d", rand.Int64N(1_000_000))

	mainDB, featDB := mergeMatrixSetup(t, env, dbName, map[string][]interface{}{
		"alpha": {
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "a1"}},
		},
		"beta": {
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "b1"}},
		},
	})

	// Main: modify alpha._id:1
	_, err := mainDB.Collection("alpha").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "main-a1"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main alpha", "alice")

	// Feature: modify alpha._id:1 (conflict), modify beta._id:1 (clean)
	_, err = featDB.Collection("alpha").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat-a1"}}}})
	require.NoError(t, err)
	_, err = featDB.Collection("beta").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat-b1"}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature both", "bob")

	// Merge: alpha conflicts, beta auto-merges.
	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "merge_in", Value: "feature"},
	})
	require.EqualValues(t, 0, raw["ok"])

	conflicts := getConflictsByCollection(t, mainDB)
	require.Len(t, conflicts["alpha"], 1, "alpha has one conflict")
	assert.Len(t, conflicts["beta"], 0, "beta must have no conflicts")

	// Beta's changes must be visible even before resolving alpha.
	assert.Equal(t, "feat-b1", getDocField(t, mainDB.Collection("beta"), 1, "v"),
		"beta._id:1 must have feature's value before resolution")

	resolveAllConflicts(t, mainDB, "ours")
	mergeContinue(t, mainDB)

	assert.Equal(t, "main-a1", getDocField(t, mainDB.Collection("alpha"), 1, "v"),
		"alpha._id:1 must have ours (main-a1)")
	assert.Equal(t, "feat-b1", getDocField(t, mainDB.Collection("beta"), 1, "v"),
		"beta._id:1 must have feature's value")
}

func TestMergeMatrix_MultiCollection_IndependentNewCollections(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mm9_%d", rand.Int64N(1_000_000))

	mainDB, featDB := mergeMatrixSetup(t, env, dbName, map[string][]interface{}{
		"shared": {
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "s1"}},
		},
	})

	// Main: create a new collection "mainonly"
	_, err := mainDB.Collection("mainonly").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "from-main"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main new coll", "alice")

	// Feature: create a different new collection "featonly"
	_, err = featDB.Collection("featonly").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "from-feat"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature new coll", "bob")

	// Merge: independent new collections -- clean merge.
	var mergeRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "merge_in", Value: "feature"},
	}).Decode(&mergeRaw), "independent new collections must merge cleanly")
	assert.EqualValues(t, 1, mergeRaw["ok"])

	// Both new collections must exist.
	assert.Equal(t, "from-main", getDocField(t, mainDB.Collection("mainonly"), 1, "v"))
	assert.Equal(t, "from-feat", getDocField(t, mainDB.Collection("featonly"), 1, "v"))
	// Shared collection must still be there.
	assert.Equal(t, "s1", getDocField(t, mainDB.Collection("shared"), 1, "v"))
}
