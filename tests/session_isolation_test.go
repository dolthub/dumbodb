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
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func siClient(t *testing.T, env *dumboDBTestEnv) *mongo.Client {
	t.Helper()
	uri := fmt.Sprintf("mongodb://127.0.0.1:%d", env.Port)
	c, err := mongo.Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })
	return c
}

func mongoCommandCode(err error) int32 {
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) {
		return cmdErr.Code
	}
	var writeExc mongo.WriteException
	if errors.As(err, &writeExc) {
		if writeExc.WriteConcernError != nil {
			return int32(writeExc.WriteConcernError.Code)
		}
		for _, we := range writeExc.WriteErrors {
			return int32(we.Code)
		}
	}
	return 0
}

// D13: startTransaction is rejected with code 263 and the redirect
// message the design specifies. The wire-protocol field, not a separate
// command, is what gets rejected so any first transactional write
// triggers it.
func TestSessionIsolation_StartTransactionRejected(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()
	coll := env.Collection(t)

	sess, err := env.Client.StartSession()
	require.NoError(t, err)
	defer sess.EndSession(ctx)
	require.NoError(t, sess.StartTransaction())

	_, err = coll.InsertOne(mongo.NewSessionContext(ctx, sess), bson.D{{Key: "_id", Value: "x"}})
	require.Error(t, err)
	assert.Equal(t, int32(263), mongoCommandCode(err))
	assert.Contains(t, err.Error(), "Transactions are not available in session-isolation mode")
	assert.Contains(t, err.Error(), "Use doltCommit instead")
}

// D13 default-mode counterpart: without --session-isolation, the same
// startTransaction-bearing operation must succeed. Guards against a
// regression that would reject startTransaction unconditionally.
func TestSessionIsolation_StartTransactionAllowedInDefaultMode(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.Collection(t)

	sess, err := env.Client.StartSession()
	require.NoError(t, err)
	defer sess.EndSession(ctx)
	require.NoError(t, sess.StartTransaction())

	_, err = coll.InsertOne(mongo.NewSessionContext(ctx, sess), bson.D{{Key: "_id", Value: "x"}})
	require.NoError(t, err, "startTransaction must succeed when --session-isolation is not set")
}

// In --session-isolation mode writes auto-fork into a per-connection overlay.
// Other connections see the committed branch HEAD, not the in-flight overlay.
// After doltCommit, the writes are visible to everyone.
func TestSessionIsolation_ImplicitForkVisibility(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()
	collA := env.Collection(t)

	cB := siClient(t, env)
	collB := cB.Database(collA.Database().Name()).Collection(collA.Name())

	_, err := collA.InsertOne(ctx, bson.D{{Key: "_id", Value: "before-commit"}})
	require.NoError(t, err)

	cnt, err := collB.CountDocuments(ctx, bson.D{{Key: "_id", Value: "before-commit"}})
	require.NoError(t, err)
	assert.EqualValues(t, 0, cnt, "B should NOT see A's uncommitted write")

	res := collA.Database().RunCommand(ctx, bson.D{{Key: "doltCommit", Value: 1}, {Key: "message", Value: "A's commit"}})
	require.NoError(t, res.Err())

	cnt, err = collB.CountDocuments(ctx, bson.D{{Key: "_id", Value: "before-commit"}})
	require.NoError(t, err)
	assert.EqualValues(t, 1, cnt, "B should see A's write after doltCommit")
}

