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

func acCommitCount(t *testing.T, db *mongo.Database) int {
	t.Helper()
	var raw bson.M
	if err := db.RunCommand(context.Background(), bson.D{{Key: "doltLog", Value: int32(1)}}).Decode(&raw); err != nil {
		return 0
	}
	return len(decodeLogResult(t, raw).Commits)
}

// TestAutoCommit_ConflictWindow_Merge: conflict-window contract for a merge.
func TestAutoCommit_ConflictWindow_Merge(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	dbName := fmt.Sprintf("acmc_%d", rand.Int64N(1_000_000))

	main := env.Client.Database(dbName + "@main")
	feat := env.Client.Database(dbName + "@feature")

	_, err := main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "base"}})
	require.NoError(t, err)
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "branch", Value: "feature"}}).Err())

	_, err = feat.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat"}}}})
	require.NoError(t, err)
	_, err = main.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "main"}}}})
	require.NoError(t, err)

	raw := runCommandRaw(t, main, bson.D{{Key: "doltMerge", Value: int32(1)}, {Key: "mergeIn", Value: "feature"}})
	require.EqualValues(t, 0, raw["ok"], "merge must conflict on _id:1")

	paused := acCommitCount(t, main)
	_, err = main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}, {Key: "v", Value: "during"}})
	require.NoError(t, err)
	_, err = main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(98)}, {Key: "v", Value: "during2"}})
	require.NoError(t, err)
	assert.Equal(t, paused, acCommitCount(t, main), "writes during a conflict must not auto-commit")

	resolveAllConflicts(t, main, "ours")
	mergeContinue(t, main)
	assert.True(t, docExists(t, main.Collection("items"), 99), "edit during conflict must survive --continue")
	assert.True(t, docExists(t, main.Collection("items"), 98), "second edit during conflict must survive --continue")

	resumed := acCommitCount(t, main)
	_, err = main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "after"}})
	require.NoError(t, err)
	assert.Equal(t, resumed+1, acCommitCount(t, main), "auto-commit must resume after --continue")
}

func acHeadHash(t *testing.T, db *mongo.Database) string {
	t.Helper()
	var raw bson.M
	require.NoError(t, db.RunCommand(context.Background(), bson.D{{Key: "doltLog", Value: int32(1)}, {Key: "limit", Value: int32(1)}}).Decode(&raw))
	lr := decodeLogResult(t, raw)
	require.NotEmpty(t, lr.Commits)
	return lr.Commits[0].CommitID
}

// assertConflictWindow: no auto-commit while paused; continue captures the edit and resumes.
func assertConflictWindow(t *testing.T, opDB *mongo.Database, continueCmd bson.D) {
	t.Helper()
	ctx := context.Background()

	paused := acCommitCount(t, opDB)
	_, err := opDB.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}, {Key: "v", Value: "during"}})
	require.NoError(t, err)
	assert.Equal(t, paused, acCommitCount(t, opDB), "writes during a conflict must not auto-commit")

	resolveAllConflicts(t, opDB, "ours")
	raw := runCommandRaw(t, opDB, continueCmd)
	require.EqualValues(t, 1, raw["ok"], "continue must succeed")

	assert.True(t, docExists(t, opDB.Collection("items"), 99), "edit during conflict must survive continue")

	resumed := acCommitCount(t, opDB)
	_, err = opDB.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "after"}})
	require.NoError(t, err)
	assert.Equal(t, resumed+1, acCommitCount(t, opDB), "auto-commit must resume after continue")
}

// TestAutoCommit_ConflictWindow_CherryPick: conflict-window contract for cherry-pick.
func TestAutoCommit_ConflictWindow_CherryPick(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	dbName := fmt.Sprintf("accp_%d", rand.Int64N(1_000_000))
	main := env.Client.Database(dbName + "@main")
	feat := env.Client.Database(dbName + "@feature")

	_, err := main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "base"}})
	require.NoError(t, err)
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "branch", Value: "feature"}}).Err())
	_, err = feat.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat"}}}})
	require.NoError(t, err)
	pickHash := acHeadHash(t, feat)
	_, err = main.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "main"}}}})
	require.NoError(t, err)

	raw := runCommandRaw(t, main, bson.D{{Key: "dumboCherryPick", Value: int32(1)}, {Key: "commit", Value: pickHash}})
	require.EqualValues(t, 0, raw["ok"], "cherry-pick must conflict")

	assertConflictWindow(t, main, bson.D{{Key: "dumboCherryPick", Value: int32(1)}, {Key: "continue", Value: int32(1)}})
}

// TestAutoCommit_ConflictWindow_Revert: conflict-window contract for revert.
func TestAutoCommit_ConflictWindow_Revert(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	dbName := fmt.Sprintf("acrv_%d", rand.Int64N(1_000_000))
	main := env.Client.Database(dbName + "@main")

	_, err := main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "v1"}})
	require.NoError(t, err)
	_, err = main.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "v2"}}}})
	require.NoError(t, err)
	revertHash := acHeadHash(t, main) // the commit that set v2
	_, err = main.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "v3"}}}})
	require.NoError(t, err)

	raw := runCommandRaw(t, main, bson.D{{Key: "dumboRevert", Value: int32(1)}, {Key: "commit", Value: revertHash}})
	require.EqualValues(t, 0, raw["ok"], "revert must conflict")

	assertConflictWindow(t, main, bson.D{{Key: "dumboRevert", Value: int32(1)}, {Key: "continue", Value: int32(1)}})
}

// TestAutoCommit_ConflictWindow_Rebase: conflict-window contract for rebase.
func TestAutoCommit_ConflictWindow_Rebase(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	dbName := fmt.Sprintf("acrb_%d", rand.Int64N(1_000_000))
	main := env.Client.Database(dbName + "@main")
	feat := env.Client.Database(dbName + "@feature")

	_, err := main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "base"}})
	require.NoError(t, err)
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "branch", Value: "feature"}}).Err())
	_, err = main.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "main"}}}})
	require.NoError(t, err)
	_, err = feat.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat"}}}})
	require.NoError(t, err)

	raw := runCommandRaw(t, feat, bson.D{{Key: "dumboRebase", Value: int32(1)}, {Key: "onto", Value: "main"}})
	require.EqualValues(t, 0, raw["ok"], "rebase must conflict")

	assertConflictWindow(t, feat, bson.D{{Key: "dumboRebase", Value: int32(1)}, {Key: "onto", Value: "main"}, {Key: "continue", Value: int32(1)}})
}
