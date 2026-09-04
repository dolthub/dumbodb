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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMerge_CollectionAddedOnBothBranches(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mergeadd_%d", rand.Int64N(1_000_000))

	mainDB := env.Client.Database(dbName)
	require.NoError(t, mainDB.Drop(ctx))
	_, err := mainDB.Collection("items").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(10)}, {Key: "side", Value: "main"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "main: add items/10", "alice <a@x.io>")

	require.NoError(t, env.Client.Database(dbName+"@main~1").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "featureA"},
	}).Err())
	featDB := env.Client.Database(dbName + "@featureA")
	_, err = featDB.Collection("items").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(20)}, {Key: "side", Value: "featureA"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@featureA", "featureA: add items/20", "bob <b@x.io>")

	var res bson.M
	err = env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "mergeIn", Value: "featureA"},
		{Key: "message", Value: "merge featureA into main"},
		{Key: "author", Value: "alice <a@x.io>"},
	}).Decode(&res)
	require.NoError(t, err, "merging a collection added on both branches must not error")
	assert.EqualValues(t, 1, res["ok"])

	merged := env.Client.Database(dbName + "@main").Collection("items")
	require.NoError(t, merged.FindOne(ctx, bson.D{{Key: "_id", Value: int32(10)}}).Err(),
		"main's items/10 must survive")
	require.NoError(t, merged.FindOne(ctx, bson.D{{Key: "_id", Value: int32(20)}}).Err(),
		"featureA's items/20 must survive")
}

func TestMerge_CollectionAddedOnBothBranches_Conflict(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mergeaddc_%d", rand.Int64N(1_000_000))

	mainDB := env.Client.Database(dbName)
	require.NoError(t, mainDB.Drop(ctx))
	_, err := mainDB.Collection("items").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(10)}, {Key: "v", Value: "main"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "main: add items/10", "alice <a@x.io>")

	require.NoError(t, env.Client.Database(dbName+"@main~1").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: "featureA"},
	}).Err())
	featDB := env.Client.Database(dbName + "@featureA")
	_, err = featDB.Collection("items").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(10)}, {Key: "v", Value: "featureA"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@featureA", "featureA: add items/10", "bob <b@x.io>")

	branch := env.Client.Database(dbName + "@main")
	raw := runCommandRaw(t, branch, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "mergeIn", Value: "featureA"},
		{Key: "message", Value: "merge featureA into main"},
		{Key: "author", Value: "alice <a@x.io>"},
	})
	require.EqualValues(t, 0, raw["ok"], "same _id added on both branches must conflict")

	conflicts := getConflictsByCollection(t, branch)
	require.Len(t, conflicts["items"], 1, "one conflict on items/10")

	resolveAllConflicts(t, branch, "ours")
	mergeContinue(t, branch)

	col := branch.Collection("items")
	assert.Equal(t, "main", getDocField(t, col, 10, "v"), "ours resolution keeps main's value")
}
