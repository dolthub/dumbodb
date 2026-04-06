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
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// assertRootishRejected verifies that any operation on an invalid rootish
// returns OperationFailed (code 96) at parse time.
func assertRootishRejected(tb testing.TB, db *mongo.Database, op string) {
	tb.Helper()

	ctx := context.Background()
	_, err := db.Collection("col").Find(ctx, bson.D{})
	require.Error(tb, err, "%s: expected parse-time rejection", op)

	cmdErr, ok := err.(mongo.CommandError)
	require.True(tb, ok, "%s: expected CommandError, got %T: %v", op, err, err)
	assert.EqualValues(tb, 96, cmdErr.Code,
		"%s: expected OperationFailed (96), got %d: %s", op, cmdErr.Code, cmdErr.Message)
}

// TestRootish_ParseRejection verifies that HEAD, HEAD-relative, reflog, and
// range rootish forms are rejected at parse time with code 96.
func TestRootish_ParseRejection(t *testing.T) {
	t.Parallel()

	env := startDocudolt(t)
	dbName := fmt.Sprintf("prtest%d", rand.Int64N(1_000_000))

	cases := []struct {
		name    string
		rootish string
	}{
		{"HEAD", "HEAD"},
		{"HEAD~1", "HEAD~1"},
		{"HEAD~2", "HEAD~2"},
		{"reflog_yesterday", "main@{yesterday}"},
		{"reflog_minutes_ago", "main@{5 minutes ago}"},
		{"reflog_bare", "@{1}"},
		{"range_two_dot", "main..feature"},
		{"range_three_dot", "main...feature"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			encoded := dbName + "__d_" + tc.rootish
			assertRootishRejected(t, env.client.Database(encoded), tc.rootish)
		})
	}
}

// TestRootish_DocudoltCurrentBranch_EndToEnd verifies that docudoltCurrentBranch
// returns the correct branch on branch connections and code 96 on read-only ones.
func TestRootish_DocudoltCurrentBranch_EndToEnd(t *testing.T) {
	t.Parallel()

	env := startDocudolt(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dcbtest%d", rand.Int64N(1_000_000))
	collName := "col"

	// Create two commits so we have a real hash and a non-trivial ancestor.
	hash1 := setupVersioningDB(t, env, dbName, collName)

	t.Run("plain_db_returns_main", func(t *testing.T) {
		t.Parallel()

		var result bson.M
		err := env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Decode(&result)
		require.NoError(t, err)
		assert.Equal(t, "main", result["branch"])
		assert.EqualValues(t, 1, result["ok"])
	})

	t.Run("explicit_main_returns_main", func(t *testing.T) {
		t.Parallel()

		var result bson.M
		err := env.client.Database(dbName+"__d_main").RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Decode(&result)
		require.NoError(t, err)
		assert.Equal(t, "main", result["branch"])
	})

	t.Run("commit_hash_returns_code96", func(t *testing.T) {
		t.Parallel()

		err := env.client.Database(dbName+"__d_"+hash1).RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Err()
		assertWriteBlockedOperationFailed(t, err, "doltCurrentBranch on hash rootish")
	})

	t.Run("ancestor_expr_returns_code96", func(t *testing.T) {
		t.Parallel()

		err := env.client.Database(dbName+"__d_main~1").RunCommand(ctx, bson.D{
			{Key: "doltCurrentBranch", Value: int32(1)},
		}).Err()
		assertWriteBlockedOperationFailed(t, err, "doltCurrentBranch on ancestor rootish")
	})
}

// TestRootish_AllDigitSuffix_TreatedAsPlainDB verifies that a database name whose
// __ suffix is an all-digit string (e.g. a UnixNano timestamp from test harnesses)
// is treated as a plain database name rather than failing with "not found as branch
// or tag". This is a regression test for the parity CI failure where names like
// "parity_AdvancedQuery_Regex_CaseInsensitive__1775505756999075683" caused an
// InternalError on insert/find.
func TestRootish_AllDigitSuffix_TreatedAsPlainDB(t *testing.T) {
	t.Parallel()

	env := startDocudolt(t)
	ctx := context.Background()

	// Simulate a parity-harness-style database name: a prefix ending in _ joined to
	// a UnixNano timestamp, producing a __ separator by accident.
	// The format "prefix_%s_%d" with a sanitized name ending in _ yields prefix__timestamp.
	dbName := "parityreg_sometest__d_1775505756999075683"
	coll := env.client.Database(dbName).Collection("col")

	// Insert must succeed — the numeric suffix must NOT be misinterpreted as a branch.
	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "hello"}})
	require.NoError(t, err, "insert to all-digit-suffix DB must not fail with branch-not-found")

	// Find must return the inserted doc.
	cur, err := coll.Find(ctx, bson.D{})
	require.NoError(t, err, "find on all-digit-suffix DB must not fail")
	var docs []bson.D
	require.NoError(t, cur.All(ctx, &docs))
	require.Len(t, docs, 1)
	assert.Equal(t, int32(1), docs[0].Map()["_id"])
}

// TestRootish_CommitHash_DataIsolation is a focused end-to-end test of snapshot
// isolation: the hash rootish sees exactly the data from that commit, regardless
// of subsequent writes to main.
func TestRootish_CommitHash_DataIsolation(t *testing.T) {
	t.Parallel()

	env := startDocudolt(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("diso%d", rand.Int64N(1_000_000))
	collName := "col"

	// hash1 has 1 doc; HEAD (after setup) has 2 docs.
	hash1 := setupVersioningDB(t, env, dbName, collName)

	snapColl := env.client.Database(dbName+"__d_"+hash1).Collection(collName)
	mainColl := env.client.Database(dbName).Collection(collName)

	// Main has 2 docs.
	n, err := mainColl.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "main must have 2 docs")

	// Snapshot at hash1 has 1 doc.
	n, err = snapColl.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "hash1 snapshot must have 1 doc")

	// Writing more to main does not affect the snapshot.
	_, err = mainColl.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "v", Value: "third"}})
	require.NoError(t, err)

	n, err = snapColl.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "hash1 snapshot must still have 1 doc after further main writes")
}