// Two concurrent sessions modifying the same _id end up with one winner;
// the second committer's doltCommit fails with a data-conflict error and
// the loser's pending overlay is preserved for retry / endSession.
func TestSessionIsolation_ConflictRejectsSecondCommit(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()
	collA := env.Collection(t)

	_, err := collA.InsertOne(ctx, bson.D{{Key: "_id", Value: "p"}, {Key: "x", Value: "seed"}})
	require.NoError(t, err)
	require.NoError(t, collA.Database().RunCommand(ctx, bson.D{{Key: "doltCommit", Value: 1}, {Key: "message", Value: "seed"}}).Err())

	cB := siClient(t, env)
	collB := cB.Database(collA.Database().Name()).Collection(collA.Name())

	_, err = collA.UpdateOne(ctx, bson.D{{Key: "_id", Value: "p"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "x", Value: "A"}}}})
	require.NoError(t, err)
	_, err = collB.UpdateOne(ctx, bson.D{{Key: "_id", Value: "p"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "x", Value: "B"}}}})
	require.NoError(t, err)

	require.NoError(t, collA.Database().RunCommand(ctx, bson.D{{Key: "doltCommit", Value: 1}, {Key: "message", Value: "A"}}).Err())

	err = collB.Database().RunCommand(ctx, bson.D{{Key: "doltCommit", Value: 1}, {Key: "message", Value: "B"}}).Err()
	require.Error(t, err, "second commit must be rejected on conflict")
	assert.Contains(t, err.Error(), "data conflict")

	var final bson.M
	require.NoError(t, collA.FindOne(ctx, bson.D{{Key: "_id", Value: "p"}}).Decode(&final))
	assert.Equal(t, "A", final["x"], "first committer's value is the one persisted; B's commit was rejected")
}

// Concurrent sessions writing different _ids both commit cleanly via three-way
// merge: the second committer sees the first's commit as "theirs" but the
// changes don't overlap, so the merge succeeds and the final state contains
// both docs plus any pre-existing seed.
func TestSessionIsolation_NonConflictingMergesCleanly(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()
	collA := env.Collection(t)

	_, err := collA.InsertOne(ctx, bson.D{{Key: "_id", Value: "seed"}})
	require.NoError(t, err)
	require.NoError(t, collA.Database().RunCommand(ctx, bson.D{{Key: "doltCommit", Value: 1}, {Key: "message", Value: "seed"}}).Err())

	cB := siClient(t, env)
	collB := cB.Database(collA.Database().Name()).Collection(collA.Name())

	_, err = collA.InsertOne(ctx, bson.D{{Key: "_id", Value: "from-A"}})
	require.NoError(t, err)
	_, err = collB.InsertOne(ctx, bson.D{{Key: "_id", Value: "from-B"}})
	require.NoError(t, err)

	require.NoError(t, collA.Database().RunCommand(ctx, bson.D{{Key: "doltCommit", Value: 1}, {Key: "message", Value: "A"}}).Err())
	require.NoError(t, collB.Database().RunCommand(ctx, bson.D{{Key: "doltCommit", Value: 1}, {Key: "message", Value: "B"}}).Err())

	cur, err := collA.Find(ctx, bson.D{})
	require.NoError(t, err)
	var docs []bson.M
	require.NoError(t, cur.All(ctx, &docs))
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		if s, ok := d["_id"].(string); ok {
			ids = append(ids, s)
		}
	}
	assert.ElementsMatch(t, []string{"seed", "from-A", "from-B"}, ids)
}

// A session sees its own writes via the per-connection overlay before
// committing; reads from the same connection inside its in-flight work
// observe the pending changes.
func TestSessionIsolation_ReadYourOwnWrites(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()
	coll := env.Collection(t)

	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: "own"}, {Key: "v", Value: "v1"}})
	require.NoError(t, err)

	var got bson.M
	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "_id", Value: "own"}}).Decode(&got))
	assert.Equal(t, "v1", got["v"], "session must read its own uncommitted write")

	_, err = coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: "own"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: "v2"}}}})
	require.NoError(t, err)

	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "_id", Value: "own"}}).Decode(&got))
	assert.Equal(t, "v2", got["v"], "session must read its own uncommitted update")
}

func siDoltCommit(t *testing.T, coll *mongo.Collection, message string) error {
	t.Helper()
	return coll.Database().RunCommand(context.Background(), bson.D{
		{Key: "doltCommit", Value: 1},
		{Key: "message", Value: message},
	}).Err()
}

func siCreateBranch(t *testing.T, db *mongo.Database, branch string) {
	t.Helper()
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "branch", Value: branch},
	}).Err())
}

