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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// A command that may not run in a transaction aborts the transaction. The
// abort rolls back the session's state, so it must happen inside the session's
// command bracket; otherwise a GC safepoint treats the session as idle and
// visits its roots while the rollback is changing them (dolthub/dumbodb#347).
// The race shows only under -race, which also builds the server with -race.
func TestTransactionAbortedByBlockedCommandRacingGC(t *testing.T) {
	const runFor = 5 * time.Second

	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("gcabort_%d", rand.Int64N(1_000_000))
	_, err := env.Client.Database(dbName).Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: "seed"}})
	require.NoError(t, err)

	deadline := time.Now().Add(runFor)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		gcClient := newClient(t, env)
		for time.Now().Before(deadline) {
			err := gcClient.Database(dbName).RunCommand(ctx, bson.D{{Key: "dumboGC", Value: 1}}).Err()
			require.NoError(t, err)
		}
	}()

	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			client := newClient(t, env)
			items := client.Database(dbName).Collection("items")
			for i := 0; time.Now().Before(deadline); i++ {
				sess, err := client.StartSession()
				require.NoError(t, err)
				require.NoError(t, sess.StartTransaction())
				sessCtx := mongo.NewSessionContext(ctx, sess)
				_, err = items.InsertOne(sessCtx, bson.D{{Key: "_id", Value: fmt.Sprintf("w%d-%d", w, i)}})
				require.NoError(t, err)
				// createIndexes may not run in a transaction, so the server
				// aborts the transaction while answering it.
				err = client.Database(dbName).RunCommand(sessCtx, bson.D{
					{Key: "createIndexes", Value: "items"},
					{Key: "indexes", Value: bson.A{bson.D{{Key: "key", Value: bson.D{{Key: "v", Value: 1}}}, {Key: "name", Value: "v_1"}}}},
				}).Err()
				require.Error(t, err)
				_ = sess.AbortTransaction(ctx)
				sess.EndSession(ctx)
			}
		}(w)
	}
	wg.Wait()
}
