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

// Coverage for the `metadata` block of dumboDiff/dumboStatus/dumboLog: a
// collection's validator and validation options are reported as path-based
// field diffs, the same shape documents.modified[].diff uses.

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

var (
	ageAtLeastZero = bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 0}}}}
	ageAtLeast21   = bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 21}}}}
)

// collModValidator applies a validator and its options to an existing collection.
func collModValidator(t *testing.T, db *mongo.Database, coll string, validator bson.D, level, action string) {
	t.Helper()
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "collMod", Value: coll},
		{Key: "validator", Value: validator},
		{Key: "validationLevel", Value: level},
		{Key: "validationAction", Value: action},
	}).Err())
}

// metadataDiffOf runs dumboDiff and returns the metadata.diff entries for the
// named collection, requiring that the collection appears in the output.
func metadataDiffOf(t *testing.T, db *mongo.Database, coll string, diffArgs ...bson.E) []bson.M {
	t.Helper()
	cmd := append(bson.D{{Key: "doltDiff", Value: int32(1)}}, diffArgs...)

	var raw bson.M
	require.NoError(t, db.RunCommand(context.Background(), cmd).Decode(&raw))

	change := changeFor(t, raw, coll)
	metadata, ok := change["metadata"].(bson.M)
	require.True(t, ok, "metadata must be a document, got %T", change["metadata"])

	entries, _ := metadata["diff"].(bson.A)
	out := make([]bson.M, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.(bson.M))
	}
	return out
}

// changeFor returns the `changes` entry for one collection.
func changeFor(t *testing.T, raw bson.M, coll string) bson.M {
	t.Helper()
	changes, _ := raw["changes"].(bson.A)
	for _, c := range changes {
		entry := c.(bson.M)
		if entry["name"] == coll {
			return entry
		}
	}
	t.Fatalf("no change entry for collection %q in %v", coll, raw["changes"])
	return nil
}

// A change to validationLevel alone reports exactly that one path. The
// unchanged validator and validationAction do not appear.
func TestDiffMetadata_LevelOnlyChange(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dmlevel%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)

	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	collModValidator(t, db, "items", ageAtLeastZero, "strict", "error")
	dumboDBCommit(t, env, dbName, "baseline", "alice")

	collModValidator(t, db, "items", ageAtLeastZero, "moderate", "error")

	entries := metadataDiffOf(t, db, "items")
	require.Len(t, entries, 1, "only validationLevel changed")
	assert.Equal(t, "modified", entries[0]["type"])
	assert.Equal(t, "$.validationLevel", entries[0]["path"])
	assert.Equal(t, "strict", entries[0]["from"])
	assert.Equal(t, "moderate", entries[0]["to"])
}

// A validator change reports the changed leaf inside the validator, not the
// whole expression.
func TestDiffMetadata_ValidatorLeafPath(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dmval%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)

	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	collModValidator(t, db, "items", ageAtLeastZero, "strict", "error")
	dumboDBCommit(t, env, dbName, "baseline", "alice")

	collModValidator(t, db, "items", ageAtLeast21, "strict", "error")

	entries := metadataDiffOf(t, db, "items")
	require.Len(t, entries, 1, "one leaf changed")
	assert.Equal(t, "modified", entries[0]["type"])
	assert.Equal(t, "$.validator.age.$gte", entries[0]["path"])
	assert.EqualValues(t, 0, entries[0]["from"])
	assert.EqualValues(t, 21, entries[0]["to"])
}

// A one-field edit deep inside a $jsonSchema validator reports only that
// field's path, not the surrounding schema.
func TestDiffMetadata_JSONSchemaLeafPath(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dmschema%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)

	schema := func(emailPattern string) bson.D {
		return bson.D{{Key: "$jsonSchema", Value: bson.D{
			{Key: "bsonType", Value: "object"},
			{Key: "required", Value: bson.A{"name", "email"}},
			{Key: "properties", Value: bson.D{
				{Key: "name", Value: bson.D{
					{Key: "bsonType", Value: "string"},
					{Key: "minLength", Value: 1},
					{Key: "description", Value: "must be a string and is required"},
				}},
				{Key: "email", Value: bson.D{
					{Key: "bsonType", Value: "string"},
					{Key: "pattern", Value: emailPattern},
					{Key: "description", Value: "must be a valid email string and is required"},
				}},
			}},
		}}}
	}

	_, err := db.Collection("users").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "name", Value: "alice"},
		{Key: "email", Value: "alice@acme.com"},
	})
	require.NoError(t, err)
	collModValidator(t, db, "users", schema("^.+@.+$"), "strict", "error")
	dumboDBCommit(t, env, dbName, "baseline schema", "alice")

	collModValidator(t, db, "users", schema(`^.+@.+\..+$`), "strict", "error")

	entries := metadataDiffOf(t, db, "users")
	require.Len(t, entries, 1, "only the email pattern changed: %v", entries)
	assert.Equal(t, "modified", entries[0]["type"])
	assert.Equal(t, "$.validator.$jsonSchema.properties.email.pattern", entries[0]["path"])
	assert.Equal(t, "^.+@.+$", entries[0]["from"])
	assert.Equal(t, `^.+@.+\..+$`, entries[0]["to"])
}

