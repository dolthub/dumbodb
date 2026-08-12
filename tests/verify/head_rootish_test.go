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

package verify

// TestHEADRootish* are the automated analog of Scenario 10 in
// docs/verify/rootish.md.
//
// HEAD-anchored refspecs (HEAD, HEAD~N, HEAD^N) resolve against the branch
// encoded in $db, so every command that takes a commit-ish accepts them. These
// tests cover the commands that resolve one: dumboRevert, dumboCherryPick,
// dumboRebase, dumboTag and dumboMerge. dumboReset and dumboBranchStatus have
// their own coverage in reset_test.go and branch_status_test.go.
//
// dumboMerge additionally accepts non-branch commit-ish sources (hashes, tags,
// traversal expressions), which is exercised here too.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// headTestDB creates a fresh database whose main branch holds one commit per
// entry in inserts: commit i inserts {_id: i+1}. It returns the commit hashes
// oldest-first.
func headTestDB(t *testing.T, env *dumboDBTestEnv, dbName string, commits int) []string {
	t.Helper()

	ctx := context.Background()
	db := env.Client.Database(dbName)
	require.NoError(t, db.Drop(ctx))

	hashes := make([]string, 0, commits)
	for i := 1; i <= commits; i++ {
		_, err := db.Collection("records").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(i)}})
		require.NoError(t, err)
		hashes = append(hashes, dumboDBCommit(t, env, dbName, fmt.Sprintf("c%d", i), "alice <alice@acme.com>"))
	}
	return hashes
}

// recordIDs returns the _id values in the records collection, sorted ascending
// so assertions do not depend on storage order.
func recordIDs(t *testing.T, env *dumboDBTestEnv, dbName string) []int {
	t.Helper()

	ctx := context.Background()
	cur, err := env.Client.Database(dbName).Collection("records").Find(ctx, bson.D{})
	require.NoError(t, err)

	var docs []bson.M
	require.NoError(t, cur.All(ctx, &docs))

	ids := make([]int, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, toInt(d["_id"]))
	}
	sort.Ints(ids)
	return ids
}

func TestHEADRootishRevert(t *testing.T) {
	env := startDumboDB(t)

	dbName := fmt.Sprintf("headrevert%d", rand.Int64N(1_000_000))
	hashes := headTestDB(t, env, dbName, 3)
	mainDB := env.Client.Database(dbName + "@main")

	// HEAD is the tip commit (c3, which inserted _id:3).
	t.Run("Revert_HEAD", func(t *testing.T) {
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboRevert", Value: int32(1)},
			{Key: "commit", Value: "HEAD"},
		})

		require.EqualValues(t, 1, raw["ok"], "dumboRevert HEAD must succeed: %v", raw["errmsg"])

		msg, ok := raw["message"].(string)
		require.True(t, ok, "message must be a string")
		assert.Contains(t, msg, hashes[2], "revert must annotate the tip commit hash")

		assert.Equal(t, []int{1, 2}, recordIDs(t, env, dbName+"@main"),
			"reverting HEAD must undo the tip commit's insert")
	})

	// After the revert the tip is the revert commit, so HEAD~1 is c3 and
	// HEAD~2 is c2 (the commit that inserted _id:2).
	t.Run("Revert_HEADTilde", func(t *testing.T) {
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboRevert", Value: int32(1)},
			{Key: "commit", Value: "HEAD~2"},
		})

		require.EqualValues(t, 1, raw["ok"], "dumboRevert HEAD~2 must succeed: %v", raw["errmsg"])

		msg, ok := raw["message"].(string)
		require.True(t, ok, "message must be a string")
		assert.Contains(t, msg, hashes[1], "revert must annotate the commit HEAD~2 resolved to")

		assert.Equal(t, []int{1}, recordIDs(t, env, dbName+"@main"),
			"reverting HEAD~2 must undo the _id:2 insert")
	})

	// A caret chain resolves the same way: HEAD^ is the first parent.
	t.Run("Revert_HEADCaret", func(t *testing.T) {
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboRevert", Value: int32(1)},
			{Key: "commit", Value: "HEAD^"},
		})

		require.EqualValues(t, 1, raw["ok"], "dumboRevert HEAD^ must succeed: %v", raw["errmsg"])
		assert.Equal(t, []int{1, 3}, recordIDs(t, env, dbName+"@main"),
			"reverting HEAD^ must undo the previous revert, restoring _id:3")
	})
}

