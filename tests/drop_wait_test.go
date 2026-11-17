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
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A client holding a command open on its own database, here a write parked
// behind its own open transaction, must not hold up drops of other databases.
// A drop of its own database interrupts the parked write and, like mongod's,
// waits for the open transaction: within maxTimeMS it fails, and once the
// transaction ends it succeeds.
func TestDropDatabaseIsNotHeldByCommandsElsewhere(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	suffix := rand.Int64()
	attackerDB := env.Client.Database(fmt.Sprintf("testdb_drop_wait_a_%d", suffix))
	victimDB := env.Client.Database(fmt.Sprintf("testdb_drop_wait_v_%d", suffix))
	t.Cleanup(func() { attackerDB.Drop(context.Background()) }) //nolint:errcheck
	t.Cleanup(func() { victimDB.Drop(context.Background()) })   //nolint:errcheck

	_, err := attackerDB.Collection("c").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "v", Value: 0}})
	require.NoError(t, err)
	_, err = victimDB.Collection("x").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}})
	require.NoError(t, err)

	connect := func() *mongo.Client {
		c, err := mongo.Connect(options.Client().ApplyURI(fmt.Sprintf("mongodb://127.0.0.1:%d/", env.Port)))
		require.NoError(t, err)
		t.Cleanup(func() { c.Disconnect(context.Background()) }) //nolint:errcheck
		return c
	}

	holder := connect()
	sess, err := holder.StartSession()
	require.NoError(t, err)
	t.Cleanup(func() { sess.EndSession(context.Background()) })
	require.NoError(t, sess.StartTransaction())
	sessCtx := mongo.NewSessionContext(ctx, sess)
	_, err = holder.Database(attackerDB.Name()).Collection("c").
		UpdateOne(sessCtx, bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: 1}}}})
	require.NoError(t, err)
	t.Cleanup(func() { sess.AbortTransaction(context.Background()) }) //nolint:errcheck

	parkedDone := make(chan error, 1)
	go func() {
		parkedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, err := connect().Database(attackerDB.Name()).Collection("c").
			UpdateOne(parkedCtx, bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: 2}}}})
		parkedDone <- err
	}()
	time.Sleep(500 * time.Millisecond) // let the plain write park on the transaction's lock

	admin := connect()
	start := time.Now()
	err = admin.Database(victimDB.Name()).RunCommand(ctx, bson.D{{Key: "dropDatabase", Value: 1}, {Key: "maxTimeMS", Value: 1000}}).Err()
	require.NoError(t, err)
	require.Less(t, time.Since(start), time.Second, "a drop waited for a command on another database")

	err = admin.Database(attackerDB.Name()).RunCommand(ctx, bson.D{{Key: "dropDatabase", Value: 1}, {Key: "maxTimeMS", Value: 1000}}).Err()
	var ce mongo.CommandError
	require.ErrorAs(t, err, &ce, "a drop must wait for the open transaction on its database")
	require.EqualValues(t, 50, ce.Code, "want MaxTimeMSExpired, got %v", err)

	select {
	case err := <-parkedDone:
		require.Error(t, err, "the parked write must be interrupted by the drop")
	case <-time.After(10 * time.Second):
		t.Fatal("the parked write was not interrupted by the drop")
	}

	require.NoError(t, sess.AbortTransaction(ctx))
	err = admin.Database(attackerDB.Name()).RunCommand(ctx, bson.D{{Key: "dropDatabase", Value: 1}, {Key: "maxTimeMS", Value: 5000}}).Err()
	require.NoError(t, err, "the drop succeeds once the transaction has ended")
}
