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
//
// Scenario 1 runs first on the fresh database (before any user commits).
// The setup block then creates three commits on the same database.
// Scenarios 2–4 use that shared three-commit history.
//
// Note: every Docudolt database begins with an auto-created "Initialize database"
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

// logResult holds the decoded top-level response from a docudoltLog command.
type logResult struct {
	Branch  string
	Commits []commitEntry
}

// commitEntry holds one entry from the "commits" array of a docudoltLog response.
type commitEntry struct {
	CommitID string
	Parent1  string
	Parent2  string
	Message  string
	Author   string
	Refs     []string
}

// decodeLogResult parses the raw bson.M from a docudoltLog RunCommand into the
// typed helpers above, failing the test if the shape is unexpected.
func decodeLogResult(t *testing.T, raw bson.M) logResult {
	t.Helper()

	branch, _ := raw["branch"].(string)

	rawCommits, ok := raw["commits"]
	require.True(t, ok, "docudoltLog result missing 'commits' field")

	commitsArr, ok := rawCommits.(bson.A)
	require.True(t, ok, "docudoltLog 'commits' is not an array, got %T", rawCommits)

	var out logResult
	out.Branch = branch

	for _, c := range commitsArr {
		cm, ok := c.(bson.M)
		require.True(t, ok, "commits entry is not a document, got %T", c)

		entry := commitEntry{
			CommitID: fmt.Sprintf("%v", cm["commitId"]),
			Message:  fmt.Sprintf("%v", cm["message"]),
			Author:   fmt.Sprintf("%v", cm["author"]),
		}
		if p1, ok := cm["parent1"]; ok {
			entry.Parent1 = fmt.Sprintf("%v", p1)
		}
		if p2, ok := cm["parent2"]; ok {
			entry.Parent2 = fmt.Sprintf("%v", p2)
		}
		if rawRefs, ok := cm["refs"]; ok {
			if refsArr, ok := rawRefs.(bson.A); ok {
				for _, r := range refsArr {
					entry.Refs = append(entry.Refs, fmt.Sprintf("%v", r))
				}
			}
		}
		out.Commits = append(out.Commits, entry)
	}

	return out
}