// D2: After client B forks at HEAD and client A commits, B continues to
// observe only its own pending writes (not A's new HEAD) until B itself
// commits. B's three-way merge then folds A's commit in.
func TestSessionIsolation_DirtySessionPinnedToForkPoint(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()
	collA := env.Collection(t)

	cB := siClient(t, env)
	collB := cB.Database(collA.Database().Name()).Collection(collA.Name())

	_, err := collA.InsertOne(ctx, bson.D{{Key: "_id", Value: "from-A"}})
	require.NoError(t, err)
	_, err = collB.InsertOne(ctx, bson.D{{Key: "_id", Value: "from-B"}})
	require.NoError(t, err)

	require.NoError(t, siDoltCommit(t, collA, "A commits first"))

	cur, err := collB.Find(ctx, bson.D{})
	require.NoError(t, err)
	var pre []bson.M
	require.NoError(t, cur.All(ctx, &pre))
	preIDs := make([]string, 0, len(pre))
	for _, d := range pre {
		if s, ok := d["_id"].(string); ok {
			preIDs = append(preIDs, s)
		}
	}
	assert.ElementsMatch(t, []string{"from-B"}, preIDs,
		"B must remain pinned to fork point: must NOT see A's commit, must see only own write")

	require.NoError(t, siDoltCommit(t, collB, "B commits second"))

	cur, err = collB.Find(ctx, bson.D{})
	require.NoError(t, err)
	var post []bson.M
	require.NoError(t, cur.All(ctx, &post))
	postIDs := make([]string, 0, len(post))
	for _, d := range post {
		if s, ok := d["_id"].(string); ok {
			postIDs = append(postIDs, s)
		}
	}
	assert.ElementsMatch(t, []string{"from-A", "from-B"}, postIDs,
		"after B commits, three-way merge folds A's commit into B's view")
}

// D5: After a session commits its own work, it advances past the fork
// point and observes commits made by other sessions in the interim.
func TestSessionIsolation_SeesNewCommitsAfterOwnCommit(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()
	collA := env.Collection(t)

	cB := siClient(t, env)
	collB := cB.Database(collA.Database().Name()).Collection(collA.Name())

	_, err := collA.InsertOne(ctx, bson.D{{Key: "_id", Value: "from-A"}})
	require.NoError(t, err)
	require.NoError(t, siDoltCommit(t, collA, "A first"))

	_, err = collB.InsertOne(ctx, bson.D{{Key: "_id", Value: "from-B"}})
	require.NoError(t, err)
	require.NoError(t, siDoltCommit(t, collB, "B second"))

	cur, err := collA.Find(ctx, bson.D{})
	require.NoError(t, err)
	var docs []bson.M
	require.NoError(t, cur.All(ctx, &docs))
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		if s, ok := d["_id"].(string); ok {
			ids = append(ids, s)
		}
	}
	assert.ElementsMatch(t, []string{"from-A", "from-B"}, ids,
		"A must see B's commit after A finished its own commit cycle")
}

// D9: One session writing to two different branches keeps the pending
// overlays separate (indexed by (owner, branch)). Committing on one
// branch does not affect the pending state on the other.
func TestSessionIsolation_MultiBranchIsolationOneSession(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()

	dbName := fmt.Sprintf("multibranch_%d", env.Port)
	collName := "items"

	cA := siClient(t, env)

	cB := siClient(t, env)

	_, err := cA.Database(dbName).Collection(collName).InsertOne(ctx, bson.D{{Key: "_id", Value: "seed"}})
	require.NoError(t, err)
	require.NoError(t, cA.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1}, {Key: "message", Value: "seed"},
	}).Err())

	siCreateBranch(t, cA.Database(dbName+"@main"), "feat")

	mainA := cA.Database(dbName).Collection(collName)
	featA := cA.Database(dbName + "@feat").Collection(collName)

	_, err = mainA.InsertOne(ctx, bson.D{{Key: "_id", Value: "main-write"}})
	require.NoError(t, err)
	_, err = featA.InsertOne(ctx, bson.D{{Key: "_id", Value: "feat-write"}})
	require.NoError(t, err)

	require.NoError(t, cA.Database(dbName+"@feat").RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1}, {Key: "message", Value: "feat-only"},
	}).Err())

	mainB := cB.Database(dbName).Collection(collName)
	featB := cB.Database(dbName + "@feat").Collection(collName)

	cnt, err := mainB.CountDocuments(ctx, bson.D{{Key: "_id", Value: "main-write"}})
	require.NoError(t, err)
	assert.EqualValues(t, 0, cnt, "B on main must NOT see A's uncommitted main write")

	cnt, err = featB.CountDocuments(ctx, bson.D{{Key: "_id", Value: "feat-write"}})
	require.NoError(t, err)
	assert.EqualValues(t, 1, cnt, "B on feat must see A's committed feat write")

	require.NoError(t, cA.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1}, {Key: "message", Value: "main-now"},
	}).Err())

	cnt, err = mainB.CountDocuments(ctx, bson.D{{Key: "_id", Value: "main-write"}})
	require.NoError(t, err)
	assert.EqualValues(t, 1, cnt, "B on main must see A's main write after A commits on main")
}

