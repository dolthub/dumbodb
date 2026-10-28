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

// A transaction belongs to its session, not to the connection it ran on. A
// plain write sent on a connection another session's open transaction has
// used is not part of that transaction: it is published when it returns, as
// any plain write is. It used to be held back as if it were, and was never
// published at all once the transaction finished on another connection --
// acknowledged, and lost.
func TestPlainWriteOnAConnectionATransactionUsedIsPublished(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("txnflag%d", rand.Int64N(1_000_000))
	uri := fmt.Sprintf("mongodb://127.0.0.1:%d", env.Port)
	oneConn, err := mongo.Connect(options.Client().ApplyURI(uri).SetMaxPoolSize(1))
	require.NoError(t, err)
	t.Cleanup(func() { _ = oneConn.Disconnect(ctx) })
	items := oneConn.Database(dbName).Collection("items")
	_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: "seed"}})
	require.NoError(t, err)

	sess, err := oneConn.StartSession()
	require.NoError(t, err)
	defer sess.EndSession(ctx)
	require.NoError(t, sess.StartTransaction(options.Transaction()))
	_, err = items.InsertOne(mongo.NewSessionContext(ctx, sess), bson.D{{Key: "_id", Value: "in-txn"}})
	require.NoError(t, err)

	_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: "plain"}})
	require.NoError(t, err)
	n, err := env.Client.Database(dbName).Collection("items").CountDocuments(ctx, bson.D{{Key: "_id", Value: "plain"}})
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "an acknowledged plain write was not published")

	require.NoError(t, sess.CommitTransaction(ctx))
	n, err = env.Client.Database(dbName).Collection("items").CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	require.EqualValues(t, 3, n)
}
