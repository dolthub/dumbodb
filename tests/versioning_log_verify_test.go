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

// TestLogVerify is the automated analog of docs/verify/log.md.
//
// Each top-level subtest corresponds to one scenario in that document.
// The setup reproduces the manual setup block exactly:
//
//   - Three sequential commits on main: "first", "second", "third" (HEAD)
//   - hash1 → hash2 → hash3 form the parent chain
//
// Note: every Dongo database begins with an auto-created "Initialize database"
// root commit. Counts below include that initial commit.
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
	"go.mongodb.org/mongo-driver/bson"
)

// logResult holds the decoded top-level response from a dongoLog command.
type logResult struct {
	Branch  string
	Commits []commitEntry
}

// commitEntry holds one entry from the "commits" array of a dongoLog response.
type commitEntry struct {
	Hash    string
	Parent1 string
	Parent2 string
	Message string
}

// decodeLogResult parses the raw bson.M from a dongoLog RunCommand into the
// typed helpers above, failing the test if the shape is unexpected.
func decodeLogResult(t *testing.T, raw bson.M) logResult {
	t.Helper()

	branch, _ := raw["branch"].(string)

	rawCommits, ok := raw["commits"]
	require.True(t, ok, "dongoLog result missing 'commits' field")

	commitsArr, ok := rawCommits.(bson.A)
	require.True(t, ok, "dongoLog 'commits' is not an array, got %T", rawCommits)

	var out logResult
	out.Branch = branch

	for _, c := range commitsArr {
		cm, ok := c.(bson.M)
		require.True(t, ok, "commits entry is not a document, got %T", c)

		entry := commitEntry{
			Hash:    fmt.Sprintf("%v", cm["hash"]),
			Message: fmt.Sprintf("%v", cm["message"]),
		}
		if p1, ok := cm["parent1"]; ok {
			entry.Parent1 = fmt.Sprintf("%v", p1)
		}
		if p2, ok := cm["parent2"]; ok {
			entry.Parent2 = fmt.Sprintf("%v", p2)
		}
		out.Commits = append(out.Commits, entry)
	}

	return out
}

