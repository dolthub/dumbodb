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

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// dumboCommit inside a client transaction finishes the transaction first, as
// Dolt's dolt_commit does: the transaction's writes are reconciled and
// published, the commit takes them, and the client carries on in a fresh
// transaction on the new tip.
func TestDumboCommit_InTransactionCommitsTheTransaction(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("committxn%d", rand.Int64N(1_000_000))
	items := env.Client.Database(dbName).Collection("items")
	_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: "base"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "base", "alice")

	client := siClient(t, env)
	sess, err := client.StartSession()
	require.NoError(t, err)
	defer sess.EndSession(ctx)
	sc := mongo.NewSessionContext(ctx, sess)
	txnItems := client.Database(dbName).Collection("items")

	require.NoError(t, sess.StartTransaction(options.Transaction()))
	_, err = txnItems.InsertOne(sc, bson.D{{Key: "_id", Value: "in-txn"}})
	require.NoError(t, err)
	require.NoError(t, client.Database(dbName).RunCommand(sc, bson.D{
		{Key: "dumboCommit", Value: int32(1)}, {Key: "message", Value: "from the transaction"},
	}).Err())

	n, err := items.CountDocuments(ctx, bson.D{{Key: "_id", Value: "in-txn"}})
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "dumboCommit must publish the transaction's writes")
	require.Empty(t, runStatus(t, env, dbName).Tables, "dumboCommit must commit the transaction's writes")

	_, err = txnItems.InsertOne(sc, bson.D{{Key: "_id", Value: "after"}})
	require.NoError(t, err)
	require.NoError(t, sess.CommitTransaction(ctx), "the client carries on in a fresh transaction")

	n, err = items.CountDocuments(ctx, bson.D{{Key: "_id", Value: "after"}})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.NotEmpty(t, runStatus(t, env, dbName).Tables, "a write after dumboCommit stays uncommitted")
}

// A dumboCommit whose transaction cannot be reconciled is refused the way
// commitTransaction is: the transaction is aborted with a WriteConflict the
// driver retries, and no paused operation is left behind.
func TestDumboCommit_InTransactionRefusedLikeCommitTransaction(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("refusetxn%d", rand.Int64N(1_000_000))
	items := env.Client.Database(dbName).Collection("items")
	_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: "base"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "base", "alice")

	client := siClient(t, env)
	sess, err := client.StartSession()
	require.NoError(t, err)
	defer sess.EndSession(ctx)
	sc := mongo.NewSessionContext(ctx, sess)

	require.NoError(t, sess.StartTransaction(options.Transaction()))
	_, err = client.Database(dbName).Collection("items").InsertOne(sc, bson.D{{Key: "_id", Value: "x"}, {Key: "v", Value: int32(1)}})
	require.NoError(t, err)
	_, err = items.InsertOne(ctx, bson.D{{Key: "_id", Value: "x"}, {Key: "v", Value: int32(2)}})
	require.NoError(t, err)

	err = client.Database(dbName).RunCommand(sc, bson.D{
		{Key: "dumboCommit", Value: int32(1)}, {Key: "message", Value: "conflicting"},
	}).Err()
	var cmdErr mongo.CommandError
	require.True(t, errors.As(err, &cmdErr), "expected a command error, got %v", err)
	require.EqualValues(t, 112, cmdErr.Code, "expected WriteConflict, got %v", err)
	require.True(t, cmdErr.HasErrorLabel("TransientTransactionError"), "the driver must be told to retry the transaction: %v", err)

	var status bson.M
	require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{{Key: "doltStatus", Value: int32(1)}}).Decode(&status))
	require.NotContains(t, status, "mergeState", "a refused dumboCommit must not leave an operation paused")

	var stored bson.M
	require.NoError(t, items.FindOne(ctx, bson.D{{Key: "_id", Value: "x"}}).Decode(&stored))
	require.EqualValues(t, 2, stored["v"])
}
