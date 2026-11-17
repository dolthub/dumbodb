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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// raceDropAllowedCodes are the errors mongod gives an operation that overlaps
// a drop of its database.
var raceDropAllowedCodes = map[int32]string{
	26:  "NamespaceNotFound",
	112: "WriteConflict",
	175: "QueryPlanKilled",
	215: "DatabaseDropPending",
	251: "NoSuchTransaction",
}

// Operations overlapping a dropDatabase fail only the ways mongod's do, never
// with an internal error from the store the drop closed
// (dolthub/dumbodb#320).
func TestOperationsRacingDropDatabaseFailCleanly(t *testing.T) {
	const runFor = 4 * time.Second

	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("dropchurn_%d", rand.Int64N(1_000_000))

	var (
		mu         sync.Mutex
		unexpected = map[string]error{}
		ops        atomic.Int64
	)
	record := func(kind string, err error) {
		ops.Add(1)
		if err == nil {
			return
		}
		var ce mongo.CommandError
		var we mongo.WriteException
		switch {
		case errors.As(err, &we) && len(we.WriteErrors) > 0:
			if _, ok := raceDropAllowedCodes[int32(we.WriteErrors[0].Code)]; ok {
				return
			}
		case errors.As(err, &we) && we.WriteConcernError != nil:
		case errors.As(err, &ce):
			if _, ok := raceDropAllowedCodes[ce.Code]; ok {
				return
			}
		}
		mu.Lock()
		if _, seen := unexpected[kind]; !seen {
			unexpected[kind] = err
		}
		mu.Unlock()
	}

	deadline := time.Now().Add(runFor)
	var wg sync.WaitGroup
	loop := func(step func(c *mongo.Client, i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := newClient(t, env)
			for i := 0; time.Now().Before(deadline); i++ {
				step(c, i)
			}
		}()
	}

	loop(func(c *mongo.Client, _ int) {
		record("dropDatabase", c.Database(dbName).Drop(ctx))
		time.Sleep(2 * time.Millisecond)
	})
	for w := 0; w < 3; w++ {
		loop(func(c *mongo.Client, i int) {
			_, err := c.Database(dbName).Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: fmt.Sprintf("w%d-%d", w, i)}, {Key: "v", Value: i}})
			record("insert", err)
		})
	}
	loop(func(c *mongo.Client, i int) {
		_, err := c.Database(dbName).Collection("items").UpdateMany(ctx, bson.D{}, bson.D{{Key: "$inc", Value: bson.D{{Key: "v", Value: 1}}}})
		record("update", err)
	})
	loop(func(c *mongo.Client, i int) {
		_, err := c.Database(dbName).Collection("items").DeleteMany(ctx, bson.D{{Key: "v", Value: bson.D{{Key: "$lt", Value: 0}}}})
		record("delete", err)
	})
	loop(func(c *mongo.Client, i int) {
		_, err := c.Database(dbName).Collection("items").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "v", Value: 1}}})
		record("createIndexes", err)
	})
	loop(func(c *mongo.Client, i int) {
		cur, err := c.Database(dbName).Collection("items").Find(ctx, bson.D{})
		if err == nil {
			err = cur.All(ctx, &[]bson.M{})
		}
		record("find", err)
	})
	for w := 0; w < 2; w++ {
		loop(func(c *mongo.Client, i int) {
			sess, err := c.StartSession()
			require.NoError(t, err)
			defer sess.EndSession(ctx)
			require.NoError(t, sess.StartTransaction())
			sc := mongo.NewSessionContext(ctx, sess)
			_, err = c.Database(dbName).Collection("items").InsertOne(sc, bson.D{{Key: "_id", Value: fmt.Sprintf("t%d-%d", w, i)}})
			record("insert in transaction", err)
			if err != nil {
				_ = sess.AbortTransaction(ctx)
				return
			}
			record("commitTransaction", sess.CommitTransaction(ctx))
		})
	}
	wg.Wait()

	require.Greater(t, ops.Load(), int64(50), "too few operations to exercise the race")
	for kind, err := range unexpected {
		t.Errorf("%s racing dropDatabase failed with an error mongod does not give: %v", kind, err)
	}
}
