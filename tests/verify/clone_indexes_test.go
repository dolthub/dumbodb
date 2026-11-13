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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestCloneCarriesSecondaryIndexesAcrossServers: a database pushed from one
// dumbodb server and cloned by another, each with its own data directory,
// keeps its secondary index: listIndexes shows it and a hinted find reads it,
// before and after a full GC on the clone (#190).
func TestCloneCarriesSecondaryIndexesAcrossServers(t *testing.T) {
	ctx := context.Background()
	source := startDumboDB(t)
	target := startDumboDB(t)
	remoteURL := "file://" + t.TempDir()

	src := source.Client.Database("idxsrc")
	docs := make([]any, 20)
	for i := range docs {
		docs[i] = bson.D{{Key: "_id", Value: int32(i)}, {Key: "a", Value: int32(i % 5)}}
	}
	_, err := src.Collection("items").InsertMany(ctx, docs)
	require.NoError(t, err)
	_, err = src.Collection("items").Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "a", Value: 1}}, Options: options.Index().SetName("a_1"),
	})
	require.NoError(t, err)
	dumboDBCommit(t, source, "idxsrc", "seed with index", "alice <alice@acme.com>")

	var res bson.M
	require.NoError(t, src.RunCommand(ctx, bson.D{
		{Key: "dumboRemote", Value: int32(1)}, {Key: "action", Value: "add"},
		{Key: "name", Value: "origin"}, {Key: "url", Value: remoteURL},
	}).Decode(&res))
	require.NoError(t, src.RunCommand(ctx, bson.D{
		{Key: "dumboPush", Value: int32(1)}, {Key: "to", Value: "origin"}, {Key: "refSpec", Value: "main"},
	}).Decode(&res))

	require.NoError(t, target.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "dumboClone", Value: int32(1)}, {Key: "from", Value: remoteURL}, {Key: "as", Value: "idxclone"},
	}).Decode(&res))

	clone := target.Client.Database("idxclone")
	assertIndexServesHintedFind(t, ctx, clone)

	require.NoError(t, clone.RunCommand(ctx, bson.D{{Key: "dumboGC", Value: int32(1)}, {Key: "mode", Value: "full"}}).Decode(&res))
	assertIndexServesHintedFind(t, ctx, clone)
}

// assertIndexServesHintedFind checks that db.items lists index a_1 and that a
// find hinted to it returns the 4 documents with a: 2 from 4 index keys.
func assertIndexServesHintedFind(t *testing.T, ctx context.Context, db *mongo.Database) {
	t.Helper()
	coll := db.Collection("items")

	specs, err := coll.Indexes().ListSpecifications(ctx)
	require.NoError(t, err)
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
	}
	assert.Contains(t, names, "a_1")

	filter := bson.D{{Key: "a", Value: int32(2)}}
	cur, err := coll.Find(ctx, filter, options.Find().SetHint("a_1").SetSort(bson.D{{Key: "_id", Value: 1}}))
	require.NoError(t, err)
	var found []bson.M
	require.NoError(t, cur.All(ctx, &found))
	var ids []int32
	for _, d := range found {
		ids = append(ids, d["_id"].(int32))
	}
	assert.Equal(t, []int32{2, 7, 12, 17}, ids)

	var explain bson.M
	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{{Key: "find", Value: "items"}, {Key: "filter", Value: filter}, {Key: "hint", Value: "a_1"}}},
		{Key: "verbosity", Value: "executionStats"},
	}).Decode(&explain))
	stats, _ := explain["executionStats"].(bson.M)
	require.NotNil(t, stats)
	assert.EqualValues(t, 4, stats["totalKeysExamined"])
	assert.EqualValues(t, 4, stats["nReturned"])
}
