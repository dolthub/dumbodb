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
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// conflictResolutionRequired is dumbodb's ConflictResolutionRequired code.
const conflictResolutionRequired = 2000000001

// While a merge is paused on conflicts the branch accepts no writes: every
// kind is refused with an error saying so, including a transaction begun
// before the pause, until the conflicts are resolved and the merge continued.
func TestWritesRefusedWhileAnOperationIsPausedOnConflicts(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("paused%d", rand.Int64N(1_000_000))
	main := env.Client.Database(dbName)
	items := main.Collection("items")
	_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: "base"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "base", "alice")
	require.NoError(t, env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"},
	}).Err())
	_, err = env.Client.Database(dbName+"@feature").Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: "x"}, {Key: "v", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature", "alice")
	_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: "x"}, {Key: "v", Value: int32(2)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "main", "alice")

	// A transaction that began, and wrote, before the merge paused.
	early := siClient(t, env)
	sess, err := early.StartSession()
	require.NoError(t, err)
	defer sess.EndSession(ctx)
	require.NoError(t, sess.StartTransaction(options.Transaction()))
	_, err = early.Database(dbName).Collection("items").InsertOne(mongo.NewSessionContext(ctx, sess), bson.D{{Key: "_id", Value: "early"}})
	require.NoError(t, err)

	require.Error(t, main.RunCommand(ctx, bson.D{{Key: "dumboMerge", Value: int32(1)}, {Key: "mergeIn", Value: "feature"}}).Err())

	refused := func(what string, err error) {
		t.Helper()
		require.ErrorContains(t, err, "conflicting state", "%s must be refused while the merge is paused", what)
		require.ErrorContains(t, err, "dumboResolveConflict", what)
		require.NotContains(t, err.Error(), ".go:", "%s must carry the refusal, not an internal error", what)
		require.EqualValues(t, conflictResolutionRequired, mongoCommandCode(err), "%s must be refused as ConflictResolutionRequired", what)
	}
	_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: "during"}})
	refused("an insert", err)
	_, err = items.UpdateOne(ctx, bson.D{{Key: "_id", Value: "base"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "touched", Value: true}}}})
	refused("an update", err)
	_, err = items.DeleteOne(ctx, bson.D{{Key: "_id", Value: "base"}})
	refused("a delete", err)
	refused("creating a collection", main.CreateCollection(ctx, "another"))
	for name, err := range map[string]error{
		"findOneAndUpdate": items.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: "base"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "t", Value: 1}}}}).Err(),
		"findOneAndDelete": items.FindOneAndDelete(ctx, bson.D{{Key: "_id", Value: "base"}}).Err(),
	} {
		refused(name, err)
	}
	var bulk bson.M
	require.NoError(t, env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "bulkWrite", Value: int32(1)},
		{Key: "ops", Value: bson.A{bson.D{{Key: "insert", Value: int32(0)}, {Key: "document", Value: bson.D{{Key: "_id", Value: "bulk"}}}}}},
		{Key: "nsInfo", Value: bson.A{bson.D{{Key: "ns", Value: dbName + ".items"}}}},
	}).Decode(&bulk))
	op := bulk["cursor"].(bson.M)["firstBatch"].(bson.A)[0].(bson.M)
	require.EqualValues(t, conflictResolutionRequired, op["code"], "bulkWrite: %v", op)
	require.Contains(t, op["errmsg"], "conflicting state")
	require.NotContains(t, op["errmsg"], ".go:", "bulkWrite must carry the refusal, not a trace")
	refused("committing a transaction begun before the pause", sess.CommitTransaction(ctx))

	// Version-control commands that the paused merge stands in the way of are
	// refused with the same code.
	for name, cmd := range map[string]bson.D{
		"dumboCommit":                     {{Key: "dumboCommit", Value: int32(1)}, {Key: "message", Value: "m"}},
		"dumboReset":                      {{Key: "dumboReset", Value: int32(1)}},
		"another merge":                   {{Key: "dumboMerge", Value: int32(1)}, {Key: "mergeIn", Value: "feature"}},
		"continue with conflicts pending": {{Key: "dumboMerge", Value: int32(1)}, {Key: "continue", Value: int32(1)}},
	} {
		err := main.RunCommand(ctx, cmd).Err()
		require.EqualValues(t, conflictResolutionRequired, mongoCommandCode(err), "%s while the merge is paused: %v", name, err)
	}

	var conflicts bson.M
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "dumboConflicts", Value: int32(1)}}).Decode(&conflicts))
	for _, c := range conflicts["conflicts"].(bson.A) {
		conflict := c.(bson.M)
		require.NoError(t, main.RunCommand(ctx, bson.D{
			{Key: "dumboResolveConflict", Value: int32(1)}, {Key: "collection", Value: conflict["collection"]},
			{Key: "conflictId", Value: conflict["conflictId"]}, {Key: "resolution", Value: "theirs"},
		}).Err())
	}
	require.NoError(t, main.RunCommand(ctx, bson.D{{Key: "dumboMerge", Value: int32(1)}, {Key: "continue", Value: int32(1)}}).Err())

	_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: "after"}})
	require.NoError(t, err, "writes are accepted again once the merge is finished")
	n, err := items.CountDocuments(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: bson.A{"during", "early"}}}}})
	require.NoError(t, err)
	require.Zero(t, n, "a refused write must not have landed")
}
