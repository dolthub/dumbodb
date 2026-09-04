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

// TestPullVerify is the automated analog of docs/verify/pull.md, covering both
// dumboFetch and dumboPull. A shared setup publishes a hub remote and clones a
// working database; the scenarios advance the hub and fetch/pull.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestPullVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	suffix := rand.Int64N(1_000_000)
	hubName := fmt.Sprintf("hub%d", suffix)
	workName := fmt.Sprintf("work%d", suffix)
	hubDir := t.TempDir()
	hubURL := "file://" + hubDir
	admin := env.Client.Database("admin")
	hub := env.Client.Database(hubName)
	work := env.Client.Database(workName)

	pushHub := func() {
		var res bson.M
		require.NoError(t, hub.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Decode(&res))
	}

	tipParents := func(t *testing.T, connDB string) (parent1, parent2 string) {
		t.Helper()
		var res bson.M
		require.NoError(t, env.Client.Database(connDB).RunCommand(ctx, bson.D{
			{Key: "dumboLog", Value: int32(1)}, {Key: "limit", Value: int32(1)},
		}).Decode(&res))
		commits, ok := res["commits"].(bson.A)
		require.True(t, ok && len(commits) > 0, "dumboLog must return commits")
		tip := commits[0].(bson.M)
		parent1, _ = tip["parent1"].(string)
		parent2, _ = tip["parent2"].(string)
		return parent1, parent2
	}

	// Setup: hub with c1 on main, pushed; a working clone tracking origin/main.
	require.NoError(t, hub.Drop(ctx))
	_, err := hub.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "one"}})
	require.NoError(t, err)
	h1 := dumboDBCommit(t, env, hubName, "c1", "alice <alice@acme.com>")
	var res bson.M
	require.NoError(t, hub.RunCommand(ctx, bson.D{
		{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
		{Key: "name", Value: "origin"}, {Key: "url", Value: hubURL},
	}).Decode(&res))
	pushHub()
	require.NoError(t, admin.RunCommand(ctx, bson.D{
		{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: hubURL}, {Key: "as", Value: workName},
	}).Decode(&res))

	// -------------------------------------------------------------------------
	// Scenario 1: dumboFetch updates tracking refs without moving local branches
	// -------------------------------------------------------------------------
	var h2 string
	t.Run("Scenario1_FetchUpdatesTrackingRefs", func(t *testing.T) {
		_, err := hub.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "two"}})
		require.NoError(t, err)
		h2 = dumboDBCommit(t, env, hubName, "c2", "alice <alice@acme.com>")
		pushHub()

		var res bson.M
		require.NoError(t, work.RunCommand(ctx, bson.D{
			{Key: "dumboFetch", Value: int32(1)}, {Key: "from", Value: "origin"},
		}).Decode(&res))
		arr := res["branches"].(bson.A)
		require.Len(t, arr, 1)
		mainRef := arr[0].(bson.M)
		assert.Equal(t, "main", mainRef["branch"])
		assert.Equal(t, h1, mainRef["commitBefore"])
		assert.Equal(t, h2, mainRef["commit"])

		n, err := work.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 1, n, "fetch must not move the local branch")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: dumboFetch with no remote uses the upstream
	// -------------------------------------------------------------------------
	t.Run("Scenario2_FetchUsesUpstream", func(t *testing.T) {
		var res bson.M
		require.NoError(t, work.RunCommand(ctx, bson.D{{Key: "dumboFetch", Value: int32(1)}}).Decode(&res))
		assert.Equal(t, "origin", res["remote"])
		arr, _ := res["branches"].(bson.A)
		assert.Empty(t, arr, "an up-to-date fetch reports no updated branches")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: dumboPull fast-forwards the branch
	// -------------------------------------------------------------------------
	t.Run("Scenario3_PullFastForward", func(t *testing.T) {
		var res bson.M
		require.NoError(t, env.Client.Database(workName+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboPull", Value: int32(1)},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, h1, res["commitBefore"])
		assert.Equal(t, h2, res["commitAfter"])
		assert.Equal(t, true, res["fastForward"])
		assert.Equal(t, false, res["alreadyUpToDate"])

		n, err := work.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 2, n, "the pulled data is now present")
	})

	// -------------------------------------------------------------------------
	// Scenario 4: dumboPull with nothing new is up to date
	// -------------------------------------------------------------------------
	t.Run("Scenario4_PullUpToDate", func(t *testing.T) {
		var res bson.M
		require.NoError(t, env.Client.Database(workName+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboPull", Value: int32(1)},
		}).Decode(&res))
		assert.Equal(t, true, res["alreadyUpToDate"])
		assert.Equal(t, false, res["fastForward"])
		assert.Equal(t, res["commitBefore"], res["commitAfter"])
	})

	// -------------------------------------------------------------------------
	// Scenario 5: dumboPull with local and remote changes creates a merge commit
	// -------------------------------------------------------------------------
	t.Run("Scenario5_PullMergeCommit", func(t *testing.T) {
		_, err := work.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(100)}, {Key: "v", Value: "local"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, workName, "local change", "bob <bob@acme.com>")

		_, err = hub.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "v", Value: "three"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, hubName, "c3", "alice <alice@acme.com>")
		pushHub()

		var res bson.M
		require.NoError(t, env.Client.Database(workName+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboPull", Value: int32(1)}, {Key: "message", Value: "merge origin"}, {Key: "author", Value: "bob <bob@acme.com>"},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, false, res["fastForward"])
		assert.Equal(t, false, res["alreadyUpToDate"])
		assert.NotEqual(t, res["commitBefore"], res["commitAfter"], "a merge commit was created")

		p1, p2 := tipParents(t, workName+"@main")
		assert.Equal(t, res["commitBefore"], p1, "parent1 is the pre-pull head")
		assert.NotEmpty(t, p2, "a merge commit has a parent2")

		n, err := work.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 4, n, "both sides' documents are present")
	})

	// -------------------------------------------------------------------------
	// Scenario 6: ffOnly rejects a non-fast-forward pull
	// -------------------------------------------------------------------------
	t.Run("Scenario6_PullFFOnlyRejected", func(t *testing.T) {
		ffName := fmt.Sprintf("ffwork%d", suffix)
		require.NoError(t, admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: hubURL}, {Key: "as", Value: ffName},
		}).Decode(&res))

		ff := env.Client.Database(ffName)
		_, err := ff.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(200)}, {Key: "v", Value: "ff-local"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, ffName, "ff local", "bob <bob@acme.com>")

		_, err = hub.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(4)}, {Key: "v", Value: "four"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, hubName, "c4", "alice <alice@acme.com>")
		pushHub()

		var res bson.M
		err = env.Client.Database(ffName+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboPull", Value: int32(1)}, {Key: "ffOnly", Value: true},
		}).Decode(&res)
		assert.Error(t, err, "a non-fast-forward pull with ffOnly must fail")
	})

	// -------------------------------------------------------------------------
	// Scenario 7: dumboPull with no upstream is an error
	// -------------------------------------------------------------------------
	t.Run("Scenario7_PullNoUpstreamErrors", func(t *testing.T) {
		soloName := fmt.Sprintf("solo%d", suffix)
		solo := env.Client.Database(soloName)
		require.NoError(t, solo.Drop(ctx))
		_, err := solo.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, soloName, "seed", "a <a@a>")

		var res bson.M
		err = env.Client.Database(soloName+"@main").RunCommand(ctx, bson.D{{Key: "dumboPull", Value: int32(1)}}).Decode(&res)
		assert.Error(t, err, "pull with no upstream must fail")
	})

	// -------------------------------------------------------------------------
	// Scenario 8: Conflicting pull reports conflicts
	// -------------------------------------------------------------------------
	t.Run("Scenario8_ConflictingPull", func(t *testing.T) {
		cfName := fmt.Sprintf("cfwork%d", suffix)
		require.NoError(t, admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: hubURL}, {Key: "as", Value: cfName},
		}).Decode(&res))

		cf := env.Client.Database(cfName)
		_, err := cf.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "clone-edit"}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, cfName, "clone edit", "bob <bob@acme.com>")

		_, err = hub.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "hub-edit"}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, hubName, "hub edit", "alice <alice@acme.com>")
		pushHub()

		raw := runCommandRaw(t, env.Client.Database(cfName+"@main"), bson.D{
			{Key: "dumboPull", Value: int32(1)}, {Key: "message", Value: "pull"}, {Key: "author", Value: "bob <bob@acme.com>"},
		})
		require.EqualValues(t, 0, raw["ok"], "a conflicting pull must return ok:0")
		conflicts, ok := raw["conflicts"].(bson.A)
		require.True(t, ok, "a conflicting pull must report a conflicts array")
		assert.NotEmpty(t, conflicts)
	})

	// -------------------------------------------------------------------------
	// Scenario 9: dumboPull with noFF forces a merge commit (git pull --no-ff)
	// -------------------------------------------------------------------------
	t.Run("Scenario9_PullNoFFForcesMergeCommit", func(t *testing.T) {
		nfName := fmt.Sprintf("nfwork%d", suffix)
		require.NoError(t, admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: hubURL}, {Key: "as", Value: nfName},
		}).Decode(&res))

		// The hub advances; the fresh clone has no local commits, so a plain pull
		// would fast-forward straight to h5.
		_, err := hub.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(5)}, {Key: "v", Value: "five"}})
		require.NoError(t, err)
		h5 := dumboDBCommit(t, env, hubName, "c5", "alice <alice@acme.com>")
		pushHub()

		var res bson.M
		require.NoError(t, env.Client.Database(nfName+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboPull", Value: int32(1)}, {Key: "noFF", Value: true},
			{Key: "message", Value: "merge origin (no-ff)"}, {Key: "author", Value: "bob <bob@acme.com>"},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		// noFF records a merge commit even though a fast-forward was possible.
		assert.Equal(t, false, res["fastForward"], "noFF must not fast-forward")
		assert.Equal(t, false, res["alreadyUpToDate"])
		assert.NotEqual(t, res["commitBefore"], res["commitAfter"], "a merge commit was created")
		assert.NotEqual(t, h5, res["commitAfter"], "commitAfter is a merge commit, not the fetched commit (a plain pull would equal h5)")

		_, p2 := tipParents(t, nfName+"@main")
		assert.Equal(t, h5, p2, "noFF's merge commit has parent2 == the fetched commit")

		nf := env.Client.Database(nfName)
		n, err := nf.Collection("items").CountDocuments(ctx, bson.D{{Key: "_id", Value: int32(5)}})
		require.NoError(t, err)
		assert.EqualValues(t, 1, n, "c5 was merged in")
	})

	diverge := func(t *testing.T, prefix string, localID, hubID int32) string {
		t.Helper()
		name := fmt.Sprintf("%s%d", prefix, suffix)
		var res bson.M
		require.NoError(t, admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: hubURL}, {Key: "as", Value: name},
		}).Decode(&res))
		w := env.Client.Database(name)
		_, err := w.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: localID}, {Key: "v", Value: "local"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, name, "local change", "bob <bob@acme.com>")

		_, err = hub.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: hubID}, {Key: "v", Value: "hub"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, hubName, "hub change", "alice <alice@acme.com>")
		pushHub()
		return name
	}

	// -------------------------------------------------------------------------
	// Scenario 10: dumboPull {rebase:true} rebases instead of merging
	// -------------------------------------------------------------------------
	t.Run("Scenario10_PullRebaseExplicit", func(t *testing.T) {
		name := diverge(t, "rbwork", 300, 6)
		var res bson.M
		require.NoError(t, env.Client.Database(name+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboPull", Value: int32(1)}, {Key: "rebase", Value: true},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, true, res["rebased"], "rebase:true must rebase, not merge")
		assert.Equal(t, false, res["fastForward"])
		assert.Equal(t, false, res["alreadyUpToDate"])

		n, err := env.Client.Database(name).Collection("items").CountDocuments(ctx,
			bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: bson.A{int32(300), int32(6)}}}}})
		require.NoError(t, err)
		assert.EqualValues(t, 2, n)

		p1, p2 := tipParents(t, name+"@main")
		assert.NotEmpty(t, p1, "the replayed tip has a parent")
		assert.Empty(t, p2, "a rebase is linear -- no parent2")

		err = env.Client.Database(name+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboPull", Value: int32(1)}, {Key: "rebase", Value: "merges"},
		}).Err()
		assert.Error(t, err, "rebase must be a bool; \"merges\" is rejected")
	})

	// -------------------------------------------------------------------------
	// Scenario 11: a branch pull policy of rebase makes a bare pull rebase
	// -------------------------------------------------------------------------
	t.Run("Scenario11_PullPolicyRebase", func(t *testing.T) {
		name := diverge(t, "rbpolicy", 301, 7)
		var res bson.M
		require.NoError(t, env.Client.Database(name+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "main"},
			{Key: "setConfig", Value: bson.D{{Key: "pull", Value: bson.D{{Key: "rebase", Value: true}}}}},
		}).Decode(&res))
		cfg := res["config"].(bson.M)["pull"].(bson.M)
		require.Equal(t, "true", cfg["rebase"])

		require.NoError(t, env.Client.Database(name+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboPull", Value: int32(1)},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, true, res["rebased"], "a bare pull must honor the rebase policy")

		_, p2 := tipParents(t, name+"@main")
		assert.Empty(t, p2, "a policy rebase is linear -- no parent2")
	})

	// -------------------------------------------------------------------------
	// Scenario 12: a branch pull policy of ff:only rejects a non-FF bare pull,
	// and an explicit noFF overrides the policy
	// -------------------------------------------------------------------------
	t.Run("Scenario12_PullPolicyFFOnlyAndOverride", func(t *testing.T) {
		name := diverge(t, "ffpolicy", 302, 8)
		var res bson.M
		require.NoError(t, env.Client.Database(name+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "main"},
			{Key: "setConfig", Value: bson.D{{Key: "pull", Value: bson.D{{Key: "ff", Value: "only"}}}}},
		}).Decode(&res))

		raw := runCommandRaw(t, env.Client.Database(name+"@main"), bson.D{{Key: "dumboPull", Value: int32(1)}})
		require.EqualValues(t, 0, raw["ok"], "ff:only policy must reject a non-fast-forward pull")

		require.NoError(t, env.Client.Database(name+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboPull", Value: int32(1)}, {Key: "noFF", Value: true},
			{Key: "message", Value: "merge over policy"}, {Key: "author", Value: "bob <bob@acme.com>"},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"], "explicit noFF overrides the ff:only policy")
		assert.Equal(t, false, res["fastForward"])
		assert.NotEqual(t, true, res["rebased"], "a merge is not a rebase")

		_, p2 := tipParents(t, name+"@main")
		assert.NotEmpty(t, p2, "the override merge has a parent2")
	})

	// -------------------------------------------------------------------------
	// Scenario 13: a bare pull follows a differently-named upstream branch
	// -------------------------------------------------------------------------
	t.Run("Scenario13_PullRenamedUpstream", func(t *testing.T) {
		rnDir := t.TempDir()
		rnURL := "file://" + rnDir
		rnHub := fmt.Sprintf("rnhub%d", suffix)
		rnWork := fmt.Sprintf("rnwork%d", suffix)
		h := env.Client.Database(rnHub)
		var res bson.M

		_, err := h.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "one"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, rnHub, "c1", "alice <alice@acme.com>")
		require.NoError(t, h.RunCommand(ctx, bson.D{
			{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
			{Key: "name", Value: "origin"}, {Key: "url", Value: rnURL},
		}).Decode(&res))
		require.NoError(t, env.Client.Database(rnHub+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "trunk"},
		}).Decode(&res))
		require.NoError(t, env.Client.Database(rnHub+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Decode(&res))
		require.NoError(t, env.Client.Database(rnHub+"@trunk").RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "trunk"},
		}).Decode(&res))

		require.NoError(t, admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: rnURL}, {Key: "as", Value: rnWork},
		}).Decode(&res))
		require.NoError(t, env.Client.Database(rnWork+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "main"},
			{Key: "setConfig", Value: bson.D{{Key: "pull", Value: bson.D{
				{Key: "remote", Value: "origin"}, {Key: "branch", Value: "trunk"},
			}}}},
		}).Decode(&res))

		_, err = env.Client.Database(rnHub+"@trunk").Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "two"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, rnHub+"@trunk", "c2 on trunk", "alice <alice@acme.com>")
		require.NoError(t, env.Client.Database(rnHub+"@trunk").RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "trunk"},
		}).Decode(&res))

		require.NoError(t, env.Client.Database(rnWork+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboPull", Value: int32(1)},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"], "bare pull of a renamed upstream must succeed")
		assert.Equal(t, "origin", res["remote"])
		assert.Equal(t, true, res["fastForward"], "local main is an ancestor of origin/trunk")

		n, err := env.Client.Database(rnWork+"@main").Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 2, n, "bare pull must merge origin/trunk into the renamed local main")
	})

	// -------------------------------------------------------------------------
	// Scenario 14: noFF + ffOnly is rejected before any fetch side effect
	// -------------------------------------------------------------------------
	t.Run("Scenario14_PullConflictingFFOptionsRejected", func(t *testing.T) {
		mxName := fmt.Sprintf("mxwork%d", suffix)
		require.NoError(t, admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: hubURL}, {Key: "as", Value: mxName},
		}).Decode(&res))
		trackingBefore := branchEntry(t, env, mxName, "origin/main")["commitId"]

		_, err := hub.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(900)}, {Key: "v", Value: "mx"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, hubName, "mx advance", "alice <alice@acme.com>")
		pushHub()

		raw := runCommandRaw(t, env.Client.Database(mxName+"@main"), bson.D{
			{Key: "dumboPull", Value: int32(1)}, {Key: "noFF", Value: true}, {Key: "ffOnly", Value: true},
		})
		require.EqualValues(t, 0, raw["ok"], "noFF + ffOnly must be rejected")
		assert.Contains(t, raw["errmsg"], "mutually exclusive")

		trackingAfter := branchEntry(t, env, mxName, "origin/main")["commitId"]
		assert.Equal(t, trackingBefore, trackingAfter, "a rejected pull must not update remote-tracking refs")
	})
}
