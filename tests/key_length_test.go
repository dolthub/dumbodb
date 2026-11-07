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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

const keyTooLongCode = 17280

func requireKeyTooLong(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var we mongo.WriteException
	if errors.As(err, &we) {
		require.Len(t, we.WriteErrors, 1)
		require.Equal(t, keyTooLongCode, we.WriteErrors[0].Code, we.WriteErrors[0].Message)
		return
	}
	var ce mongo.CommandError
	require.True(t, errors.As(err, &ce), "unexpected error %T: %v", err, err)
	require.Equal(t, int32(keyTooLongCode), ce.Code, ce.Message)
}

// TestKeyLength checks that keys over DumboDB's 4096-byte limit are refused
// with KeyTooLong on every write path, keep the connection open, and leave
// lookups by such a key returning nothing.
func TestKeyLength(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.Collection(t)
	long := strings.Repeat("x", 5000)
	fits := strings.Repeat("y", 4000)

	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: long}})
	requireKeyTooLong(t, err)

	_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: fits}})
	require.NoError(t, err, "a key under the limit is stored")

	n, err := coll.CountDocuments(ctx, bson.D{{Key: "_id", Value: long}})
	require.NoError(t, err)
	require.Zero(t, n)
	// Range bounds over the limit still order correctly against stored keys.
	above := strings.Repeat("z", 5000)
	n, err = coll.CountDocuments(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$gte", Value: above}}}})
	require.NoError(t, err)
	require.Zero(t, n)
	n, err = coll.CountDocuments(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$lt", Value: above}}}})
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	_, err = coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "f", Value: 1}}})
	require.NoError(t, err)
	_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "f", Value: long}})
	requireKeyTooLong(t, err)

	_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 2}, {Key: "f", Value: "short"}})
	require.NoError(t, err)
	_, err = coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: 2}}, bson.D{{Key: "$set", Value: bson.D{{Key: "f", Value: long}}}})
	requireKeyTooLong(t, err)

	_, err = coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: long}}, bson.D{{Key: "$set", Value: bson.D{{Key: "g", Value: 1}}}})
	require.NoError(t, err, "an update whose filter names a long _id matches nothing")

	_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 3}, {Key: "g", Value: long}})
	require.NoError(t, err)
	_, err = coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "g", Value: 1}}})
	requireKeyTooLong(t, err)

	n, err = coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	require.Equal(t, int64(3), n, "refused writes change nothing")
}
