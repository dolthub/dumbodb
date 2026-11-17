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

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// collectionUUID is the UUID listCollections reports for db.coll.
func collectionUUID(t *testing.T, db *mongo.Database, coll string) string {
	t.Helper()
	specs, err := db.ListCollectionSpecifications(context.Background(), bson.D{{Key: "name", Value: coll}})
	require.NoError(t, err)
	require.Len(t, specs, 1)
	require.NotNil(t, specs[0].UUID)
	id, err := uuid.FromBytes(specs[0].UUID.Data)
	require.NoError(t, err)
	return id.String()
}

func requireQueryPlanKilled(t *testing.T, err error, collUUID string) {
	t.Helper()
	var ce mongo.CommandError
	require.True(t, errors.As(err, &ce), "want a command error, got %v", err)
	require.EqualValues(t, 175, ce.Code, "want QueryPlanKilled, got %v", err)
	require.Contains(t, ce.Message, "collection dropped. UUID "+collUUID)
}

// As in mongod 8.0, a getMore on a cursor whose database was dropped fails
// with QueryPlanKilled naming the collection's UUID (dolthub/dumbodb#341).
func TestGetMoreAfterDropDatabaseIsQueryPlanKilled(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	db := env.Client.Database(fmt.Sprintf("dropcursor_%d", rand.Int64N(1_000_000)))
	docs := make([]any, 10)
	for i := range docs {
		docs[i] = bson.D{{Key: "_id", Value: i}}
	}
	_, err := db.Collection("c").InsertMany(ctx, docs)
	require.NoError(t, err)
	collUUID := collectionUUID(t, db, "c")

	cur, err := db.Collection("c").Find(ctx, bson.D{}, options.Find().SetBatchSize(2))
	require.NoError(t, err)
	t.Cleanup(func() { cur.Close(context.Background()) }) //nolint:errcheck
	for range 2 {
		require.True(t, cur.Next(ctx))
	}

	require.NoError(t, newClient(t, env).Database(db.Name()).Drop(ctx))

	for cur.Next(ctx) {
	}
	requireQueryPlanKilled(t, cur.Err(), collUUID)
}

// As in mongod 8.0, dropDatabase interrupts a command running on the database
// rather than waiting for it: the command fails with QueryPlanKilled and the
// drop returns promptly (dolthub/dumbodb#344).
func TestDropDatabaseInterruptsRunningAggregation(t *testing.T) {
	const docs = 60_000

	env := startDumboDB(t)
	ctx := context.Background()
	db := env.Client.Database(fmt.Sprintf("dropagg_%d", rand.Int64N(1_000_000)))
	pad := string(make([]byte, 200))
	for b := 0; b < docs/5_000; b++ {
		batch := make([]any, 5_000)
		for i := range batch {
			id := b*5_000 + i
			batch[i] = bson.D{{Key: "_id", Value: id}, {Key: "g", Value: id % 97}, {Key: "pad", Value: pad}}
		}
		_, err := db.Collection("c").InsertMany(ctx, batch)
		require.NoError(t, err)
	}
	collUUID := collectionUUID(t, db, "c")

	pipeline := mongo.Pipeline{
		{{Key: "$sort", Value: bson.D{{Key: "pad", Value: 1}, {Key: "g", Value: -1}}}},
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$g"}, {Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
	}
	start := time.Now()
	_, err := db.Collection("c").Aggregate(ctx, pipeline)
	require.NoError(t, err)
	unhindered := time.Since(start)

	aggDone := make(chan error, 1)
	go func() {
		cur, err := newClient(t, env).Database(db.Name()).Collection("c").Aggregate(ctx, pipeline)
		if err == nil {
			err = cur.All(ctx, &[]bson.M{})
		}
		aggDone <- err
	}()
	time.Sleep(unhindered / 4)

	dropStart := time.Now()
	require.NoError(t, newClient(t, env).Database(db.Name()).Drop(ctx))
	dropTook := time.Since(dropStart)

	requireQueryPlanKilled(t, <-aggDone, collUUID)
	require.Less(t, dropTook, unhindered/2,
		"the drop must interrupt the aggregation (alone it takes %s), not wait for it", unhindered)
}
