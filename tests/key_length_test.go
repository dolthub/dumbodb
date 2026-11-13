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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	idxpkg "github.com/dolthub/dumbodb/internal/index"
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

// TestKeyLength checks that keys over DumboDB's 1024-byte limit are refused
// with KeyTooLong on every write path, keep the connection open, and leave
// lookups by such a key returning nothing.
func TestKeyLength(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.Collection(t)
	long := strings.Repeat("x", 3000)
	fits := strings.Repeat("y", 1000)

	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: long}})
	requireKeyTooLong(t, err)

	_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: fits}})
	require.NoError(t, err, "a key under the limit is stored")

	n, err := coll.CountDocuments(ctx, bson.D{{Key: "_id", Value: long}})
	require.NoError(t, err)
	require.Zero(t, n)
	// Range bounds over the limit still order correctly against stored keys.
	above := strings.Repeat("z", 3000)
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

// TestKeyLength_ManyKeysAtLimit inserts enough keys at the length limit to
// grow the primary and secondary trees deep, which overflowed Dolt's 15-level
// limit when the cap was 4096 bytes.
func TestKeyLength_ManyKeysAtLimit(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	const counterWidth = 8
	// A string's KeyString adds a type byte and a terminator.
	pad := strings.Repeat("x", idxpkg.MaxKeyLength-2-counterWidth)

	t.Run("primary", func(t *testing.T) {
		insertManyKeys(t, env.Collection(t), func(counter string, _ int) bson.D {
			return bson.D{{Key: "_id", Value: counter + pad}}
		})
	})

	t.Run("secondary", func(t *testing.T) {
		coll := env.Collection(t)
		_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "a", Value: 1}}})
		require.NoError(t, err)
		// The entry also holds the int32 _id's KeyString.
		fieldPad := pad[:len(pad)-8]
		insertManyKeys(t, coll, func(counter string, i int) bson.D {
			return bson.D{{Key: "_id", Value: int32(i)}, {Key: "a", Value: counter + fieldPad}}
		})
	})
}

func insertManyKeys(t *testing.T, coll *mongo.Collection, makeDoc func(counter string, i int) bson.D) {
	t.Helper()
	ctx := context.Background()
	const total, batch = 30000, 500
	for start := 0; start < total; start += batch {
		docs := make([]any, 0, batch)
		for i := start; i < start+batch; i++ {
			docs = append(docs, makeDoc(fmt.Sprintf("%08d", i), i))
		}
		_, err := coll.InsertMany(ctx, docs)
		require.NoError(t, err, "batch starting at %d", start)
	}
	n, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	require.Equal(t, int64(total), n)
}
