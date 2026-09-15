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

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// One lsid on several connections must keep working. MongoDB drivers pool
// logical sessions independently of connections, so an implicit session is
// handed to whichever connection needs it and one lsid reaches many
// connections over the life of a client. That is ordinary, not a takeover.
//
// Driving one explicit session from several goroutines is the stand-in: it
// puts one lsid on several connections at once, which is the same server-side
// condition a pooled implicit session produces over time, and it produces it
// reliably instead of waiting on pool timing.
//
// Before the fix this failed 302 of 320 operations with code 225, "session was
// taken over by a newer connection on this lsid".
func TestSession_OneLsidAcrossManyConnections(t *testing.T) {
	const workers, perWorker = 8, 40

	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("lsid_%d", rand.Int64N(1_000_000))

	client := siClient(t, env)
	sess, err := client.StartSession()
	require.NoError(t, err)
	defer sess.EndSession(ctx)

	coll := client.Database(dbName).Collection("docs")
	failures := make([][]error, workers)

	var writers sync.WaitGroup
	for w := 0; w < workers; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			_ = mongo.WithSession(ctx, sess, func(sc context.Context) error {
				for i := 0; i < perWorker; i++ {
					if _, err := coll.InsertOne(sc, bson.D{
						{Key: "_id", Value: w*1000 + i},
					}); err != nil {
						failures[w] = append(failures[w], err)
					}
				}
				return nil
			})
		}(w)
	}
	writers.Wait()

	failed := 0
	var first error
	for _, errs := range failures {
		for _, err := range errs {
			if first == nil {
				first = err
			}
			failed++
		}
	}
	require.Zero(t, failed,
		"%d of %d writes failed on a shared lsid; first: %v", failed, workers*perWorker, first)

	count, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	require.EqualValues(t, workers*perWorker, count, "every acknowledged insert must be stored")
}