func TestLogVerify(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()

	// Randomised db name so parallel test runs don't collide.
	dbName := fmt.Sprintf("logvrfy%d", rand.Int64N(1_000_000))

	// -------------------------------------------------------------------------
	// Scenario 1: Log with no user commits — only the "Initialize database" root
	// -------------------------------------------------------------------------
	t.Run("Scenario1_NoUserCommits", func(t *testing.T) {
		// Insert a document to create the database (triggers the Initialize commit)
		// but do not commit, so no user commits exist yet.
		require.NoError(t, env.client.Database(dbName).Drop(ctx))
		_, err := env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(0)},
		})
		require.NoError(t, err)

		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dongoLog", Value: int32(1)},
		}).Decode(&raw))

		lr := decodeLogResult(t, raw)
		// Exactly 1 commit: the auto-created "Initialize database" root.
		require.Len(t, lr.Commits, 1, "expected exactly 1 commit (Initialize database)")
		assert.Equal(t, "Initialize database", lr.Commits[0].Message)
		assert.Empty(t, lr.Commits[0].Parent1, "Initialize commit is the root — no parent1")

		// Clean up the uncommitted insert so the setup below starts fresh.
		require.NoError(t, env.client.Database(dbName).Drop(ctx))
	})

	// Setup: three sequential commits.
	_, err := env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "label", Value: "alpha"},
	})
	require.NoError(t, err)
	hash1 := dongoCommit(t, env, dbName, "first")

	_, err = env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "label", Value: "beta"},
	})
	require.NoError(t, err)
	hash2 := dongoCommit(t, env, dbName, "second")

	_, err = env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(3)},
		{Key: "label", Value: "gamma"},
	})
	require.NoError(t, err)
	hash3 := dongoCommit(t, env, dbName, "third")

	// -------------------------------------------------------------------------
	// Scenario 2: Log after one commit — user commit plus root
	// -------------------------------------------------------------------------
	t.Run("Scenario2_SingleUserCommit", func(t *testing.T) {
		singleDB := fmt.Sprintf("logsingle%d", rand.Int64N(1_000_000))

		_, err := env.client.Database(singleDB).Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(1)},
			{Key: "v", Value: int32(1)},
		})
		require.NoError(t, err)
		singleHash := dongoCommit(t, env, singleDB, "only commit")

		var raw bson.M
		require.NoError(t, env.client.Database(singleDB).RunCommand(ctx, bson.D{
			{Key: "dongoLog", Value: int32(1)},
		}).Decode(&raw))

		lr := decodeLogResult(t, raw)
		// 2 commits: "only commit" on top, "Initialize database" as root.
		require.Len(t, lr.Commits, 2, "expected 2 commits (user commit + Initialize root)")
		assert.Equal(t, singleHash, lr.Commits[0].Hash, "first commit must be the user commit")
		assert.Equal(t, "only commit", lr.Commits[0].Message)
		assert.NotEmpty(t, lr.Commits[0].Parent1, "user commit must have parent1 (the Initialize root)")

		// The root Initialize commit has no parent.
		assert.Equal(t, "Initialize database", lr.Commits[1].Message)
		assert.Empty(t, lr.Commits[1].Parent1, "Initialize commit is the root — no parent1")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: Log after multiple commits — parent chain, newest-first
	// -------------------------------------------------------------------------
	t.Run("Scenario3_MultipleCommits", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dongoLog", Value: int32(1)},
		}).Decode(&raw))

		lr := decodeLogResult(t, raw)
		// 4 commits: "third", "second", "first", "Initialize database".
		require.Len(t, lr.Commits, 4, "expected 4 commits (3 user + Initialize root)")

		// Newest-first order for the user commits.
		assert.Equal(t, hash3, lr.Commits[0].Hash, "commits[0] must be hash3 (HEAD)")
		assert.Equal(t, "third", lr.Commits[0].Message)
		assert.Equal(t, hash2, lr.Commits[0].Parent1, "commits[0].parent1 must be hash2")

		assert.Equal(t, hash2, lr.Commits[1].Hash, "commits[1] must be hash2")
		assert.Equal(t, "second", lr.Commits[1].Message)
		assert.Equal(t, hash1, lr.Commits[1].Parent1, "commits[1].parent1 must be hash1")

		assert.Equal(t, hash1, lr.Commits[2].Hash, "commits[2] must be hash1")
		assert.Equal(t, "first", lr.Commits[2].Message)
		assert.NotEmpty(t, lr.Commits[2].Parent1, "hash1 must have a parent1 (the Initialize root)")

		// Root Initialize commit.
		assert.Equal(t, "Initialize database", lr.Commits[3].Message)
		assert.Empty(t, lr.Commits[3].Parent1, "Initialize commit is the root — no parent1")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: Log with limit — truncates at the specified count
	// -------------------------------------------------------------------------
	t.Run("Scenario4_WithLimit", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dongoLog", Value: int32(1)},
			{Key: "limit", Value: int32(2)},
		}).Decode(&raw))

		lr := decodeLogResult(t, raw)
		require.Len(t, lr.Commits, 2, "expected exactly 2 commits with limit=2")
		assert.Equal(t, hash3, lr.Commits[0].Hash, "first entry must be hash3 (HEAD)")
		assert.Equal(t, hash2, lr.Commits[1].Hash, "second entry must be hash2")

		// hash1 ("first") and Initialize must not appear.
		for _, c := range lr.Commits {
			assert.NotEqual(t, hash1, c.Hash, "hash1 must not appear when limit=2")
		}
	})

	// -------------------------------------------------------------------------
	// Scenario 5: Log from a specific hash — start traversal at that commit
	// -------------------------------------------------------------------------
	t.Run("Scenario5_FromHash", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "dongoLog", Value: int32(1)},
			{Key: "from", Value: hash2},
		}).Decode(&raw))

		lr := decodeLogResult(t, raw)
		// 3 commits when starting from hash2: hash2, hash1, Initialize.
		require.Len(t, lr.Commits, 3, "expected 3 commits when starting from hash2 (hash2, hash1, Initialize)")
		assert.Equal(t, hash2, lr.Commits[0].Hash, "first entry must be hash2")
		assert.Equal(t, "second", lr.Commits[0].Message)
		assert.Equal(t, hash1, lr.Commits[1].Hash, "second entry must be hash1")
		assert.Equal(t, "first", lr.Commits[1].Message)
		assert.Equal(t, "Initialize database", lr.Commits[2].Message, "third entry must be Initialize root")
		assert.Empty(t, lr.Commits[2].Parent1, "Initialize commit is the root — no parent1")

		// hash3 ("third") must not appear.
		for _, c := range lr.Commits {
			assert.NotEqual(t, hash3, c.Hash, "hash3 must not appear when from=hash2")
		}
	})

	_ = hash3 // used in subtests above
}
