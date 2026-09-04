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

// TestCloneVerify is the automated analog of docs/verify/clone.md.
//
// Each top-level subtest corresponds to one scenario in that document. A shared
// setup publishes a source remote with two branches; the scenarios clone it and
// check the git-clone-parity behavior (origin remote + default-branch upstream).

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestCloneVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	srcName := fmt.Sprintf("srccl%d", rand.Int64N(1_000_000))
	cloneName := fmt.Sprintf("clonecl%d", rand.Int64N(1_000_000))
	srcDir := t.TempDir()
	srcURL := "file://" + srcDir
	admin := env.Client.Database("admin")

	// Setup: publish a source remote with main and feature (both at hash1).
	src := env.Client.Database(srcName)
	require.NoError(t, src.Drop(ctx))
	_, err := src.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "label", Value: "alpha"}})
	require.NoError(t, err)
	hash1 := dumboDBCommit(t, env, srcName, "commit one", "alice <alice@acme.com>")

	var res bson.M
	require.NoError(t, src.RunCommand(ctx, bson.D{
		{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
		{Key: "name", Value: "origin"}, {Key: "url", Value: srcURL},
	}).Decode(&res))
	require.NoError(t, src.RunCommand(ctx, bson.D{
		{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
	}).Decode(&res))
	require.NoError(t, env.Client.Database(srcName+"@main").RunCommand(ctx, bson.D{
		{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"},
	}).Decode(&res))
	require.NoError(t, env.Client.Database(srcName+"@feature").RunCommand(ctx, bson.D{
		{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "feature"},
	}).Decode(&res))

	// -------------------------------------------------------------------------
	// Scenario 1: Clone a remote into a new database
	// -------------------------------------------------------------------------
	t.Run("Scenario1_CloneIntoNewDatabase", func(t *testing.T) {
		var res bson.M
		require.NoError(t, admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: srcURL}, {Key: "as", Value: cloneName},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, cloneName, res["db"])
		assert.Equal(t, srcURL, res["from"])

		var doc bson.M
		require.NoError(t, env.Client.Database(cloneName).Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&doc))
		assert.Equal(t, "alpha", doc["label"], "cloned data must be readable")
	})

	// -------------------------------------------------------------------------
	// Scenario 2: Clone brings every branch
	// -------------------------------------------------------------------------
	t.Run("Scenario2_CloneBringsEveryBranch", func(t *testing.T) {
		var res bson.M
		require.NoError(t, env.Client.Database(cloneName+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "list"},
		}).Decode(&res))
		got := map[string]bson.M{}
		for _, raw := range res["branches"].(bson.A) {
			e := raw.(bson.M)
			got[e["name"].(string)] = e
		}
		require.Contains(t, got, "main")
		require.Contains(t, got, "feature")
		assert.Equal(t, hash1, got["feature"]["commitId"])
		_, featureTracks := got["feature"]["config"]
		assert.False(t, featureTracks, "only the default branch is set to track")
	})

	// -------------------------------------------------------------------------
	// Scenario 3: Clone registers an origin remote (git clone parity)
	// -------------------------------------------------------------------------
	t.Run("Scenario3_CloneRegistersOrigin", func(t *testing.T) {
		var res bson.M
		require.NoError(t, env.Client.Database(cloneName).RunCommand(ctx, bson.D{
			{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "list"},
		}).Decode(&res))
		remotes := res["remotes"].(bson.A)
		require.Len(t, remotes, 1)
		origin := remotes[0].(bson.M)
		assert.Equal(t, "origin", origin["name"])
		assert.Equal(t, srcURL, origin["url"])
	})

	// -------------------------------------------------------------------------
	// Scenario 4: The default branch tracks origin, so a bare push works
	// -------------------------------------------------------------------------
	t.Run("Scenario4_DefaultBranchTracksOrigin", func(t *testing.T) {
		pull, ok := branchEntry(t, env, cloneName, "main")["config"].(bson.M)["pull"].(bson.M)
		require.True(t, ok, "cloned main must track a config.pull upstream")
		assert.Equal(t, "origin", pull["remote"])
		assert.Equal(t, "main", pull["branch"])

		c := env.Client.Database(cloneName)
		_, err := c.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "label", Value: "beta"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, cloneName, "local change", "bob <bob@acme.com>")

		var res bson.M
		require.NoError(t, c.RunCommand(ctx, bson.D{{Key: "dumboPush", Value: int32(1)}}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])
		assert.Equal(t, "origin", res["remote"], "a bare push on the clone follows the tracked upstream")
	})

	// -------------------------------------------------------------------------
	// Scenario 5: Cloning into an existing database is rejected
	// -------------------------------------------------------------------------
	t.Run("Scenario5_CloneIntoExistingRejected", func(t *testing.T) {
		var res bson.M
		err := admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: srcURL}, {Key: "as", Value: cloneName},
		}).Decode(&res)
		assert.Error(t, err, "cloning into an existing database must fail")
	})

	// -------------------------------------------------------------------------
	// Scenario 6: A reserved database name is rejected
	// -------------------------------------------------------------------------
	t.Run("Scenario6_ReservedNameRejected", func(t *testing.T) {
		var res bson.M
		err := admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: srcURL}, {Key: "as", Value: "admin"},
		}).Decode(&res)
		assert.Error(t, err, "cloning into a reserved name must fail")
	})

	// -------------------------------------------------------------------------
	// Scenario 7: An unsupported remote scheme is rejected
	// -------------------------------------------------------------------------
	t.Run("Scenario7_UnsupportedSchemeRejected", func(t *testing.T) {
		var res bson.M
		err := admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: "ssh://host/org/repo"}, {Key: "as", Value: "nope"},
		}).Decode(&res)
		assert.Error(t, err, "cloning an unsupported scheme must fail")
	})

	// -------------------------------------------------------------------------
	// Scenario 8: A remote with no main -- rejected, or mapped via trackAsMain
	// -------------------------------------------------------------------------
	t.Run("Scenario8_CloneWithoutMain_TrackAsMain", func(t *testing.T) {
		noMainURL := "file://" + t.TempDir()
		srcNM := fmt.Sprintf("srcnomain%d", rand.Int64N(1_000_000))
		s := env.Client.Database(srcNM)
		_, err := s.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "rel"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, srcNM, "c1", "a <a@a>")
		require.NoError(t, s.RunCommand(ctx, bson.D{
			{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
			{Key: "name", Value: "origin"}, {Key: "url", Value: noMainURL},
		}).Err())
		// Push main -> release, so the remote holds only "release" (no main).
		require.NoError(t, s.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main:release"},
		}).Err())

		// Without trackAsMain, cloning a main-less remote is rejected.
		var res bson.M
		err = admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: noMainURL}, {Key: "as", Value: "nomainclone"},
		}).Decode(&res)
		require.Error(t, err, "cloning a remote with no main must be rejected")

		// trackAsMain maps the remote's release branch onto local main.
		mapped := fmt.Sprintf("mapped%d", rand.Int64N(1_000_000))
		require.NoError(t, admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: noMainURL},
			{Key: "as", Value: mapped}, {Key: "trackAsMain", Value: "release"},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])

		// main holds release's content and tracks origin/release; there is no
		// separate local "release" branch (main only).
		n, err := env.Client.Database(mapped).Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 1, n, "main adopted release's content")

		names := map[string]bson.M{}
		var list bson.M
		require.NoError(t, env.Client.Database(mapped+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "list"},
		}).Decode(&list))
		for _, e := range list["branches"].(bson.A) {
			m := e.(bson.M)
			names[m["name"].(string)] = m
		}
		require.Contains(t, names, "main")
		assert.NotContains(t, names, "release", "the mapped branch has no separate local copy (main only)")
		assert.Contains(t, names, "origin/release", "the remote branch survives as a tracking ref")
		pull := names["main"]["config"].(bson.M)["pull"].(bson.M)
		assert.Equal(t, "origin", pull["remote"])
		assert.Equal(t, "release", pull["branch"], "main tracks origin/release")

		// A missing trackAsMain branch is rejected.
		err = admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: noMainURL},
			{Key: "as", Value: "nope"}, {Key: "trackAsMain", Value: "ghost"},
		}).Decode(&res)
		require.Error(t, err, "trackAsMain naming a nonexistent remote branch must be rejected")
	})

	// -------------------------------------------------------------------------
	// Scenario 9: trackAsMain overrides the remote's own main
	// -------------------------------------------------------------------------
	t.Run("Scenario9_TrackAsMainOverride", func(t *testing.T) {
		ovURL := "file://" + t.TempDir()
		ov := fmt.Sprintf("srcov%d", rand.Int64N(1_000_000))
		o := env.Client.Database(ov)
		_, err := o.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: "onmain"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, ov, "c1", "a <a@a>")
		require.NoError(t, o.RunCommand(ctx, bson.D{
			{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
			{Key: "name", Value: "origin"}, {Key: "url", Value: ovURL},
		}).Err())
		require.NoError(t, o.RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
		}).Err())
		// A "feature" branch with an extra commit, pushed too.
		require.NoError(t, env.Client.Database(ov+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"},
		}).Err())
		_, err = env.Client.Database(ov+"@feature").Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "v", Value: "onfeature"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, ov+"@feature", "c2", "a <a@a>")
		require.NoError(t, env.Client.Database(ov+"@feature").RunCommand(ctx, bson.D{
			{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "feature"},
		}).Err())

		// Clone with trackAsMain: feature overrides the remote's own main.
		var res bson.M
		ovClone := fmt.Sprintf("ovclone%d", rand.Int64N(1_000_000))
		require.NoError(t, admin.RunCommand(ctx, bson.D{
			{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: ovURL},
			{Key: "as", Value: ovClone}, {Key: "trackAsMain", Value: "feature"},
		}).Decode(&res))
		assert.EqualValues(t, 1, res["ok"])

		// main holds feature's content (both docs), tracking origin/feature.
		n, err := env.Client.Database(ovClone).Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 2, n, "main adopted feature's content, not remote main's")

		names := map[string]bson.M{}
		var list bson.M
		require.NoError(t, env.Client.Database(ovClone+"@main").RunCommand(ctx, bson.D{
			{Key: "dumboBranch", Value: int32(1)}, {Key: "action", Value: "list"},
		}).Decode(&list))
		for _, e := range list["branches"].(bson.A) {
			m := e.(bson.M)
			names[m["name"].(string)] = m
		}
		require.Contains(t, names, "main")
		assert.NotContains(t, names, "feature", "the mapped branch has no separate local copy")
		assert.Contains(t, names, "origin/main", "the overridden remote main survives as a tracking ref")
		assert.Contains(t, names, "origin/feature")
		pull := names["main"]["config"].(bson.M)["pull"].(bson.M)
		assert.Equal(t, "feature", pull["branch"], "main tracks origin/feature, not origin/main")
	})
}
