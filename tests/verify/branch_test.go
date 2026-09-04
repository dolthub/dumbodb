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

// TestBranchVerify is the automated analog of docs/verify/branch.md. dumboBranch
// takes an explicit action: "add", "update", "remove", or "list".

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

// branchVerifySetup mirrors the Setup section of docs/verify/branch.md.
func branchVerifySetup(t *testing.T, env *dumboDBTestEnv, dbName string) (hash1, hash2 string) {
	t.Helper()

	ctx := context.Background()
	db := env.Client.Database(dbName)
	items := db.Collection("products")

	require.NoError(t, db.Drop(ctx))

	_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "label", Value: "alpha"}})
	require.NoError(t, err)
	hash1 = dumboDBCommit(t, env, dbName, "commit one", "alice <alice@acme.com>")

	_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "label", Value: "beta"}})
	require.NoError(t, err)
	hash2 = dumboDBCommit(t, env, dbName, "commit two", "alice <alice@acme.com>")

	return hash1, hash2
}

// addBranch runs action:"add" for a branch on the given connection.
func addBranch(t *testing.T, env *dumboDBTestEnv, connDB, name string) {
	t.Helper()
	require.NoError(t, env.Client.Database(connDB).RunCommand(context.Background(), bson.D{
		{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: name},
	}).Err(), "add branch %q on %q", name, connDB)
}

func TestBranchVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("branchvrfy%d", rand.Int64N(1_000_000))

	hash1, _ := branchVerifySetup(t, env, dbName)

	// -------------------------------------------------------------------------
	// Scenario 1: Add a branch from main HEAD -- response shape
	// -------------------------------------------------------------------------
	t.Run("Scenario1_AddBranch_ResponseShape", func(t *testing.T) {
		var result bson.M
		err := env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"},
		}).Decode(&result)
		require.NoError(t, err, "doltBranch add must succeed")

		assert.Equal(t, "feature", result["branch"], "branch must echo the provided name")
		assert.EqualValues(t, 1, result["ok"], "ok must be 1")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: New branch points to the same commit as its source
	// -------------------------------------------------------------------------
	t.Run("Scenario2_NewBranchMatchesSourceCommit", func(t *testing.T) {
		addBranch(t, env, dbName+"@main", "snapshot")

		var raw bson.M
		require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltDiff", Value: int32(1)},
			{Key: "from", Value: "snapshot"},
			{Key: "to", Value: "main"},
		}).Decode(&raw))

		dr := decodeDiffResult(t, raw)
		assert.Empty(t, dr.Collections,
			"diff between new branch and source must be empty (identical commits)")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: Branch isolation -- writes on branch do not affect source
	// -------------------------------------------------------------------------
	t.Run("Scenario3_BranchIsolation", func(t *testing.T) {
		featureDB := env.Client.Database(dbName + "@feature")

		_, err := featureDB.Collection("products").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)}, {Key: "label", Value: "gamma"},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature adds gamma", "alice <alice@acme.com>")

		mainCount, err := env.Client.Database(dbName+"@main").Collection("products").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), mainCount,
			"main must still have 2 documents; feature write must not leak to main")

		featureCount, err := featureDB.Collection("products").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), featureCount,
			"feature branch must have 3 documents after committing _id:3")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: Add a branch from a commit hash rootish
	// -------------------------------------------------------------------------
	t.Run("Scenario4_AddBranchFromHashRootish", func(t *testing.T) {
		var result bson.M
		err := env.Client.Database(dbName+"@"+hash1).RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "at-commit-one"},
		}).Decode(&result)
		require.NoError(t, err, "add from hash rootish must succeed")

		assert.Equal(t, "at-commit-one", result["branch"])
		assert.EqualValues(t, 1, result["ok"])

		count, err := env.Client.Database(dbName+"@at-commit-one").Collection("products").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), count,
			"branch created from hash1 must see only 1 document (commit 1 state)")

		var docs []bson.M
		cursor, err := env.Client.Database(dbName+"@at-commit-one").Collection("products").Find(ctx, bson.D{})
		require.NoError(t, err)
		require.NoError(t, cursor.All(ctx, &docs))
		require.Len(t, docs, 1)
		assert.Equal(t, int32(1), docs[0]["_id"], "only _id:1 must be present at hash1 state")
	})

	// -------------------------------------------------------------------------
	// Scenario 5: Add a branch from an ancestor expression rootish
	// -------------------------------------------------------------------------
	t.Run("Scenario5_AddBranchFromAncestorExpression", func(t *testing.T) {
		ancDbName := fmt.Sprintf("branchvrfy_anc%d", rand.Int64N(1_000_000))
		branchVerifySetup(t, env, ancDbName)

		var result bson.M
		err := env.Client.Database(ancDbName+"@main~1").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "back-one"},
		}).Decode(&result)
		require.NoError(t, err, "add from ancestor expression rootish must succeed")

		assert.Equal(t, "back-one", result["branch"])
		assert.EqualValues(t, 1, result["ok"])

		count, err := env.Client.Database(ancDbName+"@back-one").Collection("products").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), count, "branch created from main~1 must see only 1 document")

		var docs []bson.M
		cursor, err := env.Client.Database(ancDbName+"@back-one").Collection("products").Find(ctx, bson.D{})
		require.NoError(t, err)
		require.NoError(t, cursor.All(ctx, &docs))
		require.Len(t, docs, 1)
		assert.Equal(t, int32(1), docs[0]["_id"], "only _id:1 must be visible at main~1 state")
	})

	// -------------------------------------------------------------------------
	// Scenario 6: Safe remove (force:false) -- branch already merged into main
	// -------------------------------------------------------------------------
	t.Run("Scenario6_SafeRemove_MergedBranch", func(t *testing.T) {
		delDbName := fmt.Sprintf("branchvrfy_del%d", rand.Int64N(1_000_000))
		branchVerifySetup(t, env, delDbName)
		addBranch(t, env, delDbName+"@main", "merged-branch")

		var result bson.M
		require.NoError(t, env.Client.Database(delDbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "remove"}, {Key: "branch", Value: "merged-branch"},
		}).Decode(&result), "safe remove of a merged branch must succeed")

		assert.Equal(t, "merged-branch", result["branch"])
		assert.EqualValues(t, 1, result["ok"])
	})

	// -------------------------------------------------------------------------
	// Scenario 7: Safe remove (force:false) -- branch has unmerged commits, rejected
	// -------------------------------------------------------------------------
	t.Run("Scenario7_SafeRemove_UnmergedBranch_Rejected", func(t *testing.T) {
		delDbName := fmt.Sprintf("branchvrfy_unm%d", rand.Int64N(1_000_000))
		branchVerifySetup(t, env, delDbName)
		addBranch(t, env, delDbName+"@main", "unmerged-branch")

		_, err := env.Client.Database(delDbName+"@unmerged-branch").Collection("products").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(99)}, {Key: "label", Value: "extra"},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, delDbName+"@unmerged-branch", "extra commit", "alice <alice@acme.com>")

		err = env.Client.Database(delDbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "remove"}, {Key: "branch", Value: "unmerged-branch"},
		}).Err()
		require.Error(t, err, "safe remove of a branch with unmerged commits must fail")
	})

	// -------------------------------------------------------------------------
	// Scenario 8: Force remove (force:true) -- branch has unmerged commits, succeeds
	// -------------------------------------------------------------------------
	t.Run("Scenario8_ForceRemove_UnmergedBranch", func(t *testing.T) {
		delDbName := fmt.Sprintf("branchvrfy_frc%d", rand.Int64N(1_000_000))
		branchVerifySetup(t, env, delDbName)
		addBranch(t, env, delDbName+"@main", "force-branch")

		_, err := env.Client.Database(delDbName+"@force-branch").Collection("products").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(77)}, {Key: "label", Value: "gone"},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, delDbName+"@force-branch", "unmerged commit", "alice <alice@acme.com>")

		var result bson.M
		require.NoError(t, env.Client.Database(delDbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "remove"},
			{Key: "branch", Value: "force-branch"}, {Key: "force", Value: true},
		}).Decode(&result), "force remove must succeed even with unmerged commits")

		assert.Equal(t, "force-branch", result["branch"])
		assert.EqualValues(t, 1, result["ok"])
	})

	// -------------------------------------------------------------------------
	// Scenario 9: Branch name that looks like a commit hash is rejected
	// -------------------------------------------------------------------------
	t.Run("Scenario9_CommitHashNameRejected", func(t *testing.T) {
		hashName := "na7kfra98h45fr2u5qtr30o2ggm7vh61"
		add := func(name string) error {
			return env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
				{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: name},
			}).Err()
		}

		require.Error(t, add(hashName), "branch name that looks like a commit hash must be rejected")
		require.Error(t, add("feature/"+hashName), "branch path ending with hash-like segment must be rejected")
		require.NoError(t, add(hashName+"/feature"), "hash-like segment in middle of path is allowed")
		require.NoError(t, add("NA7KFRA98H45FR2U5QTR30O2GGM7VH61"), "uppercase 32-char name is not a commit hash")
		require.NoError(t, add("team/alice/experiment"), "path-like branch names must work")
	})

	// -------------------------------------------------------------------------
	// Scenario 10: List branches (action:"list")
	// -------------------------------------------------------------------------
	listDbName := fmt.Sprintf("branchvrfy_list%d", rand.Int64N(1_000_000))

	t.Run("Scenario10_ListBranches", func(t *testing.T) {
		branchVerifySetup(t, env, listDbName)

		for _, name := range []string{"zeta", "alpha"} {
			addBranch(t, env, listDbName+"@main", name)
		}

		branches := listBranches(t, env, listDbName+"@main")

		require.Len(t, branches, 3, "listing must return every branch")
		assert.Equal(t, []string{"alpha", "main", "zeta"}, branchNames(branches),
			"branches must be sorted by name")

		for _, b := range branches {
			assert.NotEmpty(t, b.CommitID, "branch %q must report its HEAD commit", b.Name)
			assert.Equal(t, b.Name == "main", b.Current,
				"only the connection's branch may be current (branch %q)", b.Name)
		}

		assert.Equal(t, branches[0].CommitID, branches[1].CommitID,
			"branch created from main HEAD must share main's commit")
		assert.Equal(t, branches[1].CommitID, branches[2].CommitID,
			"branch created from main HEAD must share main's commit")
	})

	// -------------------------------------------------------------------------
	// Scenario 10 (cont.): "current" follows the connection, not the default branch
	// -------------------------------------------------------------------------
	t.Run("Scenario10_ListBranches_CurrentFollowsConnection", func(t *testing.T) {
		branches := listBranches(t, env, listDbName+"@zeta")

		require.Len(t, branches, 3)
		for _, b := range branches {
			assert.Equal(t, b.Name == "zeta", b.Current,
				"connection is on zeta, so only zeta may be current (branch %q)", b.Name)
		}
	})

	// -------------------------------------------------------------------------
	// Scenario 10 (cont.): a hash rootish is on no branch, so nothing is current
	// -------------------------------------------------------------------------
	t.Run("Scenario10_ListBranches_DetachedRootishHasNoCurrent", func(t *testing.T) {
		var logRes bson.M
		require.NoError(t, env.Client.Database(listDbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltLog", Value: int32(1)}, {Key: "limit", Value: int32(1)},
		}).Decode(&logRes))

		commits, ok := logRes["commits"].(bson.A)
		require.True(t, ok, "doltLog must return a commits array")
		require.NotEmpty(t, commits)
		headHash, ok := commits[0].(bson.M)["commitId"].(string)
		require.True(t, ok, "commit entry must carry a commitId string")

		branches := listBranches(t, env, listDbName+"@"+headHash)

		require.Len(t, branches, 3, "a detached connection still lists every branch")
		for _, b := range branches {
			assert.False(t, b.Current,
				"a hash rootish is on no branch, so %q must not be current", b.Name)
		}
	})

	// -------------------------------------------------------------------------
	// Scenario 11: action is required and per-action fields are validated
	// -------------------------------------------------------------------------
	t.Run("Scenario11_ActionValidation", func(t *testing.T) {
		conn := env.Client.Database(listDbName + "@main")
		run := func(cmd bson.D) error { return conn.RunCommand(ctx, cmd).Err() }

		err := run(bson.D{{Key: "doltBranch", Value: int32(1)}})
		require.Error(t, err, "action is required")
		assert.Contains(t, err.Error(), "action is required")

		err = run(bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "rename"}})
		require.Error(t, err, "unknown action must be rejected")
		assert.Contains(t, err.Error(), "unknown action")

		err = run(bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: ""}})
		require.Error(t, err, "add with empty branch must be rejected")
		assert.Contains(t, err.Error(), "must not be empty")

		err = run(bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "list"}, {Key: "branch", Value: "zeta"}})
		require.Error(t, err, "list with a branch field must be rejected")
		assert.Contains(t, err.Error(), "not valid with action")

		err = run(bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "remove"}})
		require.Error(t, err, "remove without a branch must be rejected")
		assert.Contains(t, err.Error(), "must not be empty")

		err = run(bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "update"}, {Key: "branch", Value: "main"}})
		require.Error(t, err, "update without setConfig must be rejected")
		assert.Contains(t, err.Error(), "requires setConfig")

		assert.Len(t, listBranches(t, env, listDbName+"@main"), 3,
			"rejected requests must leave the branch set untouched")
	})

	// -------------------------------------------------------------------------
	// Scenario 12: A listing includes remote-tracking branches with tracking info
	// -------------------------------------------------------------------------
	t.Run("Scenario12_ListIncludesRemoteTracking", func(t *testing.T) {
		rtDB := fmt.Sprintf("rtlist%d", rand.Int64N(1_000_000))
		conn := env.Client.Database(rtDB)
		require.NoError(t, conn.Drop(ctx))
		_, err := conn.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, rtDB, "c1", "alice <alice@acme.com>")
		addBranch(t, env, rtDB+"@main", "feature")

		require.NoError(t, conn.RunCommand(ctx, bson.D{
			{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
			{Key: "name", Value: "origin"}, {Key: "url", Value: "file://" + t.TempDir()},
		}).Err())
		require.NoError(t, env.Client.Database(rtDB+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Err())
		require.NoError(t, env.Client.Database(rtDB+"@feature").RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "feature"},
		}).Err())
		require.NoError(t, env.Client.Database(rtDB+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "update"}, {Key: "branch", Value: "main"},
			{Key: "setConfig", Value: bson.D{{Key: "pull", Value: bson.D{
				{Key: "remote", Value: "origin"}, {Key: "branch", Value: "main"},
			}}}},
		}).Err())

		var res bson.M
		require.NoError(t, env.Client.Database(rtDB+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "list"},
		}).Decode(&res))
		arr, ok := res["branches"].(bson.A)
		require.True(t, ok, "branches must be an array")
		byName := map[string]bson.M{}
		for _, e := range arr {
			m := e.(bson.M)
			byName[m["name"].(string)] = m
		}

		require.Contains(t, byName, "main")
		require.Contains(t, byName, "feature")
		mainEntry := byName["main"]
		assert.Equal(t, true, mainEntry["current"])
		_, hasRT := mainEntry["remoteTracking"]
		assert.False(t, hasRT, "a local branch is not remoteTracking")
		pull := mainEntry["config"].(bson.M)["pull"].(bson.M)
		assert.Equal(t, "origin", pull["remote"])
		assert.Equal(t, "main", pull["branch"])

		require.Contains(t, byName, "origin/main")
		require.Contains(t, byName, "origin/feature")
		for _, name := range []string{"origin/main", "origin/feature"} {
			e := byName[name]
			assert.Equal(t, true, e["remoteTracking"], "%s must be remoteTracking", name)
			assert.Equal(t, "origin", e["remote"])
			_, hasCurrent := e["current"]
			assert.False(t, hasCurrent, "%s must never be current (field omitted)", name)
			assert.NotEmpty(t, e["commitId"])
			_, hasConfig := e["config"]
			assert.False(t, hasConfig, "%s must carry no config", name)
		}
		assert.Contains(t, []interface{}{"main", "feature"}, byName["origin/main"]["ref"])
	})

	// -------------------------------------------------------------------------
	// Scenario 13: Branch config via setConfig; a value sets, null clears
	// -------------------------------------------------------------------------
	t.Run("Scenario13_BranchConfig", func(t *testing.T) {
		cfgDB := fmt.Sprintf("cfglist%d", rand.Int64N(1_000_000))
		conn := env.Client.Database(cfgDB)
		require.NoError(t, conn.Drop(ctx))
		_, err := conn.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, cfgDB, "c1", "alice <alice@acme.com>")
		addBranch(t, env, cfgDB+"@main", "feature")
		require.NoError(t, conn.RunCommand(ctx, bson.D{
			{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
			{Key: "name", Value: "origin"}, {Key: "url", Value: "file://" + t.TempDir()},
		}).Err())

		mainConn := env.Client.Database(cfgDB + "@main")
		var res bson.M

		update := func(setConfig bson.D) *mongo.SingleResult {
			return mainConn.RunCommand(ctx, bson.D{
				{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "update"},
				{Key: "branch", Value: "main"}, {Key: "setConfig", Value: setConfig},
			})
		}

		// Set config.pull identity + policy in one call.
		require.NoError(t, update(bson.D{{Key: "pull", Value: bson.D{
			{Key: "remote", Value: "origin"}, {Key: "branch", Value: "main"},
			{Key: "rebase", Value: true}, {Key: "ff", Value: "only"},
		}}}).Decode(&res))
		pull := res["config"].(bson.M)["pull"].(bson.M)
		assert.Equal(t, "origin", pull["remote"])
		assert.Equal(t, "main", pull["branch"])
		assert.Equal(t, "true", pull["rebase"])
		assert.Equal(t, "only", pull["ff"])

		// The listing surfaces config.pull on main.
		require.NoError(t, mainConn.RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "list"},
		}).Decode(&res))
		for _, e := range res["branches"].(bson.A) {
			m := e.(bson.M)
			if m["name"] == "main" {
				lc := m["config"].(bson.M)["pull"].(bson.M)
				assert.Equal(t, "true", lc["rebase"])
				assert.Equal(t, "only", lc["ff"])
			}
		}

		// config.push is a persistent triangular target; pull is untouched.
		require.NoError(t, update(bson.D{{Key: "push", Value: bson.D{
			{Key: "remote", Value: "origin"}, {Key: "branch", Value: "rev51"},
		}}}).Decode(&res))
		push := res["config"].(bson.M)["push"].(bson.M)
		assert.Equal(t, "origin", push["remote"])
		assert.Equal(t, "rev51", push["branch"])
		assert.Equal(t, "origin", res["config"].(bson.M)["pull"].(bson.M)["remote"])

		// null clears a single leaf; the rest of config.pull remains.
		require.NoError(t, update(bson.D{{Key: "pull", Value: bson.D{{Key: "rebase", Value: nil}}}}).Decode(&res))
		pull = res["config"].(bson.M)["pull"].(bson.M)
		_, hasRebase := pull["rebase"]
		assert.False(t, hasRebase, "pull.rebase was cleared by null")
		assert.Equal(t, "only", pull["ff"])

		// null on a whole sub-object drops config.push entirely.
		require.NoError(t, update(bson.D{{Key: "push", Value: nil}}).Decode(&res))
		_, hasPush := res["config"].(bson.M)["push"]
		assert.False(t, hasPush, "config.push was cleared by null")

		// null clears ff; config.pull keeps its identity.
		require.NoError(t, update(bson.D{{Key: "pull", Value: bson.D{{Key: "ff", Value: nil}}}}).Decode(&res))
		pull = res["config"].(bson.M)["pull"].(bson.M)
		_, hasFF := pull["ff"]
		assert.False(t, hasFF, "pull.ff was cleared by null")
		assert.Equal(t, "origin", pull["remote"], "config.pull identity is retained")

		// rebase:false is sugar for clear.
		require.NoError(t, update(bson.D{{Key: "pull", Value: bson.D{{Key: "rebase", Value: true}}}}).Decode(&res))
		require.Equal(t, "true", res["config"].(bson.M)["pull"].(bson.M)["rebase"])
		require.NoError(t, update(bson.D{{Key: "pull", Value: bson.D{{Key: "rebase", Value: false}}}}).Decode(&res))
		_, hasRebase = res["config"].(bson.M)["pull"].(bson.M)["rebase"]
		assert.False(t, hasRebase, "rebase:false clears like null")

		featureUpdate := func(setConfig bson.D) error {
			return env.Client.Database(cfgDB+"@feature").RunCommand(ctx, bson.D{
				{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "update"},
				{Key: "branch", Value: "feature"}, {Key: "setConfig", Value: setConfig},
			}).Err()
		}

		assert.Error(t, featureUpdate(bson.D{{Key: "pull", Value: bson.D{{Key: "rebase", Value: true}}}}),
			"rebase/ff require a config.pull upstream")
		assert.Error(t, featureUpdate(bson.D{{Key: "push", Value: bson.D{{Key: "remote", Value: "origin"}}}}),
			"config.push must set both remote and branch")

		assert.Error(t, update(bson.D{{Key: "pull", Value: bson.D{{Key: "remote", Value: "ghost"}}}}).Err(),
			"config.pull.remote must name a configured remote")

		bad := []struct {
			name string
			cfg  bson.D
		}{
			{"rebase merges", bson.D{{Key: "pull", Value: bson.D{{Key: "rebase", Value: "merges"}}}}},
			{"rebase string", bson.D{{Key: "pull", Value: bson.D{{Key: "rebase", Value: "true"}}}}},
			{"ff bad value", bson.D{{Key: "pull", Value: bson.D{{Key: "ff", Value: "sometimes"}}}}},
			{"ff default", bson.D{{Key: "pull", Value: bson.D{{Key: "ff", Value: "default"}}}}},
			{"ff bool", bson.D{{Key: "pull", Value: bson.D{{Key: "ff", Value: true}}}}},
			{"unknown pull leaf", bson.D{{Key: "pull", Value: bson.D{{Key: "squash", Value: true}}}}},
			{"unknown push leaf", bson.D{{Key: "push", Value: bson.D{{Key: "rebase", Value: true}}}}},
			{"unknown top key", bson.D{{Key: "rebase", Value: true}}},
			{"non-doc sub", bson.D{{Key: "pull", Value: "origin"}}},
			{"empty", bson.D{}},
		}
		for _, tc := range bad {
			assert.Error(t, update(tc.cfg).Err(), "setConfig %s must be rejected", tc.name)
		}
	})

	// -------------------------------------------------------------------------
	// Scenario 14: Removing a branch clears its config (no stale tracking)
	// -------------------------------------------------------------------------
	t.Run("Scenario14_RemoveClearsConfig", func(t *testing.T) {
		delDB := fmt.Sprintf("delcfg%d", rand.Int64N(1_000_000))
		conn := env.Client.Database(delDB)
		require.NoError(t, conn.Drop(ctx))
		_, err := conn.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, delDB, "c1", "alice <alice@acme.com>")
		for _, r := range []string{"origin", "origin2"} {
			require.NoError(t, conn.RunCommand(ctx, bson.D{
				{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
				{Key: "name", Value: r}, {Key: "url", Value: "file://" + t.TempDir()},
			}).Err())
		}

		addBranch(t, env, delDB+"@main", "release")
		require.NoError(t, env.Client.Database(delDB+"@release").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "update"}, {Key: "branch", Value: "release"},
			{Key: "setConfig", Value: bson.D{
				{Key: "pull", Value: bson.D{{Key: "remote", Value: "origin"}, {Key: "branch", Value: "main"}}},
				{Key: "push", Value: bson.D{{Key: "remote", Value: "origin2"}, {Key: "branch", Value: "release"}}},
			}},
		}).Err())

		require.NoError(t, env.Client.Database(delDB+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "remove"},
			{Key: "branch", Value: "release"}, {Key: "force", Value: true},
		}).Err())
		addBranch(t, env, delDB+"@main", "release")

		_, hasConfig := branchEntry(t, env, delDB, "release")["config"]
		assert.False(t, hasConfig, "a recreated branch must not inherit the deleted branch's config")

		var res bson.M
		err = env.Client.Database(delDB+"@release").RunCommand(ctx, bson.D{{Key: "dumboPush", Value: int32(1)}}).Decode(&res)
		assert.Error(t, err, "a bare push on the recreated branch must error (no config)")
	})

	// -------------------------------------------------------------------------
	// Scenario 15: action:"add" with setConfig applies config atomically
	// -------------------------------------------------------------------------
	t.Run("Scenario15_AddWithConfig", func(t *testing.T) {
		awDB := fmt.Sprintf("addcfg%d", rand.Int64N(1_000_000))
		conn := env.Client.Database(awDB)
		require.NoError(t, conn.Drop(ctx))
		_, err := conn.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, awDB, "c1", "alice <alice@acme.com>")
		require.NoError(t, conn.RunCommand(ctx, bson.D{
			{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
			{Key: "name", Value: "origin"}, {Key: "url", Value: "file://" + t.TempDir()},
		}).Err())

		// add + config in one call.
		var res bson.M
		require.NoError(t, env.Client.Database(awDB+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"},
			{Key: "setConfig", Value: bson.D{{Key: "pull", Value: bson.D{
				{Key: "remote", Value: "origin"}, {Key: "branch", Value: "main"},
			}}}},
		}).Decode(&res))
		pull := res["config"].(bson.M)["pull"].(bson.M)
		assert.Equal(t, "origin", pull["remote"])
		assert.Equal(t, "main", pull["branch"])

		// Invalid config rolls the branch back (add is atomic).
		err = env.Client.Database(awDB+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "bad"},
			{Key: "setConfig", Value: bson.D{{Key: "pull", Value: bson.D{{Key: "rebase", Value: true}}}}},
		}).Err()
		require.Error(t, err, "add with invalid config must fail")

		// The rolled-back branch is gone: a plain re-add succeeds.
		addBranch(t, env, awDB+"@main", "bad")
	})

	// -------------------------------------------------------------------------
	// Scenario 16: the default branch main can never be removed
	// -------------------------------------------------------------------------
	t.Run("Scenario16_CannotRemoveMain", func(t *testing.T) {
		// From any connection, removing main is refused with a reset suggestion.
		var res bson.M
		err := env.Client.Database(dbName+"@feature").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "remove"}, {Key: "branch", Value: "main"},
		}).Decode(&res)
		require.Error(t, err, "main must not be removable")
		assert.Contains(t, err.Error(), "default branch")
		assert.Contains(t, err.Error(), "dumboReset")

		// force does not override the guard.
		err = env.Client.Database(dbName+"@feature").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "remove"}, {Key: "branch", Value: "main"}, {Key: "force", Value: true},
		}).Decode(&res)
		require.Error(t, err, "force must not override the main guard")

		// main is still present.
		found := false
		for _, b := range listBranches(t, env, dbName+"@main") {
			if b.Name == "main" {
				found = true
			}
		}
		assert.True(t, found, "main must still exist")
	})
}