// A schema key literally named _id is a real key, not a document identifier,
// so it must not be skipped the way a document's _id is.
func TestDiffMetadata_SchemaPropertyNamedID(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dmid%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)

	schema := func(idType string) bson.D {
		return bson.D{{Key: "$jsonSchema", Value: bson.D{
			{Key: "bsonType", Value: "object"},
			{Key: "properties", Value: bson.D{
				{Key: "_id", Value: bson.D{{Key: "bsonType", Value: idType}}},
			}},
		}}}
	}

	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	collModValidator(t, db, "items", schema("int"), "strict", "error")
	dumboDBCommit(t, env, dbName, "baseline", "alice")

	collModValidator(t, db, "items", schema("string"), "strict", "error")

	entries := metadataDiffOf(t, db, "items")
	require.Len(t, entries, 1, "the _id property change must not be skipped: %v", entries)
	assert.Equal(t, "$.validator.$jsonSchema.properties._id.bsonType", entries[0]["path"])
	assert.Equal(t, "int", entries[0]["from"])
	assert.Equal(t, "string", entries[0]["to"])
}

// Adding a field to a validator reports it as added at the level where it
// appeared, carrying its whole subtree, with no from side.
func TestDiffMetadata_ValidatorFieldAdded(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dmvadd%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)

	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	collModValidator(t, db, "items", ageAtLeastZero, "strict", "error")
	dumboDBCommit(t, env, dbName, "baseline", "alice")

	withName := bson.D{
		{Key: "age", Value: bson.D{{Key: "$gte", Value: 0}}},
		{Key: "name", Value: bson.D{{Key: "$type", Value: "string"}}},
	}
	collModValidator(t, db, "items", withName, "strict", "error")

	entries := metadataDiffOf(t, db, "items")
	require.Len(t, entries, 1)
	assert.Equal(t, "added", entries[0]["type"])
	assert.Equal(t, "$.validator.name", entries[0]["path"])
	assert.NotContains(t, entries[0], "from", "an added field has no from side")
	assert.Equal(t, bson.M{"$type": "string"}, entries[0]["to"])
}

// Changing the validator and its level together reports both paths, validator
// first, and still omits the untouched validationAction.
func TestDiffMetadata_ValidatorAndLevelTogether(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dmboth%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)

	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	collModValidator(t, db, "items", ageAtLeastZero, "strict", "error")
	dumboDBCommit(t, env, dbName, "baseline", "alice")

	collModValidator(t, db, "items", ageAtLeast21, "moderate", "error")

	entries := metadataDiffOf(t, db, "items")
	require.Len(t, entries, 2)
	assert.Equal(t, "$.validator.age.$gte", entries[0]["path"])
	assert.Equal(t, "$.validationLevel", entries[1]["path"])
	for _, e := range entries {
		assert.Equal(t, "modified", e["type"])
	}
}

// A collection that gains a validator where it had none reports every spec
// field as added, with no `from` side.
func TestDiffMetadata_ValidatorAdded(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dmadd%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)

	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "baseline without validator", "alice")

	collModValidator(t, db, "items", ageAtLeastZero, "strict", "error")

	entries := metadataDiffOf(t, db, "items")
	require.Len(t, entries, 3, "the whole spec is new")

	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		assert.Equal(t, "added", e["type"])
		_, hasFrom := e["from"]
		assert.False(t, hasFrom, "an added field has no from side: %v", e)
		require.Contains(t, e, "to")
		paths = append(paths, e["path"].(string))
	}
	assert.Equal(t, []string{"$.validator", "$.validationLevel", "$.validationAction"}, paths)
}

