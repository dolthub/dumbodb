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
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// As in mongod 8.0, dropDatabase waits for an open transaction that wrote to
// the database: the transaction's commit succeeds, and then the drop completes
// (dolthub/dumbodb#334).
func TestDropDatabaseWaitsForOpenTransaction(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("droptxn_%d", rand.Int64N(1_000_000))
	items := env.Client.Database(dbName).Collection("items")
	_, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: 0}})
	require.NoError(t, err)

	txnClient := newClient(t, env)
	sess, err := txnClient.StartSession()
	require.NoError(t, err)
	t.Cleanup(func() { sess.EndSession(context.Background()) })
	require.NoError(t, sess.StartTransaction())
	sessCtx := mongo.NewSessionContext(ctx, sess)
	_, err = txnClient.Database(dbName).Collection("items").InsertOne(sessCtx, bson.D{{Key: "_id", Value: 1}})
	require.NoError(t, err)

	dropped := make(chan error, 1)
	go func() {
		dropped <- newClient(t, env).Database(dbName).RunCommand(ctx, bson.D{{Key: "dropDatabase", Value: 1}}).Err()
	}()
	select {
	case err := <-dropped:
		t.Fatalf("dropDatabase returned (%v) while a transaction on the database was open", err)
	case <-time.After(2 * time.Second):
	}

	require.NoError(t, sess.CommitTransaction(ctx), "the transaction commits before the drop")
	select {
	case err := <-dropped:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("dropDatabase did not finish after the transaction committed")
	}
	names, err := env.Client.ListDatabaseNames(ctx, bson.D{{Key: "name", Value: dbName}})
	require.NoError(t, err)
	require.Empty(t, names, "the database is gone after the drop")
}
