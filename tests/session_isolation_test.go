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
	uri := fmt.Sprintf("mongodb://127.0.0.1:%d", env.port)
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

// startTransaction is rejected with code 263 when the server is in
// --session-isolation mode. The wire-protocol field, not a separate command,
// is what gets rejected so any first transactional write triggers it.
func TestSessionIsolation_StartTransactionRejected(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()
	coll := env.collection(t)

	sess, err := env.client.StartSession()
	require.NoError(t, err)
	defer sess.EndSession(ctx)
	require.NoError(t, sess.StartTransaction())

	_, err = coll.InsertOne(mongo.NewSessionContext(ctx, sess), bson.D{{Key: "_id", Value: "x"}})
	require.Error(t, err)
	assert.Equal(t, int32(263), mongoCommandCode(err))
	assert.Contains(t, err.Error(), "session-isolation")
}

// In --session-isolation mode writes auto-fork into a per-connection overlay.
// Other connections see the committed branch HEAD, not the in-flight overlay.
// After doltCommit, the writes are visible to everyone.
func TestSessionIsolation_ImplicitForkVisibility(t *testing.T) {
	env := startDumboDB(t, "--session-isolation")
	ctx := context.Background()
	collA := env.collection(t)

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
	collA := env.collection(t)

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
	collA := env.collection(t)

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
	coll := env.collection(t)

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
