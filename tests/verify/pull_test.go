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
}
