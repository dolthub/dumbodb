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
	require.NoError(t, db.RunCommand(context.Background(), bson.D{{Key: "doltLog", Value: int32(1)}}).Decode(&raw))
	return len(decodeLogResult(t, raw).Commits)
}

// TestAutoCommit_ConflictWindow_Merge verifies the conflict-window contract
// under --auto-commit for a merge: while the merge is paused on conflict,
// edits do not auto-commit; --continue produces one commit that includes the
// edits made during the window; auto-commit resumes afterward.
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

	raw := runCommandRaw(t, main, bson.D{{Key: "doltMerge", Value: int32(1)}, {Key: "merge_in", Value: "feature"}})
	require.EqualValues(t, 0, raw["ok"], "merge must conflict on _id:1")

	// Auto-commit is disabled while paused: edits to the conflicting doc and to
	// an unrelated doc do not create commits.
	paused := acCommitCount(t, main)
	_, err = main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}, {Key: "v", Value: "during"}})
	require.NoError(t, err)
	_, err = main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(98)}, {Key: "v", Value: "during2"}})
	require.NoError(t, err)
	assert.Equal(t, paused, acCommitCount(t, main), "writes during a conflict must not auto-commit")

	// Resolve and continue: the edits made during the window are in the commit.
	resolveAllConflicts(t, main, "ours")
	mergeContinue(t, main)
	assert.True(t, docExists(t, main.Collection("items"), 99), "edit during conflict must survive --continue")
	assert.True(t, docExists(t, main.Collection("items"), 98), "second edit during conflict must survive --continue")

	// Auto-commit resumes after --continue: a plain insert on main advances
	// history by exactly one commit.
	resumed := acCommitCount(t, main)
	_, err = main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "after"}})
	require.NoError(t, err)
	assert.Equal(t, resumed+1, acCommitCount(t, main), "auto-commit must resume after --continue")
}
