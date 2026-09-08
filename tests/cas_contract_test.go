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

// A failed compare-and-swap is not an error. MongoDB answers {ok: 1, n: 0,
// nModified: 0} and the client branches on matchedCount, so a refusal has to
// arrive that way whether the filter simply did not match or the merge at the
// write's boundary turned the write down.
//
// The assertion is conservation, and it does not depend on the two writers
// actually racing. If they miss each other, the second one's filter no longer
// matches and it is told n:0. If they collide, the boundary refuses one and it
// is told n:0. Either way exactly one increment is acknowledged per round and
// the stored counter must equal the number of acknowledged increments. Two
// writers both told n:1 for one increment is the failure this catches, and it
// is what the server did before a merge mode governed the boundary.
func TestCAS_FailedCompareAndSwapIsNotAnError(t *testing.T) {
	const rounds = 40

	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("cas_%d", rand.Int64N(1_000_000))

	clientA, clientB := siClient(t, env), siClient(t, env)
	collOf := func(c *mongo.Client) *mongo.Collection {
		return c.Database(dbName).Collection("documents")
	}

	id := bson.D{{Key: "_id", Value: "counter"}}
	_, err := collOf(clientA).InsertOne(ctx,
		bson.D{{Key: "_id", Value: "counter"}, {Key: "v", Value: int32(0)}})
	require.NoError(t, err)

	readVersion := func() int32 {
		var stored struct {
			V int32 `bson:"v"`
		}
		require.NoError(t, collOf(clientA).FindOne(ctx, id).Decode(&stored))
		return stored.V
	}

	acknowledged := int32(0)
	for round := 0; round < rounds; round++ {
		observed := readVersion()
		cas := bson.D{
			{Key: "_id", Value: "counter"},
			{Key: "v", Value: observed},
		}
		inc := bson.D{{Key: "$inc", Value: bson.D{{Key: "v", Value: int32(1)}}}}

		var matched [2]int64
		var failure [2]error
		var writers sync.WaitGroup
		start := make(chan struct{})

		attempt := func(i int, c *mongo.Client) {
			defer writers.Done()
			<-start
			result, err := collOf(c).UpdateOne(ctx, cas, inc)
			if err != nil {
				failure[i] = err
				return
			}
			matched[i] = result.MatchedCount
		}

		writers.Add(2)
		go attempt(0, clientA)
		go attempt(1, clientB)
		close(start)
		writers.Wait()

		// This is the contract: a refused compare-and-swap reports no match,
		// it does not raise.
		require.NoError(t, failure[0], "round %d: a refused CAS must not be an error", round)
		require.NoError(t, failure[1], "round %d: a refused CAS must not be an error", round)
		require.LessOrEqual(t, matched[0]+matched[1], int64(1),
			"round %d: two writers cannot both win one compare-and-swap", round)

		acknowledged += int32(matched[0] + matched[1])
	}

	require.NotZero(t, acknowledged, "no increment was acknowledged, so this proves nothing")
	require.Equal(t, acknowledged, readVersion(),
		"the stored counter must equal the number of acknowledged increments")
}
