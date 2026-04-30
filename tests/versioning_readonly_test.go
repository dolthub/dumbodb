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
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// dumboDBCommit runs dumboDBCommit on the given database and returns the commit hash.
func dumboDBCommit(tb testing.TB, env *dumboDBTestEnv, dbName, message string, author ...string) string {
	tb.Helper()

	a := "testuser"
	if len(author) > 0 {
		a = author[0]
	}

	ctx := context.Background()
	var result bson.M
	err := env.client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: int32(1)},
		{Key: "message", Value: message},
		{Key: "author", Value: a},
	}).Decode(&result)
	require.NoError(tb, err, "doltCommit must succeed")

	hash, ok := result["commitId"].(string)
	require.True(tb, ok, "doltCommit must return a string hash, got %T", result["commitId"])
	require.NotEmpty(tb, hash, "commit hash must not be empty")
	return hash
}

// dumboDBCommitAllowEmpty runs dumboDBCommit with allowEmpty:true so it succeeds even
// when the working set has no pending changes versus HEAD.
func dumboDBCommitAllowEmpty(tb testing.TB, env *dumboDBTestEnv, dbName, message string, author ...string) string {
	tb.Helper()

	a := "testuser"
	if len(author) > 0 {
		a = author[0]
	}

	ctx := context.Background()
	var result bson.M
	err := env.client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: int32(1)},
		{Key: "message", Value: message},
		{Key: "author", Value: a},
		{Key: "allowEmpty", Value: true},
	}).Decode(&result)
	require.NoError(tb, err, "doltCommit (allowEmpty) must succeed")

	hash, ok := result["commitId"].(string)
	require.True(tb, ok, "doltCommit must return a string hash, got %T", result["commitId"])
	require.NotEmpty(tb, hash, "commit hash must not be empty")
	return hash
}

// assertWriteBlockedOperationFailed verifies that the error is a MongoDB
// CommandError with code 96 (OperationFailed), as expected for writes to
// read-only rootish connections.
func assertWriteBlockedOperationFailed(tb testing.TB, err error, op string) {
	tb.Helper()

	require.Error(tb, err, "%s on read-only rootish must return an error", op)

	cmdErr, ok := err.(mongo.CommandError)
	require.True(tb, ok, "%s: expected mongo.CommandError, got %T: %v", op, err, err)
	assert.EqualValues(tb, 96, cmdErr.Code,
		"%s: expected OperationFailed (96), got code %d: %s", op, cmdErr.Code, cmdErr.Message)
}

// setupVersioningDB creates a database with two commits and returns the first
// commit hash. After setup:
//   - Commit 1 (returned hash): has doc {_id:1, v:"first"}
//   - Commit 2 (HEAD):          has doc {_id:1, v:"first"} + {_id:2, v:"second"}
func setupVersioningDB(tb testing.TB, env *dumboDBTestEnv, dbName, collName string) string {
	tb.Helper()

	ctx := context.Background()
	db := env.client.Database(dbName)

	// First document on main.
	_, err := db.Collection(collName).InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "v", Value: "first"},
	})
	require.NoError(tb, err)

	// First commit: contains only doc {_id:1}.
	hash1 := dumboDBCommit(tb, env, dbName, "first commit")

	// Second document.
	_, err = db.Collection(collName).InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "v", Value: "second"},
	})
	require.NoError(tb, err)

	// Second commit: contains doc {_id:1} + {_id:2}.
	dumboDBCommit(tb, env, dbName, "second commit")

	return hash1
}

// TestReadOnlyRootish_CommitHash_WritesBlocked verifies that all write
// operations return OperationFailed (code 96) when issued against a
// commit-hash rootish ("dbname@<32-char-hash>").
func TestReadOnlyRootish_CommitHash_WritesBlocked(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()

	// Base name must be ≤ 29 chars so that "dbname@<32-char-hash>" fits within the 63-char limit.
	dbName := fmt.Sprintf("roh%d", rand.Int64N(1_000_000))
	collName := "col"

	hash1 := setupVersioningDB(t, env, dbName, collName)

	// Connect via the commit-hash rootish: "dbname@<hash>".
	snapshotDBName := dbName + "@" + hash1
	snapshotDB := env.client.Database(snapshotDBName)
	snapshotColl := snapshotDB.Collection(collName)

	t.Run("insert", func(t *testing.T) {
		t.Parallel()

		_, err := snapshotColl.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}})
		assertWriteBlockedOperationFailed(t, err, "insert")
	})

	t.Run("updateOne", func(t *testing.T) {
		t.Parallel()

		_, err := snapshotColl.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "changed"}}}},
		)
		assertWriteBlockedOperationFailed(t, err, "updateOne")
	})

	t.Run("deleteOne", func(t *testing.T) {
		t.Parallel()

		_, err := snapshotColl.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
		assertWriteBlockedOperationFailed(t, err, "deleteOne")
	})

	t.Run("drop", func(t *testing.T) {
		t.Parallel()

		err := snapshotColl.Drop(ctx)
		assertWriteBlockedOperationFailed(t, err, "drop")
	})

	t.Run("createCollection", func(t *testing.T) {
		t.Parallel()

		err := snapshotDB.CreateCollection(ctx, "newcol")
		assertWriteBlockedOperationFailed(t, err, "createCollection")
	})
}

