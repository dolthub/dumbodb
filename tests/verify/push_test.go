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

// TestPushVerify is the automated analog of docs/verify/push.md.
//
// Each top-level subtest corresponds to one scenario in that document and they
// run sequentially, sharing one database and remotes so each scenario's side
// effects carry into the next. Remotes are local file:// directories, which
// behave like every other transport for push.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// branchEntry returns a branch's entry from a dumboBranch listing taken on that
// branch's connection.
func branchEntry(t *testing.T, env *dumboDBTestEnv, dbName, branch string) bson.M {
	t.Helper()
	var res bson.M
	require.NoError(t, env.Client.Database(dbName+"@"+branch).RunCommand(
		context.Background(), bson.D{{Key: "dumboBranch", Value: int32(1)}},
	).Decode(&res))
	arr, ok := res["branches"].(bson.A)
	require.True(t, ok, "branches must be an array")
	for _, raw := range arr {
		e, ok := raw.(bson.M)
		require.True(t, ok)
		if e["name"] == branch {
			return e
		}
	}
	t.Fatalf("branch %q not found in listing", branch)
	return nil
}

// pullEntry / pushEntry return a listing entry's config.pull / config.push
// sub-object, or nil when unset.
func pullEntry(e bson.M) bson.M {
	cfg, _ := e["config"].(bson.M)
	if cfg == nil {
		return nil
	}
	p, _ := cfg["pull"].(bson.M)
	return p
}

func pushEntry(e bson.M) bson.M {
	cfg, _ := e["config"].(bson.M)
	if cfg == nil {
		return nil
	}
	p, _ := cfg["push"].(bson.M)
	return p
}

func TestPushVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	dbName := fmt.Sprintf("pushvrfy%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)

	originURL := "file://" + t.TempDir()
	origin2URL := "file://" + t.TempDir()

	// Setup: one commit on main, plus two remotes.
	require.NoError(t, db.Drop(ctx))
	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "label", Value: "alpha"}})
	require.NoError(t, err)
	hash1 := dumboDBCommit(t, env, dbName, "commit one", "alice <alice@acme.com>")

	addRemote := func(name, url string) {
		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
			{Key: "name", Value: name}, {Key: "url", Value: url},
		}).Decode(&res))
		require.EqualValues(t, 1, res["ok"])
	}
	addRemote("origin", originURL)
	addRemote("origin2", origin2URL)

	setConfig := func(t *testing.T, branch string, cfg bson.D) {
		t.Helper()
		var res bson.M
		require.NoError(t, env.Client.Database(dbName+"@"+branch).RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: branch},
			{Key: "setConfig", Value: cfg},
		}).Decode(&res))
		require.EqualValues(t, 1, res["ok"])
	}

	// -------------------------------------------------------------------------
	// Scenario 1: A named push (git push origin main) records no config
	// -------------------------------------------------------------------------
	t.Run("Scenario1_NamedPushRecordsNoConfig", func(t *testing.T) {
		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "origin", res["remote"])
		assert.Equal(t, "main", res["branch"])
		assert.Equal(t, hash1, res["commitPushed"])
		_, hasBefore := res["commitBefore"]
		assert.False(t, hasBefore, "a push that creates the remote branch has no commitBefore")
		assert.Equal(t, false, res["upToDate"])

		_, hasConfig := branchEntry(t, env, dbName, "main")["config"]
		assert.False(t, hasConfig, "a named push must not record config")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: A bare push with no remote and no config is an error
	// -------------------------------------------------------------------------
	t.Run("Scenario2_BarePushNoConfigErrors", func(t *testing.T) {
		var res bson.M
		err := db.RunCommand(ctx, bson.D{{Key: "dumboPush", Value: int32(1)}}).Decode(&res)
		assert.Error(t, err, "a bare push with no remote and no config must error")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: setConfig records config.pull (the fetch/push upstream)
	// -------------------------------------------------------------------------
	t.Run("Scenario3_SetConfigPull", func(t *testing.T) {
		setConfig(t, "main", bson.D{{Key: "pull", Value: bson.D{
			{Key: "remote", Value: "origin"}, {Key: "branch", Value: "main"},
		}}})
		pull := pullEntry(branchEntry(t, env, dbName, "main"))
		require.NotNil(t, pull, "config.pull must be present after setConfig")
		assert.Equal(t, "origin", pull["remote"])
		assert.Equal(t, "main", pull["branch"])
	})

	// -------------------------------------------------------------------------
	// Scenario 4: A bare push follows config.pull (git push)
	// -------------------------------------------------------------------------
	t.Run("Scenario4_BarePushFollowsConfigPull", func(t *testing.T) {
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "label", Value: "beta"}})
		require.NoError(t, err)
		hash2 := dumboDBCommit(t, env, dbName, "commit two", "alice <alice@acme.com>")

		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "dumboPush", Value: int32(1)}}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "origin", res["remote"], "bare push resolves config.pull.remote")
		assert.Equal(t, false, res["upToDate"])
		// The remote advances hash1 -> hash2; the report shows both.
		assert.Equal(t, hash1, res["commitBefore"])
		assert.Equal(t, hash2, res["commitPushed"])
	})

	// -------------------------------------------------------------------------
	// Scenario 5: An explicit push does not change the stored config
	// -------------------------------------------------------------------------
	t.Run("Scenario5_ExplicitPushDoesNotChangeConfig", func(t *testing.T) {
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "label", Value: "gamma"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "commit three", "alice <alice@acme.com>")

		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Decode(&res))
		assert.Equal(t, "origin", res["remote"])

		e := branchEntry(t, env, dbName, "main")
		pull := pullEntry(e)
		require.NotNil(t, pull)
		assert.Equal(t, "origin", pull["remote"], "an explicit push must leave config.pull unchanged")
		assert.Nil(t, pushEntry(e), "an explicit push must not create config.push")
	})

	// -------------------------------------------------------------------------
	// Scenario 6: Re-push is idempotent (up to date)
	// -------------------------------------------------------------------------
	t.Run("Scenario6_IdempotentRepush", func(t *testing.T) {
		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "dumboPush", Value: int32(1)}}).Decode(&res))
		assert.Equal(t, "origin", res["remote"])
		assert.Equal(t, true, res["upToDate"], "the branch head is already on origin/main")
	})

	// -------------------------------------------------------------------------
	// Scenario 7: Fast-forward-only by default; force overwrites
	// -------------------------------------------------------------------------
	t.Run("Scenario7_FastForwardAndForce", func(t *testing.T) {
		ffURL := "file://" + t.TempDir()
		srcName := fmt.Sprintf("pushffsrc%d", rand.Int64N(1_000_000))
		cloneName := fmt.Sprintf("pushffclone%d", rand.Int64N(1_000_000))
		src := env.Client.Database(srcName)
		var res bson.M

		_, err := src.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "who", Value: "base"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, srcName, "c1", "a <a@a>")
		require.NoError(t, src.RunCommand(ctx, bson.D{
			{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
			{Key: "name", Value: "origin"}, {Key: "url", Value: ffURL},
		}).Decode(&res))
		require.NoError(t, src.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Decode(&res))

		require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: ffURL}, {Key: "as", Value: cloneName},
		}).Decode(&res))

		_, err = src.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "who", Value: "source"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, srcName, "c2-source", "a <a@a>")
		require.NoError(t, src.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Decode(&res))

		clone := env.Client.Database(cloneName)
		_, err = clone.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "who", Value: "clone"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, cloneName, "c2-clone", "b <b@b>")

		err = clone.RunCommand(ctx, bson.D{{Key: "dumboPush", Value: int32(1)}}).Decode(&res)
		assert.Error(t, err, "a non-fast-forward push (diverged from the shared base) must be rejected")

		require.NoError(t, clone.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "force", Value: true},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
	})

	// -------------------------------------------------------------------------
	// Scenario 8: A new branch is pushed and then tracked via setConfig
	// -------------------------------------------------------------------------
	t.Run("Scenario8_NewBranchPushedAndTracked", func(t *testing.T) {
		var res bson.M
		require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "release"},
		}).Decode(&res))

		require.NoError(t, env.Client.Database(dbName+"@release").RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "release"},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "release", res["branch"])

		setConfig(t, "release", bson.D{{Key: "pull", Value: bson.D{
			{Key: "remote", Value: "origin"}, {Key: "branch", Value: "release"},
		}}})
		pull := pullEntry(branchEntry(t, env, dbName, "release"))
		require.NotNil(t, pull)
		assert.Equal(t, "origin", pull["remote"])
		assert.Equal(t, "release", pull["branch"])
	})

	// -------------------------------------------------------------------------
	// Scenario 9: Push to a differently-named remote branch via a refspec
	// -------------------------------------------------------------------------
	t.Run("Scenario9_RefspecPushToDifferentRemoteBranch", func(t *testing.T) {
		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main:published"},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "main", res["branch"])
		assert.Equal(t, "published", res["remoteBranch"])

		pull := pullEntry(branchEntry(t, env, dbName, "main"))
		require.NotNil(t, pull)
		assert.Equal(t, "main", pull["branch"], "an explicit refspec must not change config.pull")
	})

	// -------------------------------------------------------------------------
	// Scenario 10: An explicit remote with no matching config pushes same-named
	// -------------------------------------------------------------------------
	t.Run("Scenario10_ExplicitRemoteSameNamed", func(t *testing.T) {
		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin2"},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "origin2", res["remote"])
		assert.Equal(t, "main", res["branch"])

		pull := pullEntry(branchEntry(t, env, dbName, "main"))
		require.NotNil(t, pull)
		assert.Equal(t, "origin", pull["remote"], "a push to another remote must not change config.pull")
		assert.Equal(t, "main", pull["branch"])
	})

	// -------------------------------------------------------------------------
	// Scenario 11: HEAD:<dst> pushes the current head to a differently-named branch
	// -------------------------------------------------------------------------
	t.Run("Scenario11_HeadToNamedBranch", func(t *testing.T) {
		head := branchEntry(t, env, dbName, "main")["commitId"]
		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "HEAD:handy"},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "origin", res["remote"])
		assert.Equal(t, "main", res["branch"], "HEAD resolves to the connection branch")
		assert.Equal(t, "handy", res["remoteBranch"])
		assert.Equal(t, head, res["commitPushed"])
	})

	// -------------------------------------------------------------------------
	// Scenario 12: A revision source (HEAD~1) pushes an older commit to a branch
	// -------------------------------------------------------------------------
	t.Run("Scenario12_RevisionSourceToBranch", func(t *testing.T) {
		head := branchEntry(t, env, dbName, "main")["commitId"]
		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "HEAD~1:older"},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "older", res["remoteBranch"])
		// The source is a revision, not a branch, so no "branch" is reported...
		_, hasBranch := res["branch"]
		assert.False(t, hasBranch, "a revision source names no local branch")
		// ...and the commit pushed is the parent, not the current head.
		assert.NotEqual(t, head, res["commitPushed"])
		assert.NotEmpty(t, res["commitPushed"])
	})

	// -------------------------------------------------------------------------
	// Scenario 13: A colon-less revision has no destination branch (error)
	// -------------------------------------------------------------------------
	t.Run("Scenario13_ColonlessRevisionErrors", func(t *testing.T) {
		var res bson.M
		err := db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "HEAD~1"},
		}).Decode(&res)
		assert.Error(t, err, "a colon-less revision names no branch to push to")
	})

	// -------------------------------------------------------------------------
	// Scenario 14: config.push is a persistent, differently-named push target
	// -------------------------------------------------------------------------
	t.Run("Scenario14_ConfigPushPersistentTarget", func(t *testing.T) {
		setConfig(t, "main", bson.D{{Key: "push", Value: bson.D{
			{Key: "remote", Value: "origin2"}, {Key: "branch", Value: "rev51"},
		}}})

		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(4)}, {Key: "label", Value: "delta"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "commit four", "alice <alice@acme.com>")

		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "dumboPush", Value: int32(1)}}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "origin2", res["remote"], "a bare push follows config.push")
		assert.Equal(t, "rev51", res["remoteBranch"])

		e := branchEntry(t, env, dbName, "main")
		pull := pullEntry(e)
		require.NotNil(t, pull)
		assert.Equal(t, "origin", pull["remote"])
		assert.Equal(t, "main", pull["branch"])
		push := pushEntry(e)
		require.NotNil(t, push)
		assert.Equal(t, "origin2", push["remote"])
		assert.Equal(t, "rev51", push["branch"])
	})

	// -------------------------------------------------------------------------
	// Scenario 15: A leading '+' forces a non-fast-forward push
	// -------------------------------------------------------------------------
	t.Run("Scenario15_PlusPrefixForces", func(t *testing.T) {
		ffURL := "file://" + t.TempDir()
		dbA := fmt.Sprintf("pushplusA%d", rand.Int64N(1_000_000))
		dbB := fmt.Sprintf("pushplusB%d", rand.Int64N(1_000_000))
		seed := func(name, who string) {
			_, err := env.Client.Database(name).Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "who", Value: who}})
			require.NoError(t, err)
			dumboDBCommit(t, env, name, who+"1", "x <x@x>")
			var res bson.M
			require.NoError(t, env.Client.Database(name).RunCommand(ctx, bson.D{
				{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
				{Key: "name", Value: "origin"}, {Key: "url", Value: ffURL},
			}).Decode(&res))
		}
		seed(dbA, "A")
		var res bson.M
		require.NoError(t, env.Client.Database(dbA).RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Decode(&res))

		seed(dbB, "B")
		// Plain push of an unrelated history is rejected (not a fast-forward).
		err := env.Client.Database(dbB).RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Decode(&res)
		assert.Error(t, err, "a non-fast-forward push must be rejected")

		// The '+' prefix forces it, exactly like force:true.
		require.NoError(t, env.Client.Database(dbB).RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "+main"},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
	})
}
