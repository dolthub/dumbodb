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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const codeBSONObjectTooLarge = 10334

// Before the fix dumboDiff, and dumboLog with patch, built every changed
// document of every collection (and every commit) into one reply with no
// bound, so a large change set exhausted server memory. Replies are now
// capped at the 16MB BSON limit, failing with BSONObjectTooLarge as MongoDB
// does for oversized command replies.
func TestVersioningRepliesAreSizeBounded(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	db := env.Client.Database("bigdiff")
	coll := db.Collection("blobs")

	_, err := db.Collection("seed").InsertOne(ctx, bson.D{{Key: "_id", Value: 0}})
	require.NoError(t, err)
	require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "dumboCommit", Value: 1}, {Key: "message", Value: "seed"}}).Err())

	blob := strings.Repeat("x", 100*1024)
	docs := make([]any, 0, 300)
	for i := range 300 {
		docs = append(docs, bson.D{{Key: "_id", Value: i}, {Key: "blob", Value: blob}})
	}
	_, err = coll.InsertMany(ctx, docs)
	require.NoError(t, err)

	requireCode(t, db.RunCommand(ctx, bson.D{{Key: "dumboDiff", Value: 1}}).Err(), codeBSONObjectTooLarge)

	require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "dumboCommit", Value: 1}, {Key: "message", Value: "big"}}).Err())
	requireCode(t, db.RunCommand(ctx, bson.D{{Key: "dumboLog", Value: 1}, {Key: "patch", Value: true}}).Err(), codeBSONObjectTooLarge)
	require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "dumboLog", Value: 1}, {Key: "stat", Value: true}}).Err(), "summary output stays small")

	small := db.Collection("small")
	_, err = small.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}})
	require.NoError(t, err)
	var res bson.M
	require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "dumboDiff", Value: 1}}).Decode(&res), "a small diff still works")
	require.NotEmpty(t, res["changes"])
}
