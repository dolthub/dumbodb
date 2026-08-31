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

func cloneBranchList(t *testing.T, arr bson.A) []string {
	t.Helper()
	out := make([]string, 0, len(arr))
	for _, raw := range arr {
		out = append(out, fmt.Sprint(raw))
	}
	return out
}

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
		{Key: "dumboBranch", Value: int32(1)}, {Key: "branch", Value: "feature"},
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
		assert.Equal(t, "main", res["defaultBranch"])
		assert.Equal(t, hash1, res["commit"])
		names := cloneBranchList(t, res["branches"].(bson.A))
		assert.Contains(t, names, "main")
		assert.Contains(t, names, "feature")

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
			{Key: "dumboBranch", Value: int32(1)},
		}).Decode(&res))
		got := map[string]bson.M{}
		for _, raw := range res["branches"].(bson.A) {
			e := raw.(bson.M)
			got[e["name"].(string)] = e
		}
		require.Contains(t, got, "main")
		require.Contains(t, got, "feature")
		assert.Equal(t, hash1, got["feature"]["commitId"])
		_, featureTracks := got["feature"]["upstream"]
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
		up, ok := branchEntry(t, env, cloneName, "main")["upstream"].(bson.M)
		require.True(t, ok, "cloned main must track an upstream")
		assert.Equal(t, "origin", up["remote"])
		assert.Equal(t, "main", up["ref"])

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
}
