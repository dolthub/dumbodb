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
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Connections that each insert and update only their own documents must leave
// an index that matches the collection: a hinted find returns what a
// collection scan returns, with no duplicates (dolthub/dumbodb#361).
func TestConcurrentDisjointUpdatesKeepIndexInStep(t *testing.T) {
	const (
		workers = 4
		runFor  = 3 * time.Second
		batch   = 50
	)

	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("idxupd_%d", rand.Int64N(1_000_000))
	coll := env.Client.Database(dbName).Collection("items")
	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "v", Value: 1}}})
	require.NoError(t, err)

	deadline := time.Now().Add(runFor)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			items := newClient(t, env).Database(dbName).Collection("items")
			for round := 0; time.Now().Before(deadline); round++ {
				docs := make([]any, batch)
				ids := make(bson.A, batch)
				for i := range docs {
					id := fmt.Sprintf("w%d-r%d-%d", w, round, i)
					docs[i] = bson.D{{Key: "_id", Value: id}, {Key: "v", Value: int32(0)}}
					ids[i] = id
				}
				_, err := items.InsertMany(ctx, docs)
				require.NoError(t, err)
				_, err = items.UpdateMany(ctx,
					bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}},
					bson.D{{Key: "$inc", Value: bson.D{{Key: "v", Value: int32(1)}}}})
				require.NoError(t, err)
			}
		}(w)
	}
	wg.Wait()

	count := func(hint any) int {
		cur, err := coll.Find(ctx, bson.D{{Key: "v", Value: bson.D{{Key: "$gte", Value: 0}}}}, options.Find().SetHint(hint))
		require.NoError(t, err)
		var docs []bson.M
		require.NoError(t, cur.All(ctx, &docs))
		return len(docs)
	}
	scanned := count(bson.D{{Key: "$natural", Value: 1}})
	require.NotZero(t, scanned)
	require.Equal(t, scanned, count("v_1"), "the hinted find must return each document once")
}
