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

// TestBranchVerify is the automated analog of docs/verify/branch.md.
//
// Each top-level subtest corresponds to one scenario in that document.
// The setup reproduces the manual setup block exactly:
//
//   - Commit 1 (hash1): products = [ { _id:1, label:"alpha" } ]
//   - Commit 2 (hash2, HEAD): products = [ { _id:1, ... }, { _id:2, label:"beta" } ]
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
	"go.mongodb.org/mongo-driver/v2/bson"
)

// branchVerifySetup mirrors the Setup section of docs/verify/branch.md.
// Returns hash1 (commit 1) and hash2 (commit 2, same as main HEAD).
func branchVerifySetup(t *testing.T, env *dumboDBTestEnv, dbName string) (hash1, hash2 string) {
	t.Helper()

	ctx := context.Background()
	db := env.Client.Database(dbName)
	items := db.Collection("products")

	require.NoError(t, db.Drop(ctx))

	// Commit 1: one document.
	_, err := items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "label", Value: "alpha"},
	})
	require.NoError(t, err)
	hash1 = dumboDBCommit(t, env, dbName, "commit one", "alice <alice@acme.com>")

	// Commit 2: second document added.
	_, err = items.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "label", Value: "beta"},
	})
	require.NoError(t, err)
	hash2 = dumboDBCommit(t, env, dbName, "commit two", "alice <alice@acme.com>")

	return hash1, hash2
}

func TestBranchVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("branchvrfy%d", rand.Int64N(1_000_000))

	hash1, _ := branchVerifySetup(t, env, dbName)

	// -------------------------------------------------------------------------
	// Scenario 1: Create branch from main HEAD  -- response shape
	// -------------------------------------------------------------------------
	t.Run("Scenario1_CreateBranch_ResponseShape", func(t *testing.T) {
		var result bson.M
		err := env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "feature"},
		}).Decode(&result)
		require.NoError(t, err, "doltBranch must succeed")

		assert.Equal(t, "feature", result["branch"], "branch must echo the provided name")
		assert.EqualValues(t, 1, result["ok"], "ok must be 1")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: New branch points to the same commit as its source
	// -------------------------------------------------------------------------
	t.Run("Scenario2_NewBranchMatchesSourceCommit", func(t *testing.T) {
		// Create "snapshot" branch from current main HEAD.
		require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "snapshot"},
		}).Err(), "doltBranch to create 'snapshot'")

		// Diff main vs snapshot  -- identical commits -> empty collections.
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
	// Scenario 3: Branch isolation  -- writes on branch do not affect source
	// -------------------------------------------------------------------------
	t.Run("Scenario3_BranchIsolation", func(t *testing.T) {
		featureDB := env.Client.Database(dbName + "@feature")

		// Insert _id:3 on the feature branch and commit it.
		_, err := featureDB.Collection("products").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(3)},
			{Key: "label", Value: "gamma"},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature adds gamma", "alice <alice@acme.com>")

		// main must still have exactly 2 documents.
		mainCount, err := env.Client.Database(dbName+"@main").Collection("products").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(2), mainCount,
			"main must still have 2 documents; feature write must not leak to main")

		// feature must have 3 documents.
		featureCount, err := featureDB.Collection("products").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), featureCount,
			"feature branch must have 3 documents after committing _id:3")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: Create branch from a commit hash rootish
	// -------------------------------------------------------------------------
	t.Run("Scenario4_CreateBranchFromHashRootish", func(t *testing.T) {
		// Create "at-commit-one" from the commit-hash rootish at hash1.
		var result bson.M
		err := env.Client.Database(dbName+"@"+hash1).RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "at-commit-one"},
		}).Decode(&result)
		require.NoError(t, err, "doltBranch from hash rootish must succeed")

		assert.Equal(t, "at-commit-one", result["branch"])
		assert.EqualValues(t, 1, result["ok"])

		// The new branch must see only the one document from commit 1.
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
	// Scenario 5: Create branch from an ancestor expression rootish
	// Uses a fresh isolated database to avoid state drift from prior scenarios.
	// -------------------------------------------------------------------------
	t.Run("Scenario5_CreateBranchFromAncestorExpression", func(t *testing.T) {
		// Fresh database with a controlled two-commit history.
		ancDbName := fmt.Sprintf("branchvrfy_anc%d", rand.Int64N(1_000_000))
		ancHash1, _ := branchVerifySetup(t, env, ancDbName)
		_ = ancHash1

		// main~1 resolves to commit 1 (one document).
		var result bson.M
		err := env.Client.Database(ancDbName+"@main~1").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "back-one"},
		}).Decode(&result)
		require.NoError(t, err, "doltBranch from ancestor expression rootish must succeed")

		assert.Equal(t, "back-one", result["branch"])
		assert.EqualValues(t, 1, result["ok"])

		// back-one must see only one document (state at main~1 = commit 1).
		count, err := env.Client.Database(ancDbName+"@back-one").Collection("products").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), count,
			"branch created from main~1 must see only 1 document")

		var docs []bson.M
		cursor, err := env.Client.Database(ancDbName+"@back-one").Collection("products").Find(ctx, bson.D{})
		require.NoError(t, err)
		require.NoError(t, cursor.All(ctx, &docs))
		require.Len(t, docs, 1)
		assert.Equal(t, int32(1), docs[0]["_id"], "only _id:1 must be visible at main~1 state")
	})

	// -------------------------------------------------------------------------
	// Scenario 6: Safe delete (-d)  -- branch already merged into main
	// -------------------------------------------------------------------------
	t.Run("Scenario6_SafeDelete_MergedBranch", func(t *testing.T) {
		delDbName := fmt.Sprintf("branchvrfy_del%d", rand.Int64N(1_000_000))
		branchVerifySetup(t, env, delDbName)

		// Create "merged-branch" from main HEAD.
		require.NoError(t, env.Client.Database(delDbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "merged-branch"},
		}).Err(), "creating merged-branch must succeed")

		// Safe delete: merged-branch HEAD equals main HEAD, so it is reachable from
		// main  -- delete must succeed.
		var result bson.M
		require.NoError(t, env.Client.Database(delDbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "merged-branch"},
			{Key: "delete", Value: int32(1)},
		}).Decode(&result), "safe delete of a merged branch must succeed")

		assert.Equal(t, "merged-branch", result["branch"])
		assert.EqualValues(t, 1, result["ok"])
	})

	// -------------------------------------------------------------------------
	// Scenario 7: Safe delete (-d)  -- branch has unmerged commits, rejected
	// -------------------------------------------------------------------------
	t.Run("Scenario7_SafeDelete_UnmergedBranch_Rejected", func(t *testing.T) {
		delDbName := fmt.Sprintf("branchvrfy_unm%d", rand.Int64N(1_000_000))
		branchVerifySetup(t, env, delDbName)

		// Create "unmerged-branch" from main and advance it with an extra commit.
		require.NoError(t, env.Client.Database(delDbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "unmerged-branch"},
		}).Err(), "creating unmerged-branch must succeed")

		_, err := env.Client.Database(delDbName+"@unmerged-branch").Collection("products").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(99)},
			{Key: "label", Value: "extra"},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, delDbName+"@unmerged-branch", "extra commit", "alice <alice@acme.com>")

		// Safe delete must be rejected because unmerged-branch has a commit not in main.
		err = env.Client.Database(delDbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "unmerged-branch"},
			{Key: "delete", Value: int32(1)},
		}).Err()
		require.Error(t, err, "safe delete of a branch with unmerged commits must fail")
	})

	// -------------------------------------------------------------------------
	// Scenario 8: Force delete (-D)  -- branch has unmerged commits, succeeds
	// -------------------------------------------------------------------------
	t.Run("Scenario8_ForceDelete_UnmergedBranch", func(t *testing.T) {
		delDbName := fmt.Sprintf("branchvrfy_frc%d", rand.Int64N(1_000_000))
		branchVerifySetup(t, env, delDbName)

		// Create "force-branch" and advance it with an extra commit.
		require.NoError(t, env.Client.Database(delDbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "force-branch"},
		}).Err(), "creating force-branch must succeed")

		_, err := env.Client.Database(delDbName+"@force-branch").Collection("products").InsertOne(ctx, bson.D{
			{Key: "_id", Value: int32(77)},
			{Key: "label", Value: "gone"},
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, delDbName+"@force-branch", "unmerged commit", "alice <alice@acme.com>")

		// Force delete must succeed regardless of merge status.
		var result bson.M
		require.NoError(t, env.Client.Database(delDbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "force-branch"},
			{Key: "forceDelete", Value: int32(1)},
		}).Decode(&result), "force delete must succeed even with unmerged commits")

		assert.Equal(t, "force-branch", result["branch"])
		assert.EqualValues(t, 1, result["ok"])
	})

	// -------------------------------------------------------------------------
	// Scenario 9: Branch name that looks like a commit hash is rejected
	// -------------------------------------------------------------------------
	t.Run("Scenario9_CommitHashNameRejected", func(t *testing.T) {
		// 32 lowercase base32 chars -- looks like a commit hash.
		hashName := "na7kfra98h45fr2u5qtr30o2ggm7vh61"
		err := env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: hashName},
		}).Err()
		require.Error(t, err, "branch name that looks like a commit hash must be rejected")

		// Path ending with a hash-like segment is rejected.
		err = env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "feature/" + hashName},
		}).Err()
		require.Error(t, err, "branch path ending with hash-like segment must be rejected")

		// Hash-like segment in the middle is fine -- only the last segment matters.
		err = env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: hashName + "/feature"},
		}).Err()
		require.NoError(t, err, "hash-like segment in middle of path is allowed")

		// Upper case is fine -- commit hashes are always lowercase.
		err = env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "NA7KFRA98H45FR2U5QTR30O2GGM7VH61"},
		}).Err()
		require.NoError(t, err, "uppercase 32-char name is not a commit hash")

		// Path-like branch names work.
		err = env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: "team/alice/experiment"},
		}).Err()
		require.NoError(t, err, "path-like branch names must work")
	})

	// -------------------------------------------------------------------------
	// Scenario 10: List branches  -- no "branch" argument
	// Uses a fresh isolated database so the listing is exact.
	// -------------------------------------------------------------------------
	listDbName := fmt.Sprintf("branchvrfy_list%d", rand.Int64N(1_000_000))

	t.Run("Scenario10_ListBranches", func(t *testing.T) {
		branchVerifySetup(t, env, listDbName)

		// Created out of alphabetical order to prove the listing is sorted.
		for _, name := range []string{"zeta", "alpha"} {
			require.NoError(t, env.Client.Database(listDbName+"@main").RunCommand(ctx, bson.D{
				{Key: "doltBranch", Value: int32(1)},
				{Key: "branch", Value: name},
			}).Err(), "creating %q must succeed", name)
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

		// alpha and zeta branched from main HEAD, so all three share a commit.
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
			{Key: "doltLog", Value: int32(1)},
			{Key: "limit", Value: int32(1)},
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
	// Scenario 11: Listing does not swallow a malformed request
	// -------------------------------------------------------------------------
	t.Run("Scenario11_ListModeRequiresAbsentBranch", func(t *testing.T) {
		// An explicit empty string is an error, not a list request.
		err := env.Client.Database(listDbName+"@main").RunCommand(ctx, bson.D{
			{Key: "doltBranch", Value: int32(1)},
			{Key: "branch", Value: ""},
		}).Err()
		require.Error(t, err, "empty branch name must be rejected, not treated as a list")
		assert.Contains(t, err.Error(), "must not be empty")

		// Deleting still requires a name.
		for _, flag := range []string{"delete", "forceDelete"} {
			err = env.Client.Database(listDbName+"@main").RunCommand(ctx, bson.D{
				{Key: "doltBranch", Value: int32(1)},
				{Key: flag, Value: int32(1)},
			}).Err()
			require.Error(t, err, "%s without a branch name must be rejected", flag)
			assert.Contains(t, err.Error(), "branch name is required for delete")
		}

		// The rejections must not have deleted anything.
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
		require.NoError(t, env.Client.Database(rtDB+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "feature"},
		}).Err())

		// A remote with both branches pushed -- push writes the tracking refs
		// refs/remotes/origin/<branch> into the local store. Track main's upstream
		// with setConfig (config.pull).
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
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "main"},
			{Key: "setConfig", Value: bson.D{{Key: "pull", Value: bson.D{
				{Key: "remote", Value: "origin"}, {Key: "branch", Value: "main"},
			}}}},
		}).Err())

		// The default listing includes local AND remote-tracking branches.
		var res bson.M
		require.NoError(t, env.Client.Database(rtDB+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)},
		}).Decode(&res))
		arr, ok := res["branches"].(bson.A)
		require.True(t, ok, "branches must be an array")
		byName := map[string]bson.M{}
		for _, e := range arr {
			m := e.(bson.M)
			byName[m["name"].(string)] = m
		}

		// Local branches, main with its config.pull tracking info.
		require.Contains(t, byName, "main")
		require.Contains(t, byName, "feature")
		mainEntry := byName["main"]
		assert.Equal(t, true, mainEntry["current"])
		_, hasRT := mainEntry["remoteTracking"]
		assert.False(t, hasRT, "a local branch is not remoteTracking")
		pull := mainEntry["config"].(bson.M)["pull"].(bson.M)
		assert.Equal(t, "origin", pull["remote"])
		assert.Equal(t, "main", pull["branch"])

		// Remote-tracking branches, with remote/ref and never current.
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
	// Scenario 13: Set, list, and clear a tracking branch's pull policy
	// -------------------------------------------------------------------------
	t.Run("Scenario13_PullPolicyConfig", func(t *testing.T) {
		cfgDB := fmt.Sprintf("cfglist%d", rand.Int64N(1_000_000))
		conn := env.Client.Database(cfgDB)
		require.NoError(t, conn.Drop(ctx))
		_, err := conn.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, cfgDB, "c1", "alice <alice@acme.com>")
		require.NoError(t, env.Client.Database(cfgDB+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "feature"},
		}).Err())
		// main tracks a remote; feature does not.
		require.NoError(t, conn.RunCommand(ctx, bson.D{
			{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
			{Key: "name", Value: "origin"}, {Key: "url", Value: "file://" + t.TempDir()},
		}).Err())
		require.NoError(t, env.Client.Database(cfgDB+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Err())

		mainConn := env.Client.Database(cfgDB + "@main")
		var res bson.M

		// Set config.pull identity + policy in one call; the response echoes it.
		require.NoError(t, mainConn.RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "main"},
			{Key: "setConfig", Value: bson.D{{Key: "pull", Value: bson.D{
				{Key: "remote", Value: "origin"}, {Key: "branch", Value: "main"},
				{Key: "rebase", Value: true}, {Key: "ff", Value: "only"},
			}}}},
		}).Decode(&res))
		pull := res["config"].(bson.M)["pull"].(bson.M)
		assert.Equal(t, "origin", pull["remote"])
		assert.Equal(t, "main", pull["branch"])
		assert.Equal(t, "true", pull["rebase"])
		assert.Equal(t, "only", pull["ff"])

		// The listing surfaces config.pull on the local main entry.
		require.NoError(t, mainConn.RunCommand(ctx, bson.D{{Key: "dumboBranch", Value: int32(1)}}).Decode(&res))
		for _, e := range res["branches"].(bson.A) {
			m := e.(bson.M)
			if m["name"] == "main" {
				lc := m["config"].(bson.M)["pull"].(bson.M)
				assert.Equal(t, "true", lc["rebase"])
				assert.Equal(t, "only", lc["ff"])
			}
		}

		// config.push is a persistent, differently-named triangular target.
		require.NoError(t, mainConn.RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "main"},
			{Key: "setConfig", Value: bson.D{{Key: "push", Value: bson.D{
				{Key: "remote", Value: "origin"}, {Key: "branch", Value: "rev51"},
			}}}},
		}).Decode(&res))
		push := res["config"].(bson.M)["push"].(bson.M)
		assert.Equal(t, "origin", push["remote"])
		assert.Equal(t, "rev51", push["branch"])
		// The push update leaves config.pull untouched.
		assert.Equal(t, "origin", res["config"].(bson.M)["pull"].(bson.M)["remote"])

		// unsetConfig clears one leaf; the rest of config.pull remains.
		require.NoError(t, mainConn.RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "main"},
			{Key: "unsetConfig", Value: bson.A{"pull.rebase"}},
		}).Decode(&res))
		pull = res["config"].(bson.M)["pull"].(bson.M)
		_, hasRebase := pull["rebase"]
		assert.False(t, hasRebase, "pull.rebase was unset")
		assert.Equal(t, "only", pull["ff"])

		// unsetConfig on a whole sub-object drops config.push entirely.
		require.NoError(t, mainConn.RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "main"},
			{Key: "unsetConfig", Value: bson.A{"push"}},
		}).Decode(&res))
		_, hasPush := res["config"].(bson.M)["push"]
		assert.False(t, hasPush, "config.push was unset")

		// pull.ff:"default" clears ff; config.pull keeps only its identity.
		require.NoError(t, mainConn.RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "main"},
			{Key: "setConfig", Value: bson.D{{Key: "pull", Value: bson.D{{Key: "ff", Value: "default"}}}}},
		}).Decode(&res))
		pull = res["config"].(bson.M)["pull"].(bson.M)
		_, hasFF := pull["ff"]
		assert.False(t, hasFF, "pull.ff was cleared to default")
		assert.Equal(t, "origin", pull["remote"], "config.pull identity is retained")

		// A rebase/ff policy requires an upstream: feature has no config.pull -> error.
		err = env.Client.Database(cfgDB+"@feature").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "feature"},
			{Key: "setConfig", Value: bson.D{{Key: "pull", Value: bson.D{{Key: "rebase", Value: true}}}}},
		}).Err()
		assert.Error(t, err, "a pull policy applies only to a branch with a config.pull upstream")

		// A partial config.push (remote without branch) is rejected at set time.
		err = env.Client.Database(cfgDB+"@feature").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "feature"},
			{Key: "setConfig", Value: bson.D{{Key: "push", Value: bson.D{{Key: "remote", Value: "origin"}}}}},
		}).Err()
		assert.Error(t, err, "config.push must set both remote and branch")

		// An unknown remote is rejected.
		err = mainConn.RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "main"},
			{Key: "setConfig", Value: bson.D{{Key: "pull", Value: bson.D{{Key: "remote", Value: "ghost"}}}}},
		}).Err()
		assert.Error(t, err, "config.pull.remote must name a configured remote")

		// Invalid values, keys, and paths are rejected.
		bad := []struct {
			name string
			cfg  bson.D
		}{
			{"rebase merges", bson.D{{Key: "pull", Value: bson.D{{Key: "rebase", Value: "merges"}}}}}, // rebase is a bool
			{"rebase string", bson.D{{Key: "pull", Value: bson.D{{Key: "rebase", Value: "true"}}}}},   // not a bool
			{"ff bad value", bson.D{{Key: "pull", Value: bson.D{{Key: "ff", Value: "sometimes"}}}}},   // no/only/default
			{"ff bool", bson.D{{Key: "pull", Value: bson.D{{Key: "ff", Value: true}}}}},               // ff is a string
			{"unknown pull leaf", bson.D{{Key: "pull", Value: bson.D{{Key: "squash", Value: true}}}}}, // unknown leaf
			{"unknown push leaf", bson.D{{Key: "push", Value: bson.D{{Key: "rebase", Value: true}}}}}, // push has no rebase
			{"unknown top key", bson.D{{Key: "rebase", Value: true}}},                                 // must be pull/push
			{"non-doc sub", bson.D{{Key: "pull", Value: "origin"}}},                                   // pull must be a doc
		}
		for _, tc := range bad {
			err = mainConn.RunCommand(ctx, bson.D{
				{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "main"},
				{Key: "setConfig", Value: tc.cfg},
			}).Err()
			assert.Error(t, err, "setConfig %s must be rejected", tc.name)
		}

		// An unknown unsetConfig path is rejected.
		err = mainConn.RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "main"},
			{Key: "unsetConfig", Value: bson.A{"pull.squash"}},
		}).Err()
		assert.Error(t, err, "an unknown unsetConfig path must be rejected")

		// setConfig and unsetConfig may not both name the same leaf.
		err = mainConn.RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "main"},
			{Key: "setConfig", Value: bson.D{{Key: "pull", Value: bson.D{{Key: "ff", Value: "no"}}}}},
			{Key: "unsetConfig", Value: bson.A{"pull.ff"}},
		}).Err()
		assert.Error(t, err, "the same leaf in setConfig and unsetConfig must be rejected")

		// NOTE: rebase + ff together is *allowed* (git-parity: rebase wins on
		// pull, ff is inert) -- see the valid set at the top of this scenario.
	})
}

// branchListEntry is one entry of a doltBranch listing response.
type branchListEntry struct {
	Name     string
	CommitID string
	Current  bool
}

// listBranches runs doltBranch with no branch name against connDB and decodes
// the resulting listing.
func listBranches(t *testing.T, env *dumboDBTestEnv, connDB string) []branchListEntry {
	t.Helper()

	var res bson.M
	require.NoError(t, env.Client.Database(connDB).RunCommand(context.Background(), bson.D{
		{Key: "doltBranch", Value: int32(1)},
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
		// current is present (true) only on the checked-out branch; absent
		// elsewhere.
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