func TestHEADRootishCherryPick(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("headpick%d", rand.Int64N(1_000_000))
	hashes := headTestDB(t, env, dbName, 2)
	mainDB := env.Client.Database(dbName + "@main")

	// c3 removes the document c2 added, so HEAD~1 (c2) can be replayed cleanly.
	_, err := mainDB.Collection("records").DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "c3", "alice <alice@acme.com>")

	require.Equal(t, []int{1}, recordIDs(t, env, dbName+"@main"), "setup: _id:2 must be gone")

	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "dumboCherryPick", Value: int32(1)},
		{Key: "commit", Value: "HEAD~1"},
	})

	require.EqualValues(t, 1, raw["ok"], "dumboCherryPick HEAD~1 must succeed: %v", raw["errmsg"])

	msg, ok := raw["message"].(string)
	require.True(t, ok, "message must be a string")
	assert.Contains(t, msg, hashes[1], "cherry-pick must annotate the commit HEAD~1 resolved to")

	assert.Equal(t, []int{1, 2}, recordIDs(t, env, dbName+"@main"),
		"cherry-picking HEAD~1 must re-apply the _id:2 insert")
}

func TestHEADRootishRebase(t *testing.T) {
	env := startDumboDB(t)

	dbName := fmt.Sprintf("headrebase%d", rand.Int64N(1_000_000))
	hashes := headTestDB(t, env, dbName, 2)

	// HEAD~1 is an ancestor of the branch tip, so the branch already sits on
	// top of onto: nothing is replayed. The point is that the refspec resolves
	// at all -- it used to fail with "not found as commit hash, branch, or tag".
	raw := runCommandRaw(t, env.Client.Database(dbName+"@main"), bson.D{
		{Key: "dumboRebase", Value: int32(1)},
		{Key: "onto", Value: "HEAD~1"},
	})

	require.EqualValues(t, 1, raw["ok"], "dumboRebase onto HEAD~1 must succeed: %v", raw["errmsg"])
	assert.EqualValues(t, 0, toInt(raw["commitsReplayed"]), "onto is an ancestor, so nothing replays")
	assert.Equal(t, hashes[1], raw["newTip"], "branch tip must be untouched")
}

func TestHEADRootishTag(t *testing.T) {
	env := startDumboDB(t)

	dbName := fmt.Sprintf("headtag%d", rand.Int64N(1_000_000))
	hashes := headTestDB(t, env, dbName, 2)

	mainDB := env.Client.Database(dbName + "@main")

	raw := runCommandRaw(t, mainDB, bson.D{
		{Key: "dumboTag", Value: int32(1)},
		{Key: "name", Value: "at-head"},
		{Key: "hash", Value: "HEAD"},
	})
	require.EqualValues(t, 1, raw["ok"], "dumboTag at HEAD must succeed: %v", raw["errmsg"])
	assert.Equal(t, hashes[1], raw["commitId"], "tag must point at the branch tip")

	raw = runCommandRaw(t, mainDB, bson.D{
		{Key: "dumboTag", Value: int32(1)},
		{Key: "name", Value: "at-head-1"},
		{Key: "hash", Value: "HEAD~1"},
	})
	require.EqualValues(t, 1, raw["ok"], "dumboTag at HEAD~1 must succeed: %v", raw["errmsg"])
	assert.Equal(t, hashes[0], raw["commitId"], "tag must point at the tip's parent")
}

