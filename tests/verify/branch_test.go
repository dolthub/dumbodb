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
	// Scenario 12: List remote-tracking branches (git branch -r / -a)
	// -------------------------------------------------------------------------
	t.Run("Scenario12_ListRemoteTrackingBranches", func(t *testing.T) {
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
		// refs/remotes/origin/<branch> into the local store.
		require.NoError(t, conn.RunCommand(ctx, bson.D{
			{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
			{Key: "name", Value: "origin"}, {Key: "url", Value: "file://" + t.TempDir()},
		}).Err())
		for _, br := range []string{"main", "feature"} {
			require.NoError(t, env.Client.Database(rtDB+"@"+br).RunCommand(ctx, bson.D{
				{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: br},
			}).Err())
		}

		list := func(opts bson.D) []bson.M {
			cmd := append(bson.D{{Key: "dumboBranch", Value: int32(1)}}, opts...)
			var res bson.M
			require.NoError(t, env.Client.Database(rtDB+"@main").RunCommand(ctx, cmd).Decode(&res))
			arr, ok := res["branches"].(bson.A)
			require.True(t, ok, "branches must be an array")
			out := make([]bson.M, len(arr))
			for i, e := range arr {
				out[i] = e.(bson.M)
			}
			return out
		}
		names := func(entries []bson.M) []string {
			ns := make([]string, len(entries))
			for i, e := range entries {
				ns[i] = e["name"].(string)
			}
			return ns
		}

		// Default: local only, and no remoteTracking marker.
		local := list(bson.D{})
		assert.ElementsMatch(t, []string{"main", "feature"}, names(local))
		for _, e := range local {
			_, hasRT := e["remoteTracking"]
			assert.False(t, hasRT, "a local listing must not mark remoteTracking")
		}

		// remote:true -> remote-tracking only (git branch -r).
		rem := list(bson.D{{Key: "remote", Value: true}})
		assert.ElementsMatch(t, []string{"origin/feature", "origin/main"}, names(rem))
		for _, e := range rem {
			assert.Equal(t, true, e["remoteTracking"])
			assert.Equal(t, "origin", e["remote"])
			assert.Equal(t, false, e["current"], "a remote-tracking branch is never current")
			assert.NotEmpty(t, e["commitId"])
			assert.Contains(t, []string{"main", "feature"}, e["ref"])
		}

		// all:true -> both (git branch -a).
		assert.ElementsMatch(t,
			[]string{"main", "feature", "origin/main", "origin/feature"},
			names(list(bson.D{{Key: "all", Value: true}})))
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
		current, isBool := doc["current"].(bool)
		require.True(t, isBool, "branches[%d].current must be a bool", i)

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