func TestLogVerify(t *testing.T) {
	env := startDocudolt(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("logvrfy%d", rand.Int64N(1_000_000))

	// Start fresh.
	require.NoError(t, env.client.Database(dbName).Drop(ctx))
	_, err := env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(0)},
	})
	require.NoError(t, err)

	// -------------------------------------------------------------------------
	// Scenario 1: Log before any user commits — only the "Initialize database" root
	// -------------------------------------------------------------------------
	t.Run("Scenario1_NoUserCommits", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltLog", Value: int32(1)},
		}).Decode(&raw))

		lr := decodeLogResult(t, raw)
		require.Len(t, lr.Commits, 1, "expected exactly 1 commit (Initialize database)")
		assert.Equal(t, "Initialize database", lr.Commits[0].Message)
		assert.Empty(t, lr.Commits[0].Parent1, "Initialize commit is the root — no parent1")
	})

	// Setup: three sequential commits on the same database.
	_, err = env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "label", Value: "alpha"},
	})
	require.NoError(t, err)
	hash1 := docudoltCommit(t, env, dbName, "first")

	_, err = env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "label", Value: "beta"},
	})
	require.NoError(t, err)
	hash2 := docudoltCommit(t, env, dbName, "second")

	_, err = env.client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(3)},
		{Key: "label", Value: "gamma"},
	})
	require.NoError(t, err)
	hash3 := docudoltCommit(t, env, dbName, "third")

	// -------------------------------------------------------------------------
	// Scenario 2: Log after multiple commits — parent chain, newest-first
	// -------------------------------------------------------------------------
	t.Run("Scenario2_MultipleCommits", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltLog", Value: int32(1)},
		}).Decode(&raw))

		lr := decodeLogResult(t, raw)
		// 4 commits: "third", "second", "first", "Initialize database".
		require.Len(t, lr.Commits, 4, "expected 4 commits (3 user + Initialize root)")

		assert.Equal(t, hash3, lr.Commits[0].CommitID, "commits[0] must be hash3 (HEAD)")
		assert.Equal(t, "third", lr.Commits[0].Message)
		assert.Equal(t, hash2, lr.Commits[0].Parent1, "commits[0].parent1 must be hash2")

		assert.Equal(t, hash2, lr.Commits[1].CommitID, "commits[1] must be hash2")
		assert.Equal(t, "second", lr.Commits[1].Message)
		assert.Equal(t, hash1, lr.Commits[1].Parent1, "commits[1].parent1 must be hash1")

		assert.Equal(t, hash1, lr.Commits[2].CommitID, "commits[2] must be hash1")
		assert.Equal(t, "first", lr.Commits[2].Message)
		assert.NotEmpty(t, lr.Commits[2].Parent1, "hash1 must have a parent1 (the Initialize root)")

		assert.Equal(t, "Initialize database", lr.Commits[3].Message)
		assert.Empty(t, lr.Commits[3].Parent1, "Initialize commit is the root — no parent1")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: Log with limit — truncates at the specified count
	// -------------------------------------------------------------------------
	t.Run("Scenario3_WithLimit", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltLog", Value: int32(1)},
			{Key: "limit", Value: int32(2)},
		}).Decode(&raw))

		lr := decodeLogResult(t, raw)
		require.Len(t, lr.Commits, 2, "expected exactly 2 commits with limit=2")
		assert.Equal(t, hash3, lr.Commits[0].CommitID, "first entry must be hash3 (HEAD)")
		assert.Equal(t, hash2, lr.Commits[1].CommitID, "second entry must be hash2")

		for _, c := range lr.Commits {
			assert.NotEqual(t, hash1, c.CommitID, "hash1 must not appear when limit=2")
		}
	})

	// -------------------------------------------------------------------------
	// Scenario 4: Log from a specific hash — start traversal at that commit
	// -------------------------------------------------------------------------
	t.Run("Scenario4_FromHash", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.client.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "docudoltLog", Value: int32(1)},
			{Key: "from", Value: hash2},
		}).Decode(&raw))

		lr := decodeLogResult(t, raw)
		require.Len(t, lr.Commits, 3, "expected 3 commits starting from hash2 (hash2, hash1, Initialize)")
		assert.Equal(t, hash2, lr.Commits[0].CommitID, "first entry must be hash2")
		assert.Equal(t, "second", lr.Commits[0].Message)
		assert.Equal(t, hash1, lr.Commits[1].CommitID, "second entry must be hash1")
		assert.Equal(t, "first", lr.Commits[1].Message)
		assert.Equal(t, "Initialize database", lr.Commits[2].Message, "third entry must be Initialize root")
		assert.Empty(t, lr.Commits[2].Parent1, "Initialize commit is the root — no parent1")

		for _, c := range lr.Commits {
			assert.NotEqual(t, hash3, c.CommitID, "hash3 must not appear when from=hash2")
		}
	})

	// -------------------------------------------------------------------------
	// Scenario 5: Refs annotation — branch heads are decorated (git --decorate)
	// -------------------------------------------------------------------------
	t.Run("Scenario5_RefsAnnotation", func(t *testing.T) {
		// Create a second branch "logvrfy-refs" pointing at the current main HEAD
		// (hash3), so two branches share the same tip commit.
		require.NoError(t, env.client.Database(dbName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltBranch", Value: int32(1)},
			{Key: "branch", Value: "logvrfy-refs"},
		}).Err(), "creating logvrfy-refs branch must succeed")

		// Query docudoltLog on main.  hash3 is the tip of both "main" and
		// "logvrfy-refs", so its refs field must contain "HEAD", "main", and
		// "logvrfy-refs".  Non-head commits must have no refs field.
		var rawMain bson.M
		require.NoError(t, env.client.Database(dbName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltLog", Value: int32(1)},
		}).Decode(&rawMain))

		lrMain := decodeLogResult(t, rawMain)
		require.NotEmpty(t, lrMain.Commits, "docudoltLog on main must return commits")

		head := lrMain.Commits[0]
		assert.Equal(t, hash3, head.CommitID, "HEAD commit must be hash3")
		assert.Contains(t, head.Refs, "HEAD", "HEAD commit must carry 'HEAD' ref")
		assert.Contains(t, head.Refs, "main", "HEAD commit must carry 'main' ref")
		assert.Contains(t, head.Refs, "logvrfy-refs", "HEAD commit must carry bare 'logvrfy-refs' ref")

		// Non-head commits must carry no refs.
		for _, c := range lrMain.Commits[1:] {
			assert.Empty(t, c.Refs, "non-head commit %s must have no refs", c.CommitID)
		}

		// Query docudoltLog on logvrfy-refs.  hash3 is still the tip but now the
		// connection branch is "logvrfy-refs", so the decoration is reversed.
		var rawFeature bson.M
		require.NoError(t, env.client.Database(dbName+"__logvrfy-refs").RunCommand(ctx, bson.D{
			{Key: "docudoltLog", Value: int32(1)},
		}).Decode(&rawFeature))

		lrFeature := decodeLogResult(t, rawFeature)
		require.NotEmpty(t, lrFeature.Commits, "docudoltLog on logvrfy-refs must return commits")

		headF := lrFeature.Commits[0]
		assert.Equal(t, hash3, headF.CommitID, "HEAD commit on logvrfy-refs must be hash3")
		assert.Contains(t, headF.Refs, "HEAD", "must carry 'HEAD' ref")
		assert.Contains(t, headF.Refs, "logvrfy-refs", "must carry 'logvrfy-refs' ref")
		assert.Contains(t, headF.Refs, "main", "must carry bare 'main' ref")
	})

	// -------------------------------------------------------------------------
	// Scenarios 6–8: Non-linear commit graphs (merge commits with parent1+parent2)
	//
	// Fresh database with a true three-way merge:
	//
	//   init ← hashA ← hashB (main)
	//                ↖
	//                 hashC (feat)  →  hashM (merge, parent1=hashB, parent2=hashC)
	//
	// DocudoltLog follows parent1 linearly, so the walk from main is:
	//   hashM → hashB → hashA → init
	// The walk from hashC (feat tip) is:
	//   hashC → hashA → init  (hashB and hashM are unreachable)
	// -------------------------------------------------------------------------
	mergeDBName := fmt.Sprintf("logmerge%d", rand.Int64N(1_000_000))
	require.NoError(t, env.client.Database(mergeDBName).Drop(ctx))
	_, err = env.client.Database(mergeDBName).Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "v", Value: int32(1)},
	})
	require.NoError(t, err)
	hashA := docudoltCommit(t, env, mergeDBName, "add-one")

	// Create "feat" branch from main HEAD (hashA).
	require.NoError(t, env.client.Database(mergeDBName+"__main").RunCommand(ctx, bson.D{
		{Key: "docudoltBranch", Value: int32(1)},
		{Key: "branch", Value: "feat"},
	}).Err())

	// Advance main: _id:2 → hashB (diverges from hashA).
	_, err = env.client.Database(mergeDBName).Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "v", Value: int32(2)},
	})
	require.NoError(t, err)
	hashB := docudoltCommit(t, env, mergeDBName, "add-two")

	// Advance feat independently: _id:3 → hashC (diverges from hashA).
	_, err = env.client.Database(mergeDBName+"__feat").Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(3)},
		{Key: "v", Value: int32(3)},
	})
	require.NoError(t, err)
	hashC := docudoltCommit(t, env, mergeDBName+"__feat", "add-three-feat")

	// Merge feat into main → three-way merge commit hashM.
	var mergeRaw bson.M
	require.NoError(t, env.client.Database(mergeDBName+"__main").RunCommand(ctx, bson.D{
		{Key: "docudoltMerge", Value: int32(1)},
		{Key: "merge_in", Value: "feat"},
	}).Decode(&mergeRaw))
	hashM, ok := mergeRaw["commitId"].(string)
	require.True(t, ok, "merge commitId must be a string")
	require.NotEmpty(t, hashM, "three-way merge must produce a new commit hash")

	// -------------------------------------------------------------------------
	// Scenario 6: Merge commit appears in docudoltLog with parent1 and parent2
	// -------------------------------------------------------------------------
	t.Run("Scenario6_MergeCommitParents", func(t *testing.T) {
		var raw bson.M
		require.NoError(t, env.client.Database(mergeDBName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
		}).Decode(&raw))

		lr := decodeLogResult(t, raw)
		require.Len(t, lr.Commits, 1, "limit=1 must return exactly 1 commit")
		head := lr.Commits[0]
		assert.Equal(t, hashM, head.CommitID, "HEAD must be the merge commit")
		assert.Equal(t, hashB, head.Parent1, "merge commit parent1 must be main tip before merge (hashB)")
		assert.Equal(t, hashC, head.Parent2, "merge commit parent2 must be feature tip (hashC)")
		assert.Contains(t, head.Refs, "HEAD", "merge commit must carry 'HEAD' ref")
		assert.Contains(t, head.Refs, "main", "merge commit must carry 'main' ref")
	})

	// -------------------------------------------------------------------------
	// Scenario 7: docudoltLog from feature tip shows only feature branch history
	// -------------------------------------------------------------------------
	t.Run("Scenario7_FromFeatureTip", func(t *testing.T) {
		// Starting at hashC (feat tip) the walk follows parent1 only:
		// hashC → hashA → "Initialize database".
		// hashB (main-only) and hashM (merge) must not appear.
		var raw bson.M
		require.NoError(t, env.client.Database(mergeDBName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltLog", Value: int32(1)},
			{Key: "from", Value: hashC},
		}).Decode(&raw))

		lr := decodeLogResult(t, raw)
		require.Len(t, lr.Commits, 3, "traversal from hashC must return hashC, hashA, and Initialize")
		assert.Equal(t, hashC, lr.Commits[0].CommitID, "commits[0] must be hashC (feat tip)")
		assert.Equal(t, "add-three-feat", lr.Commits[0].Message)
		assert.Equal(t, hashA, lr.Commits[1].CommitID, "commits[1] must be hashA (common ancestor)")
		assert.Equal(t, "add-one", lr.Commits[1].Message)
		assert.Equal(t, "Initialize database", lr.Commits[2].Message, "commits[2] must be Initialize root")
		assert.Empty(t, lr.Commits[2].Parent1, "Initialize root has no parent1")

		for _, c := range lr.Commits {
			assert.NotEqual(t, hashB, c.CommitID, "hashB (main-only) must not appear when from=hashC")
			assert.NotEqual(t, hashM, c.CommitID, "hashM (merge commit) must not appear when from=hashC")
		}
	})

	// -------------------------------------------------------------------------
	// Scenario 8: limit works correctly on non-linear history
	// -------------------------------------------------------------------------
	t.Run("Scenario8_LimitOnNonLinearHistory", func(t *testing.T) {
		// limit=2 from main HEAD follows parent1: hashM → hashB.  hashA must not appear.
		var raw bson.M
		require.NoError(t, env.client.Database(mergeDBName+"__main").RunCommand(ctx, bson.D{
			{Key: "docudoltLog", Value: int32(1)},
			{Key: "limit", Value: int32(2)},
		}).Decode(&raw))

		lr := decodeLogResult(t, raw)
		require.Len(t, lr.Commits, 2, "limit=2 must return exactly 2 commits")
		assert.Equal(t, hashM, lr.Commits[0].CommitID, "commits[0] must be hashM (HEAD/merge)")
		assert.Equal(t, hashB, lr.Commits[1].CommitID, "commits[1] must be hashB (parent1 of merge)")

		for _, c := range lr.Commits {
			assert.NotEqual(t, hashA, c.CommitID, "hashA must not appear with limit=2")
			assert.NotEqual(t, hashC, c.CommitID, "hashC (feat-only) must not appear with limit=2")
		}
	})

	_ = hash3 // referenced in subtests above
}