// D11: Three sessions fork from the same HEAD and sequentially commit
// non-conflicting inserts. Each subsequent commit is a three-way merge
// against the advancing HEAD; all three writes land in the final state.
func TestSessionIsolation_ThreeWayConcurrentCommitSerializes(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()
	collA := env.Collection(t)

	_, err := collA.InsertOne(ctx, bson.D{{Key: "_id", Value: "seed"}})
	require.NoError(t, err)
	require.NoError(t, siDoltCommit(t, collA, "seed C0"))

	cB := siClient(t, env)
	cC := siClient(t, env)
	collB := cB.Database(collA.Database().Name()).Collection(collA.Name())
	collC := cC.Database(collA.Database().Name()).Collection(collA.Name())

	_, err = collA.InsertOne(ctx, bson.D{{Key: "_id", Value: "from-A"}})
	require.NoError(t, err)
	_, err = collB.InsertOne(ctx, bson.D{{Key: "_id", Value: "from-B"}})
	require.NoError(t, err)
	_, err = collC.InsertOne(ctx, bson.D{{Key: "_id", Value: "from-C"}})
	require.NoError(t, err)

	require.NoError(t, siDoltCommit(t, collA, "A -> C1"))
	require.NoError(t, siDoltCommit(t, collB, "B -> C2"))
	require.NoError(t, siDoltCommit(t, collC, "C -> C3"))

	cur, err := collA.Find(ctx, bson.D{})
	require.NoError(t, err)
	var docs []bson.M
	require.NoError(t, cur.All(ctx, &docs))
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		if s, ok := d["_id"].(string); ok {
			ids = append(ids, s)
		}
	}
	assert.ElementsMatch(t, []string{"seed", "from-A", "from-B", "from-C"}, ids,
		"all three concurrent writes plus seed must be present after serialization")
}

// D12: A deletes a row while B modifies the same row. After A commits,
// B's commit must be rejected with a conflict.
func TestSessionIsolation_DeleteVsModifyConflict(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()
	collA := env.Collection(t)

	_, err := collA.InsertOne(ctx, bson.D{{Key: "_id", Value: "p"}, {Key: "x", Value: "original"}})
	require.NoError(t, err)
	require.NoError(t, siDoltCommit(t, collA, "seed"))

	cB := siClient(t, env)
	collB := cB.Database(collA.Database().Name()).Collection(collA.Name())

	_, err = collA.DeleteOne(ctx, bson.D{{Key: "_id", Value: "p"}})
	require.NoError(t, err)
	_, err = collB.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: "p"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "x", Value: "modified"}}}})
	require.NoError(t, err)

	require.NoError(t, siDoltCommit(t, collA, "A deletes"))

	err = siDoltCommit(t, collB, "B modifies")
	require.Error(t, err, "delete-vs-modify on the same _id must be a conflict")
	assert.Contains(t, err.Error(), "conflict")
}

