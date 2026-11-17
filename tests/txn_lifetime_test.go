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
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// As in mongod, a transaction open longer than transactionLifetimeLimitSeconds
// is aborted by the server: its locks are released, so a write it was holding
// up proceeds, and its later statements and its commit fail with
// NoSuchTransaction.
func TestTransactionLifetimeLimitAbortsExpiredTransaction(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	admin := env.Client.Database("admin")
	require.NoError(t, admin.RunCommand(ctx, bson.D{
		{Key: "setParameter", Value: 1}, {Key: "transactionLifetimeLimitSeconds", Value: 1},
	}).Err())

	dbName := fmt.Sprintf("txnlife_%d", rand.Int64N(1_000_000))
	items := env.Client.Database(dbName).Collection("items")
	_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "v", Value: 0}})
	require.NoError(t, err)

	txnClient := newClient(t, env)
	sess, err := txnClient.StartSession()
	require.NoError(t, err)
	t.Cleanup(func() { sess.EndSession(context.Background()) })
	require.NoError(t, sess.StartTransaction())
	sessCtx := mongo.NewSessionContext(ctx, sess)
	txnItems := txnClient.Database(dbName).Collection("items")
	_, err = txnItems.UpdateOne(sessCtx, bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: 1}}}})
	require.NoError(t, err)

	// A plain write to the locked document waits for the transaction, which
	// the server aborts once it outlives the limit.
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err = newClient(t, env).Database(dbName).Collection("items").
		UpdateOne(writeCtx, bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: 2}}}})
	require.NoError(t, err, "the plain write proceeds once the expired transaction is aborted")
	require.Less(t, time.Since(start), 5*time.Second)

	_, err = txnItems.InsertOne(sessCtx, bson.D{{Key: "_id", Value: 2}})
	requireNoSuchTransaction(t, err)
	requireNoSuchTransaction(t, sess.CommitTransaction(ctx))

	var stored struct {
		V int32 `bson:"v"`
	}
	require.NoError(t, items.FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&stored))
	require.EqualValues(t, 2, stored.V, "the aborted transaction's write is gone")
}

func requireNoSuchTransaction(t *testing.T, err error) {
	t.Helper()
	var ce mongo.CommandError
	require.True(t, errors.As(err, &ce), "want a command error, got %v", err)
	require.EqualValues(t, 251, ce.Code, "want NoSuchTransaction, got %v", err)
}
