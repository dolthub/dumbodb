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

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestReadOnlyRootishCommands(t *testing.T) {
	for _, mode := range []struct {
		name string
		args []string
	}{
		{name: "default"},
		{name: "session_isolation", args: []string{"--session-isolation"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			env := startDumboDB(t, mode.args...)
			verifyReadOnlyRootishCommands(t, env)
		})
	}
}

func verifyReadOnlyRootishCommands(t *testing.T, env *dumboDBTestEnv) {
	t.Helper()

	ctx := context.Background()
	dbName := fmt.Sprintf("readonly%d", rand.Int64N(1_000_000))
	mainDB := env.Client.Database(dbName)
	require.NoError(t, mainDB.Drop(ctx))

	items := mainDB.Collection("items")
	_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "version", Value: int32(1)}})
	require.NoError(t, err)
	snapshotHash := dumboDBCommit(t, env, dbName, "snapshot", "tester <tester@example.com>")

	var tagResult bson.M
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "dumboTag", Value: int32(1)},
		{Key: "name", Value: "v1"},
		{Key: "hash", Value: snapshotHash},
	}).Decode(&tagResult))

	_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}, {Key: "version", Value: int32(2)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "advance main", "tester <tester@example.com>")

	for _, rootish := range []string{"v1", snapshotHash} {
		snapshotDB := env.Client.Database(dbName + "@" + rootish)

		var status bson.M
		require.NoError(t, snapshotDB.RunCommand(ctx, bson.D{{Key: "dumboStatus", Value: int32(1)}}).Decode(&status))
		assert.Equal(t, false, status["dirty"])
		assert.Equal(t, true, status["readonly"])
		assert.Equal(t, snapshotHash, status["commitId"])

		var diff bson.M
		require.NoError(t, snapshotDB.RunCommand(ctx, bson.D{{Key: "dumboDiff", Value: int32(1)}}).Decode(&diff))
		assert.Empty(t, diff["changes"])

		_, err = snapshotDB.Collection("items").UpdateOne(
			ctx,
			bson.D{{Key: "_id", Value: int32(1)}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "version", Value: int32(99)}}}},
		)
		assertReadOnlySnapshotCommandError(t, err)
	}

	tagDB := env.Client.Database(dbName + "@v1")
	err = tagDB.CreateCollection(ctx, "created")
	assertReadOnlySnapshotCommandError(t, err)

	_, err = tagDB.Collection("items").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "version", Value: int32(1)}},
	})
	assertReadOnlySnapshotCommandError(t, err)

	err = tagDB.Collection("items").Drop(ctx)
	assertReadOnlySnapshotCommandError(t, err)

	_, err = env.Client.Database(dbName+"@neverMade").Collection("items").UpdateOne(
		ctx,
		bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "version", Value: int32(99)}}}},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rootish \"neverMade\": not found")

	count, err := mainDB.Collection("items").CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), count, "refused snapshot writes must not alter main")
}

func assertReadOnlySnapshotCommandError(t *testing.T, err error) {
	t.Helper()

	require.Error(t, err)
	commandError, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 96, commandError.Code)
	assert.Contains(t, commandError.Message, "cannot write to a read-only database snapshot")
}
