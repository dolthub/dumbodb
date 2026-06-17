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

// Handler-level coverage for dumboLog frontier pagination (workspace-tnf.3):
// the "from" parameter accepts a string or an array of hashes, and the
// response carries "next" until the walk is exhausted. Uses a linear history
// so the walk order is unambiguous regardless of commit timestamps.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// runLog issues a dumboLog command with the given extra fields and returns the
// decoded raw response.
func runLog(t *testing.T, env *dumboDBTestEnv, dbName string, extra bson.D) bson.M {
	t.Helper()
	cmd := append(bson.D{{Key: "doltLog", Value: int32(1)}}, extra...)
	var raw bson.M
	require.NoError(t, env.client.Database(dbName).RunCommand(context.Background(), cmd).Decode(&raw))
	return raw
}

// logCommitIDs extracts the ordered commit ids from a dumboLog response.
func logCommitIDs(t *testing.T, raw bson.M) []string {
	t.Helper()
	arr, ok := raw["commits"].(bson.A)
	require.True(t, ok, "commits should be an array")
	ids := make([]string, len(arr))
	for i, c := range arr {
		ids[i] = c.(bson.M)["commitId"].(string)
	}
	return ids
}

// logNext extracts the "next" frontier as a string slice (nil when absent).
func logNext(t *testing.T, raw bson.M) []string {
	t.Helper()
	v, ok := raw["next"]
	if !ok {
		return nil
	}
	arr, ok := v.(bson.A)
	require.True(t, ok, "next should be an array")
	out := make([]string, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		require.True(t, ok, "next elements should be strings")
		out[i] = s
	}
	return out
}

func TestLogPaginationHandler(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("logpage%d", rand.Int64N(1_000_000))
	require.NoError(t, env.client.Database(dbName).Drop(ctx))

	// Linear history: c1..c5 plus the Initialize root => 6 commits total.
	var hashes []string
	for i := 1; i <= 5; i++ {
		_, err := env.client.Database(dbName).Collection("c").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(i)}})
		require.NoError(t, err)
		hashes = append(hashes, dumboDBCommit(t, env, dbName, fmt.Sprintf("c%d", i), "a <a@x.io>"))
	}
	const total = 6 // 5 user commits + Initialize root

	t.Run("FromAbsent_PagesAndReassembles", func(t *testing.T) {
		var all []string
		seen := map[string]bool{}
		var from bson.A
		pages := 0
		for {
			pages++
			extra := bson.D{{Key: "limit", Value: int32(2)}}
			if from != nil {
				extra = append(extra, bson.E{Key: "from", Value: from})
			}
			raw := runLog(t, env, dbName, extra)
			for _, id := range logCommitIDs(t, raw) {
				require.False(t, seen[id], "commit %s emitted twice", id)
				seen[id] = true
				all = append(all, id)
			}
			next := logNext(t, raw)
			if len(next) == 0 {
				break
			}
			from = bson.A{}
			for _, h := range next {
				from = append(from, h)
			}
			require.Less(t, pages, 100, "pagination did not terminate")
		}
		require.Len(t, all, total, "all pages should cover the whole history exactly once")
	})

	t.Run("NextOmittedWhenExhausted", func(t *testing.T) {
		raw := runLog(t, env, dbName, bson.D{{Key: "limit", Value: int32(100)}})
		require.Len(t, logCommitIDs(t, raw), total)
		_, hasNext := raw["next"]
		assert.False(t, hasNext, "next must be omitted when the whole history fits")
	})

	t.Run("FromString_BackCompat", func(t *testing.T) {
		// Start at c3 (hashes[2]); reachable: c3, c2, c1, init => 4 commits.
		raw := runLog(t, env, dbName, bson.D{{Key: "from", Value: hashes[2]}})
		ids := logCommitIDs(t, raw)
		require.Len(t, ids, 4)
		assert.Equal(t, hashes[2], ids[0])
	})

	t.Run("FromArray_OneElementEqualsString", func(t *testing.T) {
		rawStr := runLog(t, env, dbName, bson.D{{Key: "from", Value: hashes[2]}})
		rawArr := runLog(t, env, dbName, bson.D{{Key: "from", Value: bson.A{hashes[2]}}})
		assert.Equal(t, logCommitIDs(t, rawStr), logCommitIDs(t, rawArr))
	})

	t.Run("Limit0_EmptyNoNext", func(t *testing.T) {
		raw := runLog(t, env, dbName, bson.D{{Key: "limit", Value: int32(0)}})
		assert.Empty(t, logCommitIDs(t, raw))
		_, hasNext := raw["next"]
		assert.False(t, hasNext)
	})

	t.Run("InvalidHashElement_Errors", func(t *testing.T) {
		cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "from", Value: bson.A{"notavalidhash"}}}
		err := env.client.Database(dbName).RunCommand(ctx, cmd).Err()
		require.Error(t, err)
	})

	t.Run("WrongTypeElement_Errors", func(t *testing.T) {
		cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "from", Value: bson.A{int32(123)}}}
		err := env.client.Database(dbName).RunCommand(ctx, cmd).Err()
		require.Error(t, err)
	})
}

func TestLogAllHandler(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("logall%d", rand.Int64N(1_000_000))
	require.NoError(t, env.client.Database(dbName).Drop(ctx))

	// main: one commit.
	_, err := env.client.Database(dbName).Collection("c").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "main-1", "a <a@x.io>")

	// side branch with a commit not reachable from main.
	require.NoError(t, env.client.Database(dbName+"@main").RunCommand(ctx,
		bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "branch", Value: "side"}}).Err())
	_, err = env.client.Database(dbName+"@side").Collection("c").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
	require.NoError(t, err)
	sideHash := dumboDBCommit(t, env, dbName+"@side", "side-1", "a <a@x.io>")

	t.Run("AllSpansBranches", func(t *testing.T) {
		def := logCommitIDs(t, runLog(t, env, dbName, bson.D{}))
		assert.NotContains(t, def, sideHash, "default main walk excludes side-only commit")

		all := logCommitIDs(t, runLog(t, env, dbName, bson.D{{Key: "all", Value: true}}))
		assert.Contains(t, all, sideHash, "all walk includes the side-branch commit")
	})

	t.Run("AllAndFromMutuallyExclusive", func(t *testing.T) {
		cmd := bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "all", Value: true}, {Key: "from", Value: sideHash}}
		require.Error(t, env.client.Database(dbName).RunCommand(ctx, cmd).Err())
	})
}