// TestReadOnlyRootish_CommitHash_ReadsSucceed verifies that read operations
// succeed and return the correct snapshot data when issued against a
// commit-hash rootish.
func TestReadOnlyRootish_CommitHash_ReadsSucceed(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()

	// Base name must be ≤ 29 chars so that "dbname@<32-char-hash>" fits within the 63-char limit.
	dbName := fmt.Sprintf("rhr%d", rand.Int64N(1_000_000))
	collName := "col"

	// After setup: hash1 has 1 doc; HEAD (commit 2) has 2 docs.
	hash1 := setupVersioningDB(t, env, dbName, collName)

	snapshotDBName := dbName + "@" + hash1
	snapshotColl := env.client.Database(snapshotDBName).Collection(collName)

	t.Run("find", func(t *testing.T) {
		t.Parallel()

		cursor, err := snapshotColl.Find(ctx, bson.D{})
		require.NoError(t, err, "find on commit-hash rootish must succeed")
		defer cursor.Close(ctx) //nolint:errcheck

		var docs []bson.D
		require.NoError(t, cursor.All(ctx, &docs))
		assert.Len(t, docs, 1, "snapshot at hash1 must contain exactly 1 document")
	})

	t.Run("aggregate", func(t *testing.T) {
		t.Parallel()

		cursor, err := snapshotColl.Aggregate(ctx, bson.A{
			bson.D{{Key: "$match", Value: bson.D{}}},
		})
		require.NoError(t, err, "aggregate on commit-hash rootish must succeed")
		defer cursor.Close(ctx) //nolint:errcheck

		var docs []bson.D
		require.NoError(t, cursor.All(ctx, &docs))
		assert.Len(t, docs, 1, "aggregate on snapshot must return 1 doc")
	})

	t.Run("countDocuments", func(t *testing.T) {
		t.Parallel()

		n, err := snapshotColl.CountDocuments(ctx, bson.D{})
		require.NoError(t, err, "countDocuments on commit-hash rootish must succeed")
		assert.Equal(t, int64(1), n, "count at hash1 must be 1")
	})
}

// TestReadOnlyRootish_AncestorExpr_WritesBlocked verifies that all write
// operations return OperationFailed (code 96) when issued against a
// relative-ancestor rootish ("dbname@branch~N").
func TestReadOnlyRootish_AncestorExpr_WritesBlocked(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("testdb_ronly_anc_%d", rand.Int64N(1_000_000))
	collName := "col"

	// Create two commits so main~1 (HEAD-1) exists.
	setupVersioningDB(t, env, dbName, collName)

	// Connect via ancestor expression: "dbname@main~1".
	ancestorDBName := dbName + "@main~1"
	ancestorDB := env.client.Database(ancestorDBName)
	ancestorColl := ancestorDB.Collection(collName)

	t.Run("insert", func(t *testing.T) {
		t.Parallel()

		_, err := ancestorColl.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}})
		assertWriteBlockedOperationFailed(t, err, "insert")
	})

	t.Run("updateOne", func(t *testing.T) {
		t.Parallel()

		_, err := ancestorColl.UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "changed"}}}},
		)
		assertWriteBlockedOperationFailed(t, err, "updateOne")
	})

	t.Run("deleteOne", func(t *testing.T) {
		t.Parallel()

		_, err := ancestorColl.DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
		assertWriteBlockedOperationFailed(t, err, "deleteOne")
	})

	t.Run("drop", func(t *testing.T) {
		t.Parallel()

		err := ancestorColl.Drop(ctx)
		assertWriteBlockedOperationFailed(t, err, "drop")
	})

	t.Run("createCollection", func(t *testing.T) {
		t.Parallel()

		err := ancestorDB.CreateCollection(ctx, "newcol")
		assertWriteBlockedOperationFailed(t, err, "createCollection")
	})
}

// TestReadOnlyRootish_AncestorExpr_ReadsSucceed verifies that read operations
// succeed against a relative-ancestor rootish and return the snapshot data at
// that ancestor commit.
func TestReadOnlyRootish_AncestorExpr_ReadsSucceed(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("testdb_ronly_anc_reads_%d", rand.Int64N(1_000_000))
	collName := "col"

	// Create two commits.  main~1 is the first commit (1 doc); HEAD has 2 docs.
	setupVersioningDB(t, env, dbName, collName)

	// main~1 = one commit back from HEAD = first commit.
	ancestorColl := env.client.Database(dbName + "@main~1").Collection(collName)

	t.Run("find", func(t *testing.T) {
		t.Parallel()

		cursor, err := ancestorColl.Find(ctx, bson.D{})
		require.NoError(t, err, "find on ancestor rootish must succeed")
		defer cursor.Close(ctx) //nolint:errcheck

		var docs []bson.D
		require.NoError(t, cursor.All(ctx, &docs))
		assert.Len(t, docs, 1, "main~1 must see exactly 1 document (first commit)")
	})

	t.Run("aggregate", func(t *testing.T) {
		t.Parallel()

		cursor, err := ancestorColl.Aggregate(ctx, bson.A{
			bson.D{{Key: "$match", Value: bson.D{}}},
		})
		require.NoError(t, err, "aggregate on ancestor rootish must succeed")
		defer cursor.Close(ctx) //nolint:errcheck

		var docs []bson.D
		require.NoError(t, cursor.All(ctx, &docs))
		assert.Len(t, docs, 1, "aggregate at main~1 must return 1 doc")
	})

	t.Run("countDocuments", func(t *testing.T) {
		t.Parallel()

		n, err := ancestorColl.CountDocuments(ctx, bson.D{})
		require.NoError(t, err, "countDocuments on ancestor rootish must succeed")
		assert.Equal(t, int64(1), n, "count at main~1 must be 1")
	})
}
