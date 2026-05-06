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

// Tests for dumboStatus and dumboDiff on non-main branches.
// These verify that status/diff correctly compare the branch's HEAD against
// the branch's working set, not the main branch.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestBranchStatus_DirtyAfterUpdate verifies that dumboStatus on a non-main
// branch correctly reports dirty:true after an update on that branch.
func TestBranchStatus_DirtyAfterUpdate(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("brstatus%d", rand.Int64N(1_000_000))

	// Baseline: one doc committed on main.
	db := env.client.Database(dbName)
	require.NoError(t, db.Drop(ctx))
	_, err := db.Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)}, {Key: "v", Value: "orig"},
	})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "baseline", "alice")

	// Create feature branch.
	mainDB := env.client.Database(dbName + "@main")
	var brRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "feature"},
	}).Decode(&brRaw))

	// Status on feature before any changes: clean.
	srClean := runStatus(t, env, dbName+"@feature")
	assert.Empty(t, srClean.Tables, "feature must be clean before changes")
	assert.NotEmpty(t, srClean.CommitID, "commitId must be present on clean branch")

	// Update a doc on the feature branch.
	featDB := env.client.Database(dbName + "@feature")
	_, err = featDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "updated"}}}})
	require.NoError(t, err)

	// Status on feature: dirty.
	srDirty := runStatus(t, env, dbName+"@feature")
	assert.NotEmpty(t, srDirty.Tables, "feature must show changes after update")
	assert.Empty(t, srDirty.CommitID, "commitId must be absent when dirty")

	entry := findTableStatus(srDirty, "items")
	require.NotNil(t, entry, "items collection must appear in status")
	assert.Equal(t, "modified", entry.Status)
	assert.Equal(t, 1, entry.Modified, "one document modified")

	// Status on main must still be clean (update was on feature only).
	srMain := runStatus(t, env, dbName)
	assert.Empty(t, srMain.Tables, "main must be clean -- update was on feature")
	assert.NotEmpty(t, srMain.CommitID, "main commitId must be present")
}

// TestBranchStatus_CleanAfterCommit verifies that committing on a non-main
// branch clears the dirty state.
func TestBranchStatus_CleanAfterCommit(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("brcommit%d", rand.Int64N(1_000_000))

	db := env.client.Database(dbName)
	require.NoError(t, db.Drop(ctx))
	_, err := db.Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)}, {Key: "v", Value: "orig"},
	})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "baseline", "alice")

	mainDB := env.client.Database(dbName + "@main")
	var brRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "dev"},
	}).Decode(&brRaw))

	// Update on dev branch.
	devDB := env.client.Database(dbName + "@dev")
	_, err = devDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "devval"}}}})
	require.NoError(t, err)

	// Dirty before commit.
	srDirty := runStatus(t, env, dbName+"@dev")
	assert.NotEmpty(t, srDirty.Tables, "dev must be dirty before commit")

	// Commit on dev.
	dumboDBCommit(t, env, dbName+"@dev", "dev commit", "bob")

	// Clean after commit.
	srClean := runStatus(t, env, dbName+"@dev")
	assert.Empty(t, srClean.Tables, "dev must be clean after commit")
	assert.NotEmpty(t, srClean.CommitID, "commitId must be present after commit")
}

// TestBranchDiff_ShowsChanges verifies that dumboDiff on a non-main branch
// correctly shows uncommitted changes on that branch.
func TestBranchDiff_ShowsChanges(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("brdiff%d", rand.Int64N(1_000_000))

	db := env.client.Database(dbName)
	require.NoError(t, db.Drop(ctx))
	_, err := db.Collection("items").InsertMany(ctx, []interface{}{
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "a"}},
		bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "b"}},
	})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "baseline", "alice")

	mainDB := env.client.Database(dbName + "@main")
	var brRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "work"},
	}).Decode(&brRaw))

	// Modify _id:1 and delete _id:2 on work branch.
	workDB := env.client.Database(dbName + "@work")
	_, err = workDB.Collection("items").UpdateOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "changed"}}}})
	require.NoError(t, err)
	_, err = workDB.Collection("items").DeleteOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
	require.NoError(t, err)

	// Diff on work branch must show changes.
	var diffRaw bson.M
	require.NoError(t, workDB.RunCommand(ctx, bson.D{
		{Key: "doltDiff", Value: int32(1)},
	}).Decode(&diffRaw))

	colls, _ := diffRaw["collections"].(bson.A)
	require.Len(t, colls, 1, "one collection with changes")
	coll := colls[0].(bson.M)
	assert.Equal(t, "items", coll["name"])

	modified, _ := coll["modified"].(bson.A)
	removed, _ := coll["removed"].(bson.A)
	assert.Len(t, modified, 1, "one modified document")
	assert.Len(t, removed, 1, "one removed document")

	// Diff on main must be empty (no changes on main).
	var mainDiffRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltDiff", Value: int32(1)},
	}).Decode(&mainDiffRaw))

	mainColls, _ := mainDiffRaw["collections"].(bson.A)
	assert.Len(t, mainColls, 0, "main must have no diff -- changes are on work branch")
}

// TestBranchDiff_EmptyAfterCommit verifies that committing on a non-main
// branch clears the diff.
func TestBranchDiff_EmptyAfterCommit(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("brdiffcmt%d", rand.Int64N(1_000_000))

	db := env.client.Database(dbName)
	require.NoError(t, db.Drop(ctx))
	_, err := db.Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)}, {Key: "v", Value: "orig"},
	})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "baseline", "alice")

	mainDB := env.client.Database(dbName + "@main")
	var brRaw bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: "br"},
	}).Decode(&brRaw))

	// Insert on br.
	brDB := env.client.Database(dbName + "@br")
	_, err = brDB.Collection("items").InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)}, {Key: "v", Value: "new"},
	})
	require.NoError(t, err)

	// Diff before commit: non-empty.
	var diffBefore bson.M
	require.NoError(t, brDB.RunCommand(ctx, bson.D{
		{Key: "doltDiff", Value: int32(1)},
	}).Decode(&diffBefore))
	require.Len(t, diffBefore["collections"].(bson.A), 1, "diff must show changes before commit")

	// Commit.
	dumboDBCommit(t, env, dbName+"@br", "br commit", "bob")

	// Diff after commit: empty.
	var diffAfter bson.M
	require.NoError(t, brDB.RunCommand(ctx, bson.D{
		{Key: "doltDiff", Value: int32(1)},
	}).Decode(&diffAfter))
	assert.Len(t, diffAfter["collections"].(bson.A), 0, "diff must be empty after commit")
}