func TestHEADRootishMerge(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("headmerge%d", rand.Int64N(1_000_000))
	hashes := headTestDB(t, env, dbName, 3)

	mainDB := env.Client.Database(dbName + "@main")

	// Merging the connection's own tip is a self-merge: already up-to-date.
	t.Run("MergeIn_HEAD", func(t *testing.T) {
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboMerge", Value: int32(1)},
			{Key: "mergeIn", Value: "HEAD"},
		})

		require.EqualValues(t, 1, raw["ok"], "dumboMerge HEAD must succeed: %v", raw["errmsg"])
		assert.Equal(t, "already up-to-date", raw["message"], "merging HEAD into itself changes nothing")
		assert.Equal(t, hashes[2], raw["commitId"], "branch tip must be untouched")
	})

	// An ancestor is already merged; the refspec still has to resolve.
	t.Run("MergeIn_HEADTilde", func(t *testing.T) {
		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboMerge", Value: int32(1)},
			{Key: "mergeIn", Value: "HEAD~1"},
		})

		require.EqualValues(t, 1, raw["ok"], "dumboMerge HEAD~1 must succeed: %v", raw["errmsg"])
		assert.Equal(t, "already up-to-date", raw["message"], "an ancestor is already merged")
	})

	// A bare commit hash is a valid merge source: feature diverged from c1, so
	// merging c2 (which main reached long ago) is a real three-way merge.
	t.Run("MergeIn_CommitHash", func(t *testing.T) {
		bsBranchCreate(t, env, dbName, hashes[0], "feature")

		featureDB := env.Client.Database(dbName + "@feature")
		_, err := featureDB.Collection("records").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature work", "bob <bob@widgets.io>")

		raw := runCommandRaw(t, featureDB, bson.D{
			{Key: "dumboMerge", Value: int32(1)},
			{Key: "mergeIn", Value: hashes[1]},
		})

		require.EqualValues(t, 1, raw["ok"], "dumboMerge of a bare commit hash must succeed: %v", raw["errmsg"])

		msg, ok := raw["message"].(string)
		require.True(t, ok, "message must be a string")
		assert.Contains(t, msg, "Merge commit '"+hashes[1]+"'",
			"a non-branch source must be described as a commit, not a branch")

		assert.Equal(t, []int{1, 2, 99}, recordIDs(t, env, dbName+"@feature"),
			"feature must gain the document from the merged commit")
	})

	// A traversal expression names the same commit as a hash and merges the same
	// way; only the message differs, since it echoes the refspec as written.
	t.Run("MergeIn_TraversalExpression", func(t *testing.T) {
		bsBranchCreate(t, env, dbName, hashes[0], "feature2")

		feature2DB := env.Client.Database(dbName + "@feature2")
		_, err := feature2DB.Collection("records").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(98)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature2", "feature2 work", "bob <bob@widgets.io>")

		raw := runCommandRaw(t, feature2DB, bson.D{
			{Key: "dumboMerge", Value: int32(1)},
			{Key: "mergeIn", Value: "main~1"},
		})

		require.EqualValues(t, 1, raw["ok"], "dumboMerge of a traversal expression must succeed: %v", raw["errmsg"])

		msg, ok := raw["message"].(string)
		require.True(t, ok, "message must be a string")
		assert.Contains(t, msg, "Merge commit 'main~1'", "a traversal expression is not a branch")

		assert.Equal(t, []int{1, 2, 98}, recordIDs(t, env, dbName+"@feature2"),
			"feature2 must gain the document from main~1")
	})

	// A tag resolves as a merge source too, and reads as a tag in the message.
	t.Run("MergeIn_Tag", func(t *testing.T) {
		bsTag(t, env, dbName+"@main", "v-tip", hashes[2])

		raw := runCommandRaw(t, env.Client.Database(dbName+"@feature"), bson.D{
			{Key: "dumboMerge", Value: int32(1)},
			{Key: "mergeIn", Value: "v-tip"},
		})

		require.EqualValues(t, 1, raw["ok"], "dumboMerge of a tag must succeed: %v", raw["errmsg"])

		msg, ok := raw["message"].(string)
		require.True(t, ok, "message must be a string")
		assert.Contains(t, msg, "Merge tag 'v-tip'", "a tag source must be described as a tag")

		assert.Equal(t, []int{1, 2, 3, 99}, recordIDs(t, env, dbName+"@feature"),
			"feature must gain the document from the tagged commit")
	})

	// Merging partway into another branch's history: side~2 is two commits below
	// that branch's tip, so main must pick up only the work up to that point.
	// This is the case that distinguishes resolving the expression from resolving
	// the branch it is anchored on.
	t.Run("MergeIn_SideBranchAncestor", func(t *testing.T) {
		bsBranchCreate(t, env, dbName, hashes[0], "side")

		sideDB := env.Client.Database(dbName + "@side")
		for _, id := range []int32{101, 102, 103} {
			_, err := sideDB.Collection("records").InsertOne(ctx, bson.D{{Key: "_id", Value: id}})
			require.NoError(t, err)
			dumboDBCommit(t, env, dbName+"@side", fmt.Sprintf("side %d", id), "bob <bob@widgets.io>")
		}

		raw := runCommandRaw(t, mainDB, bson.D{
			{Key: "dumboMerge", Value: int32(1)},
			{Key: "mergeIn", Value: "side~2"},
		})

		require.EqualValues(t, 1, raw["ok"], "dumboMerge of side~2 must succeed: %v", raw["errmsg"])

		msg, ok := raw["message"].(string)
		require.True(t, ok, "message must be a string")
		assert.Contains(t, msg, "Merge commit 'side~2'", "the message echoes the refspec as written")

		assert.Equal(t, []int{1, 2, 3, 101}, recordIDs(t, env, dbName+"@main"),
			"main must gain only the work committed at side~2, not side's later commits")

		assert.Equal(t, []int{1, 101, 102, 103}, recordIDs(t, env, dbName+"@side"),
			"the source branch must be untouched")
	})
}