// Branch variant of D1: ImplicitForkVisibility, exercised on a freshly
// created non-main branch. Ensures the (owner, branch) overlay key
// works for non-main branches, not just main.
func TestSessionIsolation_ImplicitForkVisibility_OnFeatureBranch(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()

	dbName := fmt.Sprintf("brfork_%d", env.Port)
	collName := "items"

	_, err := env.Client.Database(dbName).Collection(collName).InsertOne(ctx, bson.D{{Key: "_id", Value: "seed"}})
	require.NoError(t, err)
	require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1}, {Key: "message", Value: "seed"},
	}).Err())

	siCreateBranch(t, env.Client.Database(dbName+"@main"), "feature")

	collA := env.Client.Database(dbName + "@feature").Collection(collName)
	cB := siClient(t, env)
	collB := cB.Database(dbName + "@feature").Collection(collName)

	_, err = collA.InsertOne(ctx, bson.D{{Key: "_id", Value: "feature-only"}})
	require.NoError(t, err)

	cnt, err := collB.CountDocuments(ctx, bson.D{{Key: "_id", Value: "feature-only"}})
	require.NoError(t, err)
	assert.EqualValues(t, 0, cnt, "B on feature must NOT see A's uncommitted write")

	require.NoError(t, collA.Database().RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1}, {Key: "message", Value: "A on feature"},
	}).Err())

	cnt, err = collB.CountDocuments(ctx, bson.D{{Key: "_id", Value: "feature-only"}})
	require.NoError(t, err)
	assert.EqualValues(t, 1, cnt, "B on feature must see A's write after commit")
}

// Branch variant of D4: NonConflictingMergesCleanly, on a non-main branch.
func TestSessionIsolation_NonConflictingMergesCleanly_OnFeatureBranch(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()

	dbName := fmt.Sprintf("brmerge_%d", env.Port)
	collName := "items"

	_, err := env.Client.Database(dbName).Collection(collName).InsertOne(ctx, bson.D{{Key: "_id", Value: "seed"}})
	require.NoError(t, err)
	require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1}, {Key: "message", Value: "seed"},
	}).Err())
	siCreateBranch(t, env.Client.Database(dbName+"@main"), "feature")

	collA := env.Client.Database(dbName + "@feature").Collection(collName)
	cB := siClient(t, env)
	collB := cB.Database(dbName + "@feature").Collection(collName)

	_, err = collA.InsertOne(ctx, bson.D{{Key: "_id", Value: "from-A"}})
	require.NoError(t, err)
	_, err = collB.InsertOne(ctx, bson.D{{Key: "_id", Value: "from-B"}})
	require.NoError(t, err)

	require.NoError(t, collA.Database().RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1}, {Key: "message", Value: "A"},
	}).Err())
	require.NoError(t, collB.Database().RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1}, {Key: "message", Value: "B"},
	}).Err())

	cur, err := collA.Find(ctx, bson.D{})
	require.NoError(t, err)
	var docs []bson.M
	require.NoError(t, cur.All(ctx, &docs))
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		if s, ok := d["_id"].(string); ok {
			ids = append(ids, s)
		}
	}
	assert.ElementsMatch(t, []string{"seed", "from-A", "from-B"}, ids)
}

func TestSessionIsolation_LsidSupersedeSurfacedAsCode225(t *testing.T) {
	env := startDumboDB(t)

	a := dialWire(t, env)
	b := dialWire(t, env)

	dbName := fmt.Sprintf("supersede_%d", env.Port)
	lsid := freshLsid()

	resA, err := a.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "from-A"}}}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	assert.Equal(t, 1.0, resA["ok"])

	resB, err := b.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "from-B"}}}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	assert.Equal(t, 1.0, resB["ok"])

	resA2, err := a.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	assert.Equal(t, 0.0, resA2["ok"])
	if code, ok := resA2["code"].(int32); ok {
		assert.Equal(t, int32(225), code)
	} else if code, ok := resA2["code"].(int64); ok {
		assert.Equal(t, int64(225), code)
	} else {
		t.Fatalf("expected numeric code in response: %#v", resA2)
	}
	if msg, ok := resA2["errmsg"].(string); ok {
		assert.Contains(t, msg, "taken over")
	}
}

