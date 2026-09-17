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
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"}}).Err())

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

// conflictOp is one of the four operations that can pause on a conflict. Each
// diverges _id:1 of "items" and leaves its operation paused on that document,
// so one resolution table can be run against all of them.
//
// oursV and theirsV are the values the two sides carry. For a rebase ours is
// the commit being replayed and theirs is the branch it lands on: a caller's own
// work stays on the "ours" side for every operation here.
type conflictOp struct {
	name        string
	oursV       string
	theirsV     string
	continueCmd bson.D
	// pause diverges the branches and starts the operation, returning the
	// database handle the operation is paused on.
	pause func(t *testing.T, env *dumboDBTestEnv, dbName string) *mongo.Database
}

func acSetV(t *testing.T, db *mongo.Database, v string) {
	t.Helper()
	_, err := db.Collection("items").UpdateOne(context.Background(),
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: v}}}})
	require.NoError(t, err)
}

func acSeedBranches(t *testing.T, env *dumboDBTestEnv, dbName string) (main, feat *mongo.Database) {
	t.Helper()
	main = env.Client.Database(dbName + "@main")
	_, err := main.Collection("items").InsertOne(context.Background(),
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "base"}})
	require.NoError(t, err)
	require.NoError(t, main.RunCommand(context.Background(), bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "action", Value: "add"},
		{Key: "branch", Value: "feature"},
	}).Err())
	return main, env.Client.Database(dbName + "@feature")
}

func conflictOps() []conflictOp {
	return []conflictOp{
		{
			name: "merge", oursV: "main", theirsV: "feat",
			continueCmd: bson.D{{Key: "doltMerge", Value: int32(1)}, {Key: "continue", Value: int32(1)}, {Key: "author", Value: "tester"}},
			pause: func(t *testing.T, env *dumboDBTestEnv, dbName string) *mongo.Database {
				main, feat := acSeedBranches(t, env, dbName)
				acSetV(t, feat, "feat")
				acSetV(t, main, "main")
				raw := runCommandRaw(t, main, bson.D{{Key: "doltMerge", Value: int32(1)}, {Key: "mergeIn", Value: "feature"}})
				require.EqualValues(t, 0, raw["ok"], "merge must conflict on _id:1")
				return main
			},
		},
		{
			name: "cherry-pick", oursV: "main", theirsV: "feat",
			continueCmd: bson.D{{Key: "dumboCherryPick", Value: int32(1)}, {Key: "continue", Value: int32(1)}},
			pause: func(t *testing.T, env *dumboDBTestEnv, dbName string) *mongo.Database {
				main, feat := acSeedBranches(t, env, dbName)
				acSetV(t, feat, "feat")
				pickHash := acHeadHash(t, feat)
				acSetV(t, main, "main")
				raw := runCommandRaw(t, main, bson.D{{Key: "dumboCherryPick", Value: int32(1)}, {Key: "commit", Value: pickHash}})
				require.EqualValues(t, 0, raw["ok"], "cherry-pick must conflict on _id:1")
				return main
			},
		},
		{
			// Reverting the commit that set v2 restores v1, so "theirs" is the
			// state being restored and "ours" is the later v3.
			name: "revert", oursV: "v3", theirsV: "v1",
			continueCmd: bson.D{{Key: "dumboRevert", Value: int32(1)}, {Key: "continue", Value: int32(1)}},
			pause: func(t *testing.T, env *dumboDBTestEnv, dbName string) *mongo.Database {
				main := env.Client.Database(dbName + "@main")
				_, err := main.Collection("items").InsertOne(context.Background(),
					bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "v1"}})
				require.NoError(t, err)
				acSetV(t, main, "v2")
				revertHash := acHeadHash(t, main)
				acSetV(t, main, "v3")
				raw := runCommandRaw(t, main, bson.D{{Key: "dumboRevert", Value: int32(1)}, {Key: "commit", Value: revertHash}})
				require.EqualValues(t, 0, raw["ok"], "revert must conflict on _id:1")
				return main
			},
		},
		{
			// The feature commit is replayed onto main, so the feature edit is
			// ours and main's is theirs.
			name: "rebase", oursV: "feat", theirsV: "main",
			continueCmd: bson.D{{Key: "dumboRebase", Value: int32(1)}, {Key: "onto", Value: "main"}, {Key: "continue", Value: int32(1)}},
			pause: func(t *testing.T, env *dumboDBTestEnv, dbName string) *mongo.Database {
				main, feat := acSeedBranches(t, env, dbName)
				acSetV(t, main, "main")
				acSetV(t, feat, "feat")
				raw := runCommandRaw(t, feat, bson.D{{Key: "dumboRebase", Value: int32(1)}, {Key: "onto", Value: "main"}})
				require.EqualValues(t, 0, raw["ok"], "rebase must conflict on _id:1")
				return feat
			},
		},
	}
}

