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

	// -------------------------------------------------------------------------
	// Scenario 1: Push a named branch (git push origin main) -- no upstream set
	// -------------------------------------------------------------------------
	t.Run("Scenario1_NamedPushSetsNoUpstream", func(t *testing.T) {
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

		_, hasUpstream := branchEntry(t, env, dbName, "main")["upstream"]
		assert.False(t, hasUpstream, "a named push must not set an upstream")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: Bare push with no upstream is an error (git push origin)
	// -------------------------------------------------------------------------
	t.Run("Scenario2_BarePushNoUpstreamErrors", func(t *testing.T) {
		var res bson.M
		err := db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"},
		}).Decode(&res)
		assert.Error(t, err, "bare push with no upstream must error")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: setUpstream records the upstream (git push -u origin main)
	// -------------------------------------------------------------------------
	t.Run("Scenario3_SetUpstreamRecords", func(t *testing.T) {
		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"},
			{Key: "refSpec", Value: "main"}, {Key: "setUpstream", Value: true},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, true, res["upToDate"], "hash1 was already pushed in Scenario 1")

		up, ok := branchEntry(t, env, dbName, "main")["upstream"].(bson.M)
		require.True(t, ok, "upstream must be present after -u")
		assert.Equal(t, "origin", up["remote"])
		assert.Equal(t, "main", up["ref"])
	})

	// -------------------------------------------------------------------------
	// Scenario 4: Bare push follows the upstream (git push)
	// -------------------------------------------------------------------------
	t.Run("Scenario4_BarePushFollowsUpstream", func(t *testing.T) {
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "label", Value: "beta"}})
		require.NoError(t, err)
		hash2 := dumboDBCommit(t, env, dbName, "commit two", "alice <alice@acme.com>")

		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "dumboPush", Value: int32(1)}}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "origin", res["remote"], "bare push resolves the upstream remote")
		assert.Equal(t, false, res["upToDate"])
		// The remote advances hash1 -> hash2; the report shows both.
		assert.Equal(t, hash1, res["commitBefore"])
		assert.Equal(t, hash2, res["commitPushed"])
	})

	// -------------------------------------------------------------------------
	// Scenario 5: setUpstream to a second remote switches the upstream
	// -------------------------------------------------------------------------
	t.Run("Scenario5_SetUpstreamSwitchesRemote", func(t *testing.T) {
		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin2"},
			{Key: "refSpec", Value: "main"}, {Key: "setUpstream", Value: true},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "origin2", res["remote"])

		up := branchEntry(t, env, dbName, "main")["upstream"].(bson.M)
		assert.Equal(t, "origin2", up["remote"], "-u overwrites the upstream")
	})

	// -------------------------------------------------------------------------
	// Scenario 6: An explicit push does not change the upstream
	// -------------------------------------------------------------------------
	t.Run("Scenario6_ExplicitPushDoesNotChangeUpstream", func(t *testing.T) {
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(3)}, {Key: "label", Value: "gamma"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "commit three", "alice <alice@acme.com>")

		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Decode(&res))
		assert.Equal(t, "origin", res["remote"])

		up := branchEntry(t, env, dbName, "main")["upstream"].(bson.M)
		assert.Equal(t, "origin2", up["remote"], "an explicit push must leave the upstream unchanged")
	})

	// -------------------------------------------------------------------------
	// Scenario 7: Re-push is idempotent
	// -------------------------------------------------------------------------
	t.Run("Scenario7_IdempotentRepush", func(t *testing.T) {
		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Decode(&res))
		assert.Equal(t, "origin", res["remote"])
		assert.Equal(t, true, res["upToDate"])
	})

	// -------------------------------------------------------------------------
	// Scenario 8: Fast-forward-only by default; force overwrites
	// -------------------------------------------------------------------------
	t.Run("Scenario8_FastForwardAndForce", func(t *testing.T) {
		ffURL := "file://" + t.TempDir()
		dbA := fmt.Sprintf("pushffA%d", rand.Int64N(1_000_000))
		dbB := fmt.Sprintf("pushffB%d", rand.Int64N(1_000_000))

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
		// Non-fast-forward push is rejected.
		err := env.Client.Database(dbB).RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Decode(&res)
		assert.Error(t, err, "a non-fast-forward push must be rejected")

		// force overwrites the remote.
		require.NoError(t, env.Client.Database(dbB).RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"},
			{Key: "refSpec", Value: "main"}, {Key: "force", Value: true},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
	})

	// -------------------------------------------------------------------------
	// Scenario 9: A new branch at an existing commit is pushed and tracked
	// -------------------------------------------------------------------------
	t.Run("Scenario9_NewBranchAtExistingCommit", func(t *testing.T) {
		var res bson.M
		require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "release"},
		}).Decode(&res))

		require.NoError(t, env.Client.Database(dbName+"@release").RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"},
			{Key: "refSpec", Value: "release"}, {Key: "setUpstream", Value: true},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "release", res["branch"])

		up := branchEntry(t, env, dbName, "release")["upstream"].(bson.M)
		assert.Equal(t, "origin", up["remote"])
		assert.Equal(t, "release", up["ref"])
	})

	// -------------------------------------------------------------------------
	// Scenario 10: Push to a differently-named remote branch (refspec)
	// -------------------------------------------------------------------------
	t.Run("Scenario10_RefspecPushToDifferentRemoteBranch", func(t *testing.T) {
		var res bson.M
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"},
			{Key: "refSpec", Value: "main:published"}, {Key: "setUpstream", Value: true},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "main", res["branch"])
		assert.Equal(t, "published", res["remoteBranch"])

		// main now tracks origin/published -- the upstream ref differs from the name.
		up := branchEntry(t, env, dbName, "main")["upstream"].(bson.M)
		assert.Equal(t, "origin", up["remote"])
		assert.Equal(t, "published", up["ref"])
	})

	// -------------------------------------------------------------------------
	// Scenario 11: Bare push to a different remote is triangular (git push origin2)
	// -------------------------------------------------------------------------
	t.Run("Scenario11_TriangularPushToUntrackedRemote", func(t *testing.T) {
		var res bson.M
		// Put main's upstream in a known place (origin/main) first.
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"},
			{Key: "refSpec", Value: "main"}, {Key: "setUpstream", Value: true},
		}).Decode(&res))
		up := branchEntry(t, env, dbName, "main")["upstream"].(bson.M)
		require.Equal(t, "origin", up["remote"])
		require.Equal(t, "main", up["ref"])

		// A bare push to origin2 -- which main does NOT track -- is triangular: it
		// sends main to origin2/main and needs no upstream, unlike Scenario 2's push
		// to the branch's own remote.
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin2"},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "origin2", res["remote"])
		assert.Equal(t, "main", res["branch"])

		// The triangular push never touches the upstream -- still origin/main.
		up = branchEntry(t, env, dbName, "main")["upstream"].(bson.M)
		assert.Equal(t, "origin", up["remote"], "a triangular push must not change the upstream")
		assert.Equal(t, "main", up["ref"])
	})

	// -------------------------------------------------------------------------
	// Scenario 12: HEAD:<dst> pushes the current head to a differently-named branch
	// -------------------------------------------------------------------------
	t.Run("Scenario12_HeadToNamedBranch", func(t *testing.T) {
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

		// No setUpstream, so main still tracks origin/main from Scenario 11.
		up := branchEntry(t, env, dbName, "main")["upstream"].(bson.M)
		assert.Equal(t, "main", up["ref"], "an explicit refspec must not change the upstream")
	})

	// -------------------------------------------------------------------------
	// Scenario 13: A revision source (HEAD~1) pushes an older commit to a branch
	// -------------------------------------------------------------------------
	t.Run("Scenario13_RevisionSourceToBranch", func(t *testing.T) {
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
	// Scenario 14: A colon-less revision has no destination branch (error)
	// -------------------------------------------------------------------------
	t.Run("Scenario14_ColonlessRevisionErrors", func(t *testing.T) {
		var res bson.M
		err := db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "HEAD~1"},
		}).Decode(&res)
		assert.Error(t, err, "a colon-less revision names no branch to push to")
	})

	// -------------------------------------------------------------------------
	// Scenario 15: A bare push errors when the upstream name differs (git simple)
	// -------------------------------------------------------------------------
	t.Run("Scenario15_BarePushNameMismatchErrors", func(t *testing.T) {
		var res bson.M
		// Track a differently-named remote branch via a refspec + setUpstream.
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"},
			{Key: "refSpec", Value: "main:renamed"}, {Key: "setUpstream", Value: true},
		}).Decode(&res))
		up := branchEntry(t, env, dbName, "main")["upstream"].(bson.M)
		require.Equal(t, "renamed", up["ref"])

		// A bare push now refuses: main's name does not match its upstream ref.
		err := db.RunCommand(ctx, bson.D{{Key: "dumboPush", Value: int32(1)}}).Decode(&res)
		assert.Error(t, err, "git simple refuses a bare push to a name-mismatched upstream")
	})

	// -------------------------------------------------------------------------
	// Scenario 16: A leading '+' forces a non-fast-forward push
	// -------------------------------------------------------------------------
	t.Run("Scenario16_PlusPrefixForces", func(t *testing.T) {
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