// Diffing in the direction that drops the validator reports every spec field as
// removed, with no `to` side.
func TestDiffMetadata_ValidatorRemoved(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dmrem%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)

	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "baseline without validator", "alice")

	collModValidator(t, db, "items", ageAtLeastZero, "strict", "error")
	dumboDBCommit(t, env, dbName, "add validator", "alice")

	// from the validated tip back to its parent: the validator goes away.
	entries := metadataDiffOf(t, db, "items",
		bson.E{Key: "from", Value: "HEAD"},
		bson.E{Key: "to", Value: "HEAD~1"})
	require.Len(t, entries, 3, "the whole spec is gone")

	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		assert.Equal(t, "removed", e["type"])
		_, hasTo := e["to"]
		assert.False(t, hasTo, "a removed field has no to side: %v", e)
		require.Contains(t, e, "from")
		paths = append(paths, e["path"].(string))
	}
	assert.Equal(t, []string{"$.validator", "$.validationLevel", "$.validationAction"}, paths)
}

// Unchanged metadata stays an empty document, so consumers keep testing for
// emptiness rather than branching on field presence.
func TestDiffMetadata_UnchangedIsEmptyDocument(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dmnone%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)

	_, err := db.Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "age", Value: int32(30)},
		{Key: "v", Value: "orig"},
	})
	require.NoError(t, err)
	collModValidator(t, db, "items", ageAtLeastZero, "strict", "error")
	dumboDBCommit(t, env, dbName, "baseline", "alice")

	_, err = db.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "changed"}}}})
	require.NoError(t, err)

	var raw bson.M
	require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "doltDiff", Value: int32(1)}}).Decode(&raw))

	change := changeFor(t, raw, "items")
	metadata, ok := change["metadata"].(bson.M)
	require.True(t, ok, "metadata must be a document, got %T", change["metadata"])
	assert.Empty(t, metadata, "a document-only change leaves metadata empty")
}

// dumboStatus renders metadata at summary verbosity: the changed paths and
// their types, without the values.
func TestStatusMetadata_PathsWithoutValues(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dmstat%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)

	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	collModValidator(t, db, "items", ageAtLeastZero, "strict", "error")
	dumboDBCommit(t, env, dbName, "baseline", "alice")

	collModValidator(t, db, "items", ageAtLeast21, "moderate", "error")

	var raw bson.M
	require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&raw))

	change := changeFor(t, raw, "items")
	metadata, ok := change["metadata"].(bson.M)
	require.True(t, ok, "metadata must be a document, got %T", change["metadata"])

	entries, _ := metadata["diff"].(bson.A)
	require.Len(t, entries, 2)
	for _, e := range entries {
		entry := e.(bson.M)
		assert.Equal(t, "modified", entry["type"])
		require.Contains(t, entry, "path")
		assert.NotContains(t, entry, "from", "summary verbosity carries no values")
		assert.NotContains(t, entry, "to", "summary verbosity carries no values")
	}
}

// dumboLog carries the metadata change at both verbosities: stat gives paths
// only, patch gives the values too.
func TestLogMetadata_StatVersusPatch(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dmlog%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)

	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	collModValidator(t, db, "items", ageAtLeastZero, "strict", "error")
	dumboDBCommit(t, env, dbName, "baseline", "alice")

	collModValidator(t, db, "items", ageAtLeastZero, "moderate", "error")
	dumboDBCommit(t, env, dbName, "relax the validation level", "alice")

	metadataFromLog := func(arg string) bson.M {
		var raw bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
			{Key: arg, Value: true},
		}).Decode(&raw))

		commits, _ := raw["commits"].(bson.A)
		require.Len(t, commits, 1)
		head := commits[0].(bson.M)

		changes, _ := head["changes"].(bson.A)
		require.NotEmpty(t, changes, "%s must report the collMod", arg)
		metadata, ok := changes[0].(bson.M)["metadata"].(bson.M)
		require.True(t, ok, "metadata must be a document")
		return metadata
	}

	statEntries, _ := metadataFromLog("stat")["diff"].(bson.A)
	require.Len(t, statEntries, 1)
	statEntry := statEntries[0].(bson.M)
	assert.Equal(t, "$.validationLevel", statEntry["path"])
	assert.NotContains(t, statEntry, "from", "stat carries no values")

	patchEntries, _ := metadataFromLog("patch")["diff"].(bson.A)
	require.Len(t, patchEntries, 1)
	patchEntry := patchEntries[0].(bson.M)
	assert.Equal(t, "$.validationLevel", patchEntry["path"])
	assert.Equal(t, "strict", patchEntry["from"])
	assert.Equal(t, "moderate", patchEntry["to"])
}