func TestSessionIsolation_DoltCommitDurabilityUnderConcurrentSupersede(t *testing.T) {
	env := startDumboDB(t)
	dbName := fmt.Sprintf("commitfence_%d", env.Port)
	lsid := freshLsid()

	a := dialWire(t, env)

	_, err := a.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "from-A"}}}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)

	commitDone := make(chan struct{})
	bDone := make(chan bson.M, 1)
	go func() {
		<-commitDone
		b := dialWire(t, env)
		res, err := b.run(bson.D{
			{Key: "find", Value: "c"},
			{Key: "filter", Value: bson.D{}},
			{Key: "lsid", Value: lsid},
			{Key: "$db", Value: dbName},
		})
		require.NoError(t, err)
		bDone <- res
	}()

	close(commitDone)
	resCommit, err := a.run(bson.D{
		{Key: "doltCommit", Value: int32(1)},
		{Key: "message", Value: "A commits"},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resCommit["ok"])

	resFind := <-bDone
	require.Equal(t, 1.0, resFind["ok"])
	first := firstBatchFromFindResponse(t, resFind)
	require.Len(t, first, 1)
	doc, ok := first[0].(bson.D)
	require.True(t, ok)
	assert.Equal(t, "from-A", bsonDLookup(doc, "_id"))

	resA2, err := a.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	assert.Equal(t, 0.0, resA2["ok"])
	if code, ok := resA2["code"].(int32); ok {
		assert.Equal(t, int32(225), code)
	} else if code, ok := resA2["code"].(int64); ok {
		assert.Equal(t, int64(225), code)
	}
}

// Default-mode reconnect: a multi-document transaction started by
// conn1 continues on conn2 when conn2 carries the same lsid +
// txnNumber + autocommit:false. The dispatch observes autocommit:false
// on every frame (not just the first via startTransaction:true), so
// the second TCP connection's InTransaction flag reflects the wire
// frame and writes/reads route through the lsid-keyed pending overlay.
// This is the property the parity-side P6 lsid_reconnect_wire test
// covers against an RS Mongo; this local version locks it in for
// DumboDB CI runs where MONGO_RS_URI may not be set.
func TestTransaction_WireReconnectResumesTransactionState(t *testing.T) {
	env := startDumboDB(t)
	dbName := fmt.Sprintf("txnreconnect_%d", env.Port)
	lsid := freshLsid()
	const txnNum = int64(1)

	conn1 := dialWire(t, env)
	resInsert, err := conn1.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "in-txn"}}}},
		{Key: "lsid", Value: lsid},
		{Key: "txnNumber", Value: txnNum},
		{Key: "startTransaction", Value: true},
		{Key: "autocommit", Value: false},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resInsert["ok"])
	_ = conn1.c.Close()

	conn2 := dialWire(t, env)
	resFind, err := conn2.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "lsid", Value: lsid},
		{Key: "txnNumber", Value: txnNum},
		{Key: "autocommit", Value: false},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resFind["ok"])
	first := firstBatchFromFindResponse(t, resFind)
	require.Len(t, first, 1, "conn2 must see conn1's uncommitted in-txn write via the lsid-keyed overlay")
	doc, ok := first[0].(bson.D)
	require.True(t, ok)
	assert.Equal(t, "in-txn", bsonDLookup(doc, "_id"))

	resCommit, err := conn2.run(bson.D{
		{Key: "commitTransaction", Value: int32(1)},
		{Key: "lsid", Value: lsid},
		{Key: "txnNumber", Value: txnNum},
		{Key: "autocommit", Value: false},
		{Key: "$db", Value: "admin"},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resCommit["ok"])

	// A fresh lsid sees the now-durable write.
	conn3 := dialWire(t, env)
	resFind2, err := conn3.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "lsid", Value: freshLsid()},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	first = firstBatchFromFindResponse(t, resFind2)
	require.Len(t, first, 1)
	doc, ok = first[0].(bson.D)
	require.True(t, ok)
	assert.Equal(t, "in-txn", bsonDLookup(doc, "_id"))
}