// branchListEntry is one entry of a doltBranch listing response.
type branchListEntry struct {
	Name     string
	CommitID string
	Current  bool
}

// listBranches runs action:"list" against connDB and decodes the listing.
func listBranches(t *testing.T, env *dumboDBTestEnv, connDB string) []branchListEntry {
	t.Helper()

	var res bson.M
	require.NoError(t, env.Client.Database(connDB).RunCommand(context.Background(), bson.D{
		{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "list"},
	}).Decode(&res), "doltBranch listing on %q", connDB)

	assert.EqualValues(t, 1, res["ok"], "ok must be 1")
	_, hasBranch := res["branch"]
	assert.False(t, hasBranch, "a listing must not carry the single-branch 'branch' field")

	raw, ok := res["branches"].(bson.A)
	require.True(t, ok, "doltBranch listing must return a branches array, got %#v", res["branches"])

	out := make([]branchListEntry, 0, len(raw))
	for i, e := range raw {
		doc, isDoc := e.(bson.M)
		require.True(t, isDoc, "branches[%d] must be a document", i)

		name, isStr := doc["name"].(string)
		require.True(t, isStr, "branches[%d].name must be a string", i)
		commitID, isStr := doc["commitId"].(string)
		require.True(t, isStr, "branches[%d].commitId must be a string", i)
		current, _ := doc["current"].(bool)

		out = append(out, branchListEntry{Name: name, CommitID: commitID, Current: current})
	}

	return out
}

func branchNames(entries []branchListEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}
