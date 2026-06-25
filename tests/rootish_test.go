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
)


// TestRootish_ParseRejection verifies that reflog and range rootish
// forms are rejected at parse time with code 96. HEAD, HEAD~N, and HEAD^N
// are accepted and therefore do NOT appear in this list.
func TestRootish_ParseRejection(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	dbName := fmt.Sprintf("prtest%d", rand.Int64N(1_000_000))

	cases := []struct {
		name    string
		rootish string
	}{
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

			encoded := dbName + "@" + tc.rootish
			assertRootishRejected(t, env.Client.Database(encoded), tc.rootish)
		})
	}
}

// TestRootish_AllDigitSuffix_TreatedAsPlainDB verifies that a database name whose
// @ suffix is an all-digit string (e.g. a UnixNano timestamp from test harnesses)
// is treated as a plain database name rather than failing with "not found as branch
// or tag". This guards against clients accidentally producing database names like
// "prefix@1775505756999075683" that would otherwise be misinterpreted as a branch.
func TestRootish_AllDigitSuffix_TreatedAsPlainDB(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()

	// Simulate a database name containing @ followed by an all-digit timestamp.
	dbName := "parityreg_sometest@1775505756999075683"
	coll := env.Client.Database(dbName).Collection("col")

	// Insert must succeed  -- the numeric suffix must NOT be misinterpreted as a branch.
	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "hello"}})
	require.NoError(t, err, "insert to all-digit-suffix DB must not fail with branch-not-found")

	// Find must return the inserted doc.
	cur, err := coll.Find(ctx, bson.D{})
	require.NoError(t, err, "find on all-digit-suffix DB must not fail")
	var docs []bson.D
	require.NoError(t, cur.All(ctx, &docs))
	require.Len(t, docs, 1)
	assert.Equal(t, int32(1), dmap(docs[0])["_id"])
}

// TestRootish_CommitHash_DataIsolation is a focused end-to-end test of snapshot
// isolation: the hash rootish sees exactly the data from that commit, regardless
// of subsequent writes to main.
func TestRootish_CommitHash_DataIsolation(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("diso%d", rand.Int64N(1_000_000))
	collName := "col"

	// hash1 has 1 doc; HEAD (after setup) has 2 docs.
	hash1 := setupVersioningDB(t, env, dbName, collName)

	snapColl := env.Client.Database(dbName + "@" + hash1).Collection(collName)
	mainColl := env.Client.Database(dbName).Collection(collName)

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