// D10: in --session-isolation mode, uncommitted writes survive a TCP
// disconnect when a new connection arrives with the same lsid. The
// pending overlay sits on the underlying *dsess.DoltSession; the
// supersede on reconnect swaps the shadow but keeps the session. A
// subsequent doltCommit through the resumed session merges the
// previously-uncommitted state to HEAD, where a fresh lsid can read it.
func TestSessionIsolation_WireReconnectResumesUncommittedState(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	dbName := fmt.Sprintf("wirereconnect_%d", env.Port)
	lsid := freshLsid()

	conn1 := dialWire(t, env)
	resInsert, err := conn1.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "before-disconnect"}}}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resInsert["ok"])

	// Drop conn1 without committing. The write lives in the session's
	// pending overlay, not on disk.
	_ = conn1.c.Close()

	// conn2 with the same lsid must observe the uncommitted write via
	// the resumed *dsess.DoltSession.
	conn2 := dialWire(t, env)
	resFind1, err := conn2.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resFind1["ok"])
	first := firstBatchFromFindResponse(t, resFind1)
	require.Len(t, first, 1, "conn2 must see conn1's uncommitted write")
	doc, ok := first[0].(bson.D)
	require.True(t, ok)
	assert.Equal(t, "before-disconnect", bsonDLookup(doc, "_id"))

	// conn2 commits. The write becomes durable in HEAD.
	resCommit, err := conn2.run(bson.D{
		{Key: "doltCommit", Value: int32(1)},
		{Key: "message", Value: "conn2 commits conn1's write"},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resCommit["ok"])

	// A fresh lsid sees the now-durable write from HEAD.
	conn3 := dialWire(t, env)
	freshOtherLsid := freshLsid()
	resFind2, err := conn3.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "lsid", Value: freshOtherLsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resFind2["ok"])
	first = firstBatchFromFindResponse(t, resFind2)
	require.Len(t, first, 1, "a fresh lsid must read the now-durable write from HEAD")
	doc, ok = first[0].(bson.D)
	require.True(t, ok)
	assert.Equal(t, "before-disconnect", bsonDLookup(doc, "_id"))
}

func TestSessionIsolation_ReconnectResumesCommittedState(t *testing.T) {
	env := startDumboDB(t)

	a := dialWire(t, env)
	dbName := fmt.Sprintf("reconnect_%d", env.Port)
	lsid := freshLsid()

	resInsert, err := a.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "from-A"}}}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resInsert["ok"])

	resCommit, err := a.run(bson.D{
		{Key: "doltCommit", Value: int32(1)},
		{Key: "message", Value: "A commits"},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resCommit["ok"])

	_ = a.c.Close()

	b := dialWire(t, env)
	resFind, err := b.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resFind["ok"])

	first := firstBatchFromFindResponse(t, resFind)
	require.Len(t, first, 1)
	doc, ok := first[0].(bson.D)
	require.True(t, ok)
	assert.Equal(t, "from-A", bsonDLookup(doc, "_id"))
}

func firstBatchFromFindResponse(t *testing.T, res bson.M) bson.A {
	t.Helper()
	cursor, ok := res["cursor"].(bson.D)
	require.True(t, ok, "find response must carry cursor; got %#v", res)
	first, ok := bsonDLookup(cursor, "firstBatch").(bson.A)
	require.True(t, ok, "cursor must have firstBatch; got %#v", cursor)
	return first
}

func bsonDLookup(d bson.D, key string) interface{} {
	for _, e := range d {
		if e.Key == key {
			return e.Value
		}
	}
	return nil
}

// Branch variant of D6: ConflictRejectsSecondCommit, on a non-main branch.
func TestSessionIsolation_ConflictRejectsSecondCommit_OnFeatureBranch(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()

	dbName := fmt.Sprintf("brconf_%d", env.Port)
	collName := "items"

	_, err := env.Client.Database(dbName).Collection(collName).InsertOne(ctx, bson.D{
		{Key: "_id", Value: "p"}, {Key: "x", Value: "seed"},
	})
	require.NoError(t, err)
	require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1}, {Key: "message", Value: "seed"},
	}).Err())
	siCreateBranch(t, env.Client.Database(dbName+"@main"), "feature")

	collA := env.Client.Database(dbName + "@feature").Collection(collName)
	cB := siClient(t, env)
	collB := cB.Database(dbName + "@feature").Collection(collName)

	_, err = collA.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: "p"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "x", Value: "A"}}}})
	require.NoError(t, err)
	_, err = collB.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: "p"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "x", Value: "B"}}}})
	require.NoError(t, err)

	require.NoError(t, collA.Database().RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1}, {Key: "message", Value: "A"},
	}).Err())

	err = collB.Database().RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1}, {Key: "message", Value: "B"},
	}).Err()
	require.Error(t, err, "second commit on feature branch must be rejected on conflict")
	assert.Contains(t, err.Error(), "conflict")

	var final bson.M
	require.NoError(t, collA.FindOne(ctx, bson.D{{Key: "_id", Value: "p"}}).Decode(&final))
	assert.Equal(t, "A", final["x"], "feature branch must reflect A's value, not B's rejected commit")
}
