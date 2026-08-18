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

// dumboResolveConflict takes conflictId alone; "collection" is only needed to
// disambiguate an id shared by two collections, which happens because a
// document conflict id hashes the document key and the "theirs" commit hash,
// not the owning collection.

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

// conflictingMerge builds a merge that conflicts on _id 1 of each named
// collection, leaving main mid-merge. It returns the main-branch handle.
func conflictingMerge(t *testing.T, env *dumboDBTestEnv, dbName string, collections ...string) *mongo.Database {
	t.Helper()
	ctx := context.Background()

	db := env.Client.Database(dbName)
	require.NoError(t, db.Drop(ctx))
	for _, coll := range collections {
		_, err := db.Collection(coll).InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(1)}, {Key: "v", Value: "base"},
		})
		require.NoError(t, err)
	}
	dumboDBCommit(t, env, dbName, "baseline", "alice")

	mainDB := env.Client.Database(dbName + "@main")
	var branchRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "feature"},
	}).Decode(&branchRaw))

	featDB := env.Client.Database(dbName + "@feature")
	for _, coll := range collections {
		_, err := featDB.Collection(coll).UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "theirs"}}}})
		require.NoError(t, err)
	}
	dumboDBCommit(t, env, dbName+"@feature", "feature edit", "bob")

	for _, coll := range collections {
		_, err := db.Collection(coll).UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "ours"}}}})
		require.NoError(t, err)
	}
	dumboDBCommit(t, env, dbName, "main edit", "alice")

	// A conflicting merge answers ok:0; runCommandRaw captures that body.
	mergeRaw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "mergeIn", Value: "feature"},
	})
	require.EqualValues(t, 0, mergeRaw["ok"], "merge must conflict: %v", mergeRaw)

	return mainDB
}

func conflictEntries(t *testing.T, db *mongo.Database) []bson.M {
	t.Helper()
	var raw bson.M
	require.NoError(t, db.RunCommand(context.Background(),
		bson.D{{Key: "doltConflicts", Value: int32(1)}}).Decode(&raw))
	all, _ := raw["conflicts"].(bson.A)
	out := make([]bson.M, 0, len(all))
	for _, c := range all {
		out = append(out, c.(bson.M))
	}
	return out
}

// A conflict entry names its namespace under `collection`.
func TestConflicts_EntryNamesCollection(t *testing.T) {
	env := startDumboDB(t)
	dbName := fmt.Sprintf("cfname%d", rand.Int64N(1_000_000))
	mainDB := conflictingMerge(t, env, dbName, "items")

	entries := conflictEntries(t, mainDB)
	require.Len(t, entries, 1)
	assert.Equal(t, "items", entries[0]["collection"])
	assert.NotContains(t, entries[0], "name", "the namespace field is `collection`")
}

// conflictId alone resolves a document conflict.
func TestResolveConflict_ByIDAlone(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("cfbyid%d", rand.Int64N(1_000_000))
	mainDB := conflictingMerge(t, env, dbName, "items")

	entries := conflictEntries(t, mainDB)
	require.Len(t, entries, 1)

	var raw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "conflictId", Value: entries[0]["conflictId"]},
		{Key: "resolution", Value: "theirs"},
	}).Decode(&raw))
	assert.EqualValues(t, 1, raw["ok"])

	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltMerge", Value: int32(1)}, {Key: "continue", Value: int32(1)},
	}).Err())

	var got bson.M
	require.NoError(t, mainDB.Collection("items").
		FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&got))
	assert.Equal(t, "theirs", got["v"], "resolution applied")
}

// conflictId alone resolves a metadata conflict too.
func TestResolveConflict_MetadataByIDAlone(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("cfmeta%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)
	require.NoError(t, db.Drop(ctx))

	_, err := db.Collection("users").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "users"},
		{Key: "validator", Value: bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 0}}}}},
	}).Err())
	dumboDBCommit(t, env, dbName, "baseline validator", "alice")

	mainDB := env.Client.Database(dbName + "@main")
	var branchRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)}, {Key: "branch", Value: "feature"},
	}).Decode(&branchRaw))

	require.NoError(t, env.Client.Database(dbName+"@feature").RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "users"},
		{Key: "validator", Value: bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 18}}}}},
	}).Err())
	dumboDBCommit(t, env, dbName+"@feature", "feature validator", "bob")

	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "users"},
		{Key: "validator", Value: bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 21}}}}},
	}).Err())
	dumboDBCommit(t, env, dbName, "main validator", "alice")

	mergeRaw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)}, {Key: "mergeIn", Value: "feature"},
	})
	require.EqualValues(t, 0, mergeRaw["ok"], "divergent validators must conflict")

	entries := conflictEntries(t, mainDB)
	require.Len(t, entries, 1)
	assert.Equal(t, "metadata", entries[0]["type"])
	assert.Equal(t, "users", entries[0]["collection"])

	var raw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "conflictId", Value: entries[0]["conflictId"]},
		{Key: "resolution", Value: "theirs"},
	}).Decode(&raw))
	assert.EqualValues(t, 1, raw["ok"])
}

// The same _id conflicting in two collections yields one id in both, since the
// id does not encode the collection. That must be reported, not guessed.
func TestResolveConflict_AmbiguousIDRequiresCollection(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("cfambig%d", rand.Int64N(1_000_000))
	mainDB := conflictingMerge(t, env, dbName, "alpha", "beta")

	entries := conflictEntries(t, mainDB)
	require.Len(t, entries, 2)
	require.Equal(t, entries[0]["conflictId"], entries[1]["conflictId"],
		"same _id in one merge shares an id across collections")

	err := mainDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "conflictId", Value: entries[0]["conflictId"]},
		{Key: "resolution", Value: "theirs"},
	}).Err()
	require.Error(t, err, "an ambiguous id must not be resolved by guessing")
	assert.Contains(t, err.Error(), "alpha, beta", "the error names the candidates")
	assert.Contains(t, err.Error(), "collection", "the error names the way out")

	// Naming the collection resolves the intended one.
	var raw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "collection", Value: "beta"},
		{Key: "conflictId", Value: entries[0]["conflictId"]},
		{Key: "resolution", Value: "theirs"},
	}).Decode(&raw))
	assert.EqualValues(t, 1, raw["ok"])
}

// An id that matches nothing is reported as such.
func TestResolveConflict_UnknownID(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("cfunknown%d", rand.Int64N(1_000_000))
	mainDB := conflictingMerge(t, env, dbName, "items")

	err := mainDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "conflictId", Value: "not-a-real-conflict-id"},
		{Key: "resolution", Value: "ours"},
	}).Err()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no unresolved conflict with id")
}

// A conflict id that does not belong to the named collection is rejected
// rather than silently resolving whatever that collection has.
func TestResolveConflict_IDMustMatchNamedCollection(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("cfmismatch%d", rand.Int64N(1_000_000))
	mainDB := conflictingMerge(t, env, dbName, "items")

	err := mainDB.RunCommand(ctx, bson.D{
		{Key: "doltResolveConflict", Value: int32(1)},
		{Key: "collection", Value: "items"},
		{Key: "conflictId", Value: "not-a-real-conflict-id"},
		{Key: "resolution", Value: "ours"},
	}).Err()
	require.Error(t, err, "a mismatched id must not resolve the collection's conflict")
}
