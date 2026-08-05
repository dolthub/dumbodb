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

// TestAutoCommit_CleanTreeAfterWrite: the working tree is clean after an auto-committed write.
func TestAutoCommit_CleanTreeAfterWrite(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	dbName := fmt.Sprintf("acvc_clean_%d", rand.Int64N(1_000_000))
	_, err := env.Client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)

	sr := runStatus(t, env, dbName)
	assert.Empty(t, sr.Tables, "working tree must be clean after an auto-committed write")
	assert.NotEmpty(t, sr.CommitID, "a clean tree reports a HEAD commit id")
}

// TestAutoCommit_ReadsDoNotCommit: read-only VC commands create no commit.
func TestAutoCommit_ReadsDoNotCommit(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	db := env.Client.Database(fmt.Sprintf("acvc_reads_%d", rand.Int64N(1_000_000)))
	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)

	before := acCommitCount(t, db)
	for _, cmd := range []bson.D{
		{{Key: "doltLog", Value: int32(1)}},
		{{Key: "doltStatus", Value: int32(1)}},
		{{Key: "doltDiff", Value: int32(1)}},
	} {
		require.NoError(t, db.RunCommand(ctx, cmd).Err(), "read command %v", cmd)
	}
	assert.Equal(t, before, acCommitCount(t, db), "read-only VC commands must not create commits")
}

// TestAutoCommit_ExplicitCommitNothing: explicit doltCommit on the clean tree reports nothing to commit.
func TestAutoCommit_ExplicitCommitNothing(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	dbName := fmt.Sprintf("acvc_ec_%d", rand.Int64N(1_000_000))
	_, err := env.Client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)

	var raw bson.M
	err = env.Client.Database(dbName).RunCommand(ctx, bson.D{{Key: "doltCommit", Value: int32(1)}, {Key: "message", Value: "manual"}}).Decode(&raw)
	require.Error(t, err, "explicit commit on a clean tree must fail with nothing to commit")
}

// TestAutoCommit_ResetNoSpuriousCommit: reset moves HEAD without creating a commit.
func TestAutoCommit_ResetNoSpuriousCommit(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	db := env.Client.Database(fmt.Sprintf("acvc_reset_%d", rand.Int64N(1_000_000)))
	coll := db.Collection("items")
	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
	require.NoError(t, err)

	before := acCommitCount(t, db)
	require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "doltReset", Value: int32(1)}, {Key: "to", Value: "HEAD~1"}, {Key: "hard", Value: true}}).Err())
	assert.Equal(t, before-1, acCommitCount(t, db), "reset must move HEAD back, not add a commit")
}

// TestAutoCommit_BranchTagNoDataCommit: branch/tag creation adds no data commit.
func TestAutoCommit_BranchTagNoDataCommit(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	dbBase := fmt.Sprintf("acvc_bt_%d", rand.Int64N(1_000_000))
	main := env.Client.Database(dbBase + "@main")
	_, err := main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)

	before := acCommitCount(t, main)
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "branch", Value: "feature"}}).Err())
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "doltTag", Value: int32(1)}, {Key: "name", Value: "v1"}}).Err())
	assert.Equal(t, before, acCommitCount(t, main), "branch/tag must not create data commits")
}

// TestAutoCommit_CleanMergeOneCommit: a non-conflicting merge succeeds under auto-commit.
func TestAutoCommit_CleanMergeOneCommit(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	dbBase := fmt.Sprintf("acvc_cm_%d", rand.Int64N(1_000_000))
	main := env.Client.Database(dbBase + "@main")
	feat := env.Client.Database(dbBase + "@feature")

	_, err := main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	require.NoError(t, err)
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "branch", Value: "feature"}}).Err())
	_, err = main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
	require.NoError(t, err)
	_, err = feat.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}})
	require.NoError(t, err)

	raw := runCommandRaw(t, main, bson.D{{Key: "doltMerge", Value: int32(1)}, {Key: "mergeIn", Value: "feature"}})
	require.EqualValues(t, 1, raw["ok"], "non-conflicting merge must succeed")
	assert.True(t, docExists(t, main.Collection("items"), 3), "merge must bring feature's _id:3 into main")
}

// TestAutoCommit_MergeStateSurvivesRestart: a paused conflict still suppresses auto-commit after restart.
func TestAutoCommit_MergeStateSurvivesRestart(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()
	dbBase := fmt.Sprintf("acvc_rs_%d", rand.Int64N(1_000_000))
	main := env.Client.Database(dbBase + "@main")
	feat := env.Client.Database(dbBase + "@feature")

	_, err := main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "base"}})
	require.NoError(t, err)
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "branch", Value: "feature"}}).Err())
	_, err = feat.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat"}}}})
	require.NoError(t, err)
	_, err = main.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "main"}}}})
	require.NoError(t, err)
	raw := runCommandRaw(t, main, bson.D{{Key: "doltMerge", Value: int32(1)}, {Key: "mergeIn", Value: "feature"}})
	require.EqualValues(t, 0, raw["ok"], "merge must conflict")

	env.Restart(t)
	main = env.Client.Database(dbBase + "@main")

	paused := acCommitCount(t, main)
	_, err = main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}})
	require.NoError(t, err)
	assert.Equal(t, paused, acCommitCount(t, main), "conflict must survive restart and still suppress auto-commit")

	var conflictsRaw bson.M
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "doltConflicts", Value: int32(1)}}).Decode(&conflictsRaw))
	conflicts, _ := conflictsRaw["conflicts"].(bson.A)
	assert.NotEmpty(t, conflicts, "conflict must still be reported after restart")
}
