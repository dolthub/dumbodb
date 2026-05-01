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

// TestTagVerify is the automated analog of docs/verify/tag.md.
//
// Each top-level subtest corresponds to one scenario in that document.
// The setup reproduces the manual setup block exactly:
//
//   - Commit 1 (hash1): items = [ { _id:1, label:"alpha" } ]
//   - Commit 2 (hash2, HEAD): items = [ { _id:1, ... }, { _id:2, label:"beta" } ]
//
// Subtests run sequentially (no t.Parallel inside) so they share a single
// database and the side effects of one scenario carry into the next.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// tagVerifySetup mirrors the Setup section of docs/verify/tag.md.
// Returns hash1 (commit 1) and hash2 (commit 2, same as main HEAD).
func tagVerifySetup(t *testing.T, env *dumboDBTestEnv, dbName string) (hash1, hash2 string) {
	t.Helper()

	ctx := context.Background()
	db := env.client.Database(dbName)
	items := db.Collection("items")

	require.NoError(t, db.Drop(ctx))

	// Commit 1: one document.
	_, err := items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "label", Value: "alpha"},
	})
	require.NoError(t, err)
	hash1 = dumboDBCommit(t, env, dbName, "commit one", "alice <alice@acme.com>")

	// Commit 2: second document added.
	_, err = items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "label", Value: "beta"},
	})
	require.NoError(t, err)
	hash2 = dumboDBCommit(t, env, dbName, "commit two", "alice <alice@acme.com>")

	return hash1, hash2
}

// tagFromList finds a tag by name in a dumboTag list result.
func tagFromList(t *testing.T, tags []interface{}, name string) bson.M {
	t.Helper()
	for _, raw := range tags {
		tag, ok := raw.(bson.M)
		require.True(t, ok)
		if tag["name"] == name {
			return tag
		}
	}
	t.Fatalf("tag %q not found in list", name)
	return nil
}

func TestTagVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("tagvrfy%d", rand.Int64N(1_000_000))

	hash1, hash2 := tagVerifySetup(t, env, dbName)

	// -------------------------------------------------------------------------
	// Scenario 1: Create a tag at the current branch HEAD
	// -------------------------------------------------------------------------
	t.Run("Scenario1_CreateTagAtHead", func(t *testing.T) {
		var result bson.M
		err := env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: int32(1)},
			{Key: "name", Value: "v-head"},
			{Key: "message", Value: "tag at current head"},
			{Key: "author", Value: "alice"},
			{Key: "email", Value: "alice@example.com"},
		}).Decode(&result)
		require.NoError(t, err, "dumboTag create must succeed")

		assert.EqualValues(t, 1, result["ok"], "ok must be 1")

		tags, ok := result["tags"].(bson.A)
		require.True(t, ok, "tags must be an array")
		require.Len(t, tags, 1, "create returns single-element array")

		tag := tags[0].(bson.M)
		assert.Equal(t, "v-head", tag["name"], "tag name must match")
		assert.Equal(t, hash2, tag["commitId"], "tag must point at current HEAD (hash2)")
		assert.Equal(t, "alice", tag["tagger"], "tagger must echo provided author")
		assert.Equal(t, "alice@example.com", tag["email"], "email must echo provided value")
		assert.Equal(t, "tag at current head", tag["message"], "message must echo provided value")
		assert.NotNil(t, tag["timestamp"], "timestamp must be present")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: Create a tag at a specific commit hash
	// -------------------------------------------------------------------------
	t.Run("Scenario2_CreateTagAtSpecificHash", func(t *testing.T) {
		var result bson.M
		err := env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: int32(1)},
			{Key: "name", Value: "v1.0"},
			{Key: "hash", Value: hash1},
			{Key: "message", Value: "first release"},
		}).Decode(&result)
		require.NoError(t, err, "dumboTag create at specific hash must succeed")

		assert.EqualValues(t, 1, result["ok"])

		tags := result["tags"].(bson.A)
		require.Len(t, tags, 1)

		tag := tags[0].(bson.M)
		assert.Equal(t, "v1.0", tag["name"])
		assert.Equal(t, hash1, tag["commitId"], "tag must point at hash1, not HEAD")
		assert.Equal(t, "dumbodb", tag["tagger"], "tagger must default to 'dumbodb'")
		assert.Equal(t, "dumbodb@dumbodb", tag["email"], "email must default to 'dumbodb@dumbodb'")
		assert.Equal(t, "first release", tag["message"])
	})

	// -------------------------------------------------------------------------
	// Scenario 3: List all tags
	// -------------------------------------------------------------------------
	t.Run("Scenario3_ListAllTags", func(t *testing.T) {
		var result bson.M
		err := env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: int32(1)},
		}).Decode(&result)
		require.NoError(t, err, "dumboTag list must succeed")

		assert.EqualValues(t, 1, result["ok"])

		tags, ok := result["tags"].(bson.A)
		require.True(t, ok)
		require.Len(t, tags, 2, "must list both v-head and v1.0")

		// Find each tag by name (order not guaranteed).
		vHead := tagFromList(t, tags, "v-head")
		assert.Equal(t, hash2, vHead["commitId"])
		assert.Equal(t, "alice", vHead["tagger"])
		assert.Equal(t, "alice@example.com", vHead["email"])
		assert.Equal(t, "tag at current head", vHead["message"])

		v10 := tagFromList(t, tags, "v1.0")
		assert.Equal(t, hash1, v10["commitId"])
		assert.Equal(t, "dumbodb", v10["tagger"])
		assert.Equal(t, "first release", v10["message"])
	})

	// -------------------------------------------------------------------------
	// Scenario 4: Delete a tag
	// -------------------------------------------------------------------------
	t.Run("Scenario4_DeleteTag", func(t *testing.T) {
		var result bson.M
		err := env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: int32(1)},
			{Key: "name", Value: "v-head"},
			{Key: "delete", Value: true},
		}).Decode(&result)
		require.NoError(t, err, "dumboTag delete must succeed")

		assert.EqualValues(t, 1, result["ok"])

		tags := result["tags"].(bson.A)
		require.Len(t, tags, 1)
		deleted := tags[0].(bson.M)
		assert.Equal(t, "v-head", deleted["name"], "deleted tag name must be echoed")
		assert.Equal(t, hash2, deleted["commitId"], "deleted tag commitId must be echoed")

		// Verify it's gone from the list.
		var listResult bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: int32(1)},
		}).Decode(&listResult))

		listTags := listResult["tags"].(bson.A)
		require.Len(t, listTags, 1, "only v1.0 should remain")
		assert.Equal(t, "v1.0", listTags[0].(bson.M)["name"])
	})

	// -------------------------------------------------------------------------
	// Scenario 7: Duplicate tag name is rejected
	// -------------------------------------------------------------------------
	t.Run("Scenario7_DuplicateTagRejected", func(t *testing.T) {
		var result bson.M
		err := env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: int32(1)},
			{Key: "name", Value: "v1.0"},
			{Key: "hash", Value: hash2},
		}).Decode(&result)
		assert.Error(t, err, "creating a duplicate tag must fail")

		// Verify original tag is unchanged.
		var listResult bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: int32(1)},
		}).Decode(&listResult))

		v10 := tagFromList(t, listResult["tags"].(bson.A), "v1.0")
		assert.Equal(t, hash1, v10["commitId"], "v1.0 must still point at hash1")
	})

	// -------------------------------------------------------------------------
	// Scenario 8: Deleting a nonexistent tag is rejected
	// -------------------------------------------------------------------------
	t.Run("Scenario8_DeleteNonexistentTagRejected", func(t *testing.T) {
		var result bson.M
		err := env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: int32(1)},
			{Key: "name", Value: "does-not-exist"},
			{Key: "delete", Value: true},
		}).Decode(&result)
		assert.Error(t, err, "deleting a nonexistent tag must fail")
	})

	// -------------------------------------------------------------------------
	// Scenario 9: Invalid tag names
	// -------------------------------------------------------------------------
	t.Run("Scenario9_InvalidTagNames", func(t *testing.T) {
		// @ is rejected
		var result1 bson.M
		err := env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: int32(1)},
			{Key: "name", Value: "bad@name"},
		}).Decode(&result1)
		assert.Error(t, err, "tag name with @ must be rejected")

		// Whitespace is rejected
		var result2 bson.M
		err = env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: int32(1)},
			{Key: "name", Value: "bad name"},
		}).Decode(&result2)
		assert.Error(t, err, "tag name with whitespace must be rejected")
	})

	// -------------------------------------------------------------------------
	// Scenario 2b: Create tag using ancestor expression
	// -------------------------------------------------------------------------
	t.Run("Scenario2b_TagWithAncestorExpression", func(t *testing.T) {
		var result bson.M
		err := env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: int32(1)},
			{Key: "name", Value: "v-ancestor"},
			{Key: "hash", Value: "main~1"},
			{Key: "message", Value: "ancestor tag"},
		}).Decode(&result)
		require.NoError(t, err, "dumboTag with ancestor expression must succeed")

		tags := result["tags"].(bson.A)
		tag := tags[0].(bson.M)
		assert.Equal(t, "v-ancestor", tag["name"])
		assert.Equal(t, hash1, tag["commitId"], "main~1 must resolve to hash1")
	})

	// -------------------------------------------------------------------------
	// Scenario 2c: Create tag using branch name as hash
	// -------------------------------------------------------------------------
	t.Run("Scenario2c_TagWithBranchName", func(t *testing.T) {
		var result bson.M
		err := env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: int32(1)},
			{Key: "name", Value: "v-branch"},
			{Key: "hash", Value: "main"},
			{Key: "message", Value: "tagged via branch name"},
		}).Decode(&result)
		require.NoError(t, err, "dumboTag with branch name as hash must succeed")

		tags := result["tags"].(bson.A)
		tag := tags[0].(bson.M)
		assert.Equal(t, hash2, tag["commitId"], "main must resolve to hash2 (current HEAD)")
	})

	// -------------------------------------------------------------------------
	// Scenario 2d: Create tag using another tag as hash
	// -------------------------------------------------------------------------
	t.Run("Scenario2d_TagFromTag", func(t *testing.T) {
		var result bson.M
		err := env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dumboTag", Value: int32(1)},
			{Key: "name", Value: "v-from-tag"},
			{Key: "hash", Value: "v1.0"},
			{Key: "message", Value: "tagged via another tag"},
		}).Decode(&result)
		require.NoError(t, err, "dumboTag with another tag as hash must succeed")

		tags := result["tags"].(bson.A)
		tag := tags[0].(bson.M)
		assert.Equal(t, hash1, tag["commitId"], "v1.0 resolves to hash1")
	})
}