// Every resolution is written through the branch working set, so each must
// merge its edit into the branch's live root rather than stamp the root staged
// when the operation paused back over it. Run against all four operations
// because each one has its own continue path publishing that root.
//
// "ours" is covered per-operation by the contract tests below; these are the
// resolutions that rewrite the conflicted document.
func TestAutoCommit_ConflictWindow_RewritingResolutions(t *testing.T) {
	resolutions := []string{"theirs", "custom"}

	for _, op := range conflictOps() {
		t.Run(op.name, func(t *testing.T) {
			for _, resolution := range resolutions {
				t.Run(resolution, func(t *testing.T) {
					env := startDumboDB(t, "--auto-commit")
					ctx := context.Background()
					dbName := fmt.Sprintf("cw%s_%d", resolution, rand.Int64N(1_000_000))
					opDB := op.pause(t, env, dbName)

					_, err := opDB.Collection("items").InsertOne(ctx,
						bson.D{{Key: "_id", Value: int32(99)}, {Key: "v", Value: "during"}})
					require.NoError(t, err)

					wantV := op.theirsV
					cmd := bson.D{
						{Key: "doltResolveConflict", Value: int32(1)},
						{Key: "collection", Value: "items"},
						{Key: "conflictId", Value: soleConflictID(t, opDB)},
						{Key: "resolution", Value: resolution},
					}
					if resolution == "custom" {
						wantV = "resolved"
						cmd = append(cmd, bson.E{Key: "value", Value: bson.D{
							{Key: "_id", Value: int32(1)},
							{Key: "v", Value: wantV},
						}})
					}
					var resolveRaw bson.M
					require.NoError(t, opDB.RunCommand(ctx, cmd).Decode(&resolveRaw))
					require.EqualValues(t, 1, resolveRaw["ok"])

					assert.True(t, docExists(t, opDB.Collection("items"), 99),
						"edit during conflict must survive the resolution itself")

					contRaw := runCommandRaw(t, opDB, op.continueCmd)
					require.EqualValues(t, 1, contRaw["ok"], "continue must succeed")

					assert.True(t, docExists(t, opDB.Collection("items"), 99),
						"edit during conflict must survive continue")

					var doc bson.M
					require.NoError(t, opDB.Collection("items").FindOne(ctx,
						bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
					assert.Equal(t, wantV, doc["v"], "the resolution must still have been applied")
				})
			}
		})
	}
}

// A view or collection-metadata conflict is resolved through the same working
// set as a document conflict, so those two paths must leave a write made during
// the conflict window alone as well.
func TestAutoCommit_ConflictWindow_NamespaceResolutions(t *testing.T) {
	cases := []struct {
		name string
		// diverge makes main and feature disagree about something that
		// conflicts at the namespace level rather than the document level.
		diverge func(t *testing.T, main, feat *mongo.Database)
		// entry is the name doltConflicts reports the conflict against.
		entry string
	}{
		{
			name:  "view",
			entry: "v1",
			diverge: func(t *testing.T, main, feat *mongo.Database) {
				createView := func(db *mongo.Database, match string) {
					require.NoError(t, db.RunCommand(context.Background(), bson.D{
						{Key: "create", Value: "v1"},
						{Key: "viewOn", Value: "items"},
						{Key: "pipeline", Value: bson.A{
							bson.D{{Key: "$match", Value: bson.D{{Key: "v", Value: match}}}},
						}},
					}).Err())
				}
				createView(main, "main")
				createView(feat, "feat")
			},
		},
		{
			name:  "metadata",
			entry: "orders",
			diverge: func(t *testing.T, main, feat *mongo.Database) {
				ctx := context.Background()
				require.NoError(t, main.RunCommand(ctx, bson.D{
					{Key: "collMod", Value: "orders"},
					{Key: "validationLevel", Value: "strict"},
				}).Err())
				require.NoError(t, feat.RunCommand(ctx, bson.D{
					{Key: "collMod", Value: "orders"},
					{Key: "validationLevel", Value: "moderate"},
				}).Err())
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := startDumboDB(t, "--auto-commit")
			ctx := context.Background()
			dbName := fmt.Sprintf("acmn%s_%d", tc.name, rand.Int64N(1_000_000))

			main := env.Client.Database(dbName + "@main")
			_, err := main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "base"}})
			require.NoError(t, err)
			require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "create", Value: "orders"}}).Err())
			require.NoError(t, main.RunCommand(ctx, bson.D{
				{Key: "doltBranch", Value: int32(1)},
				{Key: "action", Value: "add"},
				{Key: "branch", Value: "feature"},
			}).Err())

			tc.diverge(t, main, env.Client.Database(dbName+"@feature"))

			raw := runCommandRaw(t, main, bson.D{{Key: "doltMerge", Value: int32(1)}, {Key: "mergeIn", Value: "feature"}})
			require.EqualValues(t, 0, raw["ok"], "merge must conflict on %s", tc.entry)

			_, err = main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(99)}, {Key: "v", Value: "during"}})
			require.NoError(t, err)

			cid := soleConflictID(t, main)
			var resolveRaw bson.M
			require.NoError(t, main.RunCommand(ctx, bson.D{
				{Key: "doltResolveConflict", Value: int32(1)},
				{Key: "collection", Value: tc.entry},
				{Key: "conflictId", Value: cid},
				{Key: "resolution", Value: "theirs"},
			}).Decode(&resolveRaw))
			require.EqualValues(t, 1, resolveRaw["ok"])

			assert.True(t, docExists(t, main.Collection("items"), 99),
				"edit during conflict must survive a %s resolution", tc.name)

			mergeContinue(t, main)
			assert.True(t, docExists(t, main.Collection("items"), 99),
				"edit during conflict must survive --continue")
		})
	}
}

// soleConflictID returns the id of the single conflict reported on db.
func soleConflictID(t *testing.T, db *mongo.Database) string {
	t.Helper()
	var raw bson.M
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "doltConflicts", Value: int32(1)},
	}).Decode(&raw))
	conflicts, ok := raw["conflicts"].(bson.A)
	require.True(t, ok, "conflicts must be an array, got %T", raw["conflicts"])
	require.Len(t, conflicts, 1, "expected exactly one conflict")
	return conflicts[0].(bson.M)["conflictId"].(string)
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
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"}}).Err())
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
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"}}).Err())
	_, err = main.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "main"}}}})
	require.NoError(t, err)
	_, err = feat.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "feat"}}}})
	require.NoError(t, err)

	raw := runCommandRaw(t, feat, bson.D{{Key: "dumboRebase", Value: int32(1)}, {Key: "onto", Value: "main"}})
	require.EqualValues(t, 0, raw["ok"], "rebase must conflict")

	assertConflictWindow(t, feat, bson.D{{Key: "dumboRebase", Value: int32(1)}, {Key: "onto", Value: "main"}, {Key: "continue", Value: int32(1)}})
}
