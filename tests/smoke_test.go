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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

func TestTransactionSmoke(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	sess, err := env.client.StartSession()
	require.NoError(t, err, "StartSession must succeed")
	defer sess.EndSession(ctx)

	_, err = sess.WithTransaction(ctx, func(sessCtx context.Context) (interface{}, error) {
		_, insErr := coll.InsertOne(sessCtx, bson.D{
			{Key: "_id", Value: "smoke-commit"},
			{Key: "n", Value: int32(1)},
		})
		return nil, insErr
	}, options.Transaction().SetReadPreference(readpref.Primary()))
	require.NoError(t, err, "WithTransaction must succeed now that transactions are implemented")

	var got bson.M
	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "_id", Value: "smoke-commit"}}).Decode(&got))
	assert.EqualValues(t, 1, got["n"])
}

func TestTransactionSmoke_Abort(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	sess, err := env.client.StartSession()
	require.NoError(t, err, "StartSession must succeed")
	defer sess.EndSession(ctx)

	require.NoError(t, sess.StartTransaction(), "StartTransaction must succeed")

	mongoCtx := mongo.NewSessionContext(ctx, sess)
	_, _ = coll.InsertOne(mongoCtx, bson.D{{Key: "_id", Value: "smoke-abort"}, {Key: "n", Value: int32(2)}})

	require.NoError(t, sess.AbortTransaction(ctx))

	err = coll.FindOne(ctx, bson.D{{Key: "_id", Value: "smoke-abort"}}).Err()
	require.ErrorIs(t, err, mongo.ErrNoDocuments, "abort must discard the uncommitted write")
}
