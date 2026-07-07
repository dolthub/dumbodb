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

// TestMerge_CollectionAddedOnBothBranches reproduces workspace-d1b: merging a
// collection that was created independently on both branches (so it is absent
// from the merge base) must not crash. The base collection hash for such a
// collection is the zero hash, and the 3-way collection merge must treat the
// missing base as an empty map rather than trying to open it.
//
// Before the fix this fails with:
//
//	DumboDBMerge: opening base collection "items": unexpected file ID "" for
//	collection (want DTBL or TUPM)
func TestMerge_CollectionAddedOnBothBranches(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mergeadd_%d", rand.Int64N(1_000_000))

	// main creates a new collection "items".
	mainDB := env.Client.Database(dbName)
	require.NoError(t, mainDB.Drop(ctx))
	_, err := mainDB.Collection("items").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(10)}, {Key: "side", Value: "main"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "main: add items/10", "alice <a@x.io>")

	// featureA branches from before "items" existed (main~1, the root), then adds
	// the SAME collection independently. "items" is therefore absent from the
	// merge base.
	require.NoError(t, env.Client.Database(dbName+"@main~1").RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)}, {Key: "branch", Value: "featureA"},
	}).Err())
	featDB := env.Client.Database(dbName + "@featureA")
	_, err = featDB.Collection("items").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(20)}, {Key: "side", Value: "featureA"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@featureA", "featureA: add items/20", "bob <b@x.io>")

	// Merge featureA into main. "items" was added on both sides and is absent
	// from the merge base, so its base collection hash is empty.
	var res bson.M
	err = env.Client.Database(dbName+"@main").RunCommand(ctx, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "merge_in", Value: "featureA"},
		{Key: "message", Value: "merge featureA into main"},
		{Key: "author", Value: "alice <a@x.io>"},
	}).Decode(&res)
	require.NoError(t, err, "merging a collection added on both branches must not error")
	assert.EqualValues(t, 1, res["ok"])

	// Both independently-added documents survive the merge.
	merged := env.Client.Database(dbName + "@main").Collection("items")
	require.NoError(t, merged.FindOne(ctx, bson.D{{Key: "_id", Value: int32(10)}}).Err(),
		"main's items/10 must survive")
	require.NoError(t, merged.FindOne(ctx, bson.D{{Key: "_id", Value: int32(20)}}).Err(),
		"featureA's items/20 must survive")
}
