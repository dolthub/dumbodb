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

// A plain write parked behind another user's open transaction holds no roots
// GC must wait for, so it must not hold up dumboGC of any database
// (dolthub/dumbodb#342).
func TestDumboGCIsNotHeldByWriteParkedOnTransaction(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	suffix := rand.Int64N(1_000_000)
	attacker := fmt.Sprintf("gcpark_a_%d", suffix)
	victim := fmt.Sprintf("gcpark_v_%d", suffix)
	_, err := env.Client.Database(attacker).Collection("c").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}})
	require.NoError(t, err)
	_, err = env.Client.Database(victim).Collection("c").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}})
	require.NoError(t, err)

	holder := newClient(t, env)
	sess, err := holder.StartSession()
	require.NoError(t, err)
	t.Cleanup(func() { sess.EndSession(context.Background()) })
	require.NoError(t, sess.StartTransaction())
	sessCtx := mongo.NewSessionContext(ctx, sess)
	_, err = holder.Database(attacker).Collection("c").
		UpdateOne(sessCtx, bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: 1}}}})
	require.NoError(t, err)
	t.Cleanup(func() { sess.AbortTransaction(context.Background()) }) //nolint:errcheck

	parkedCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() {
		_, _ = newClient(t, env).Database(attacker).Collection("c").
			UpdateOne(parkedCtx, bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: 2}}}})
	}()
	time.Sleep(500 * time.Millisecond) // let the plain write park on the transaction's lock

	start := time.Now()
	err = env.Client.Database(victim).RunCommand(ctx, bson.D{{Key: "dumboGC", Value: 1}, {Key: "maxTimeMS", Value: 5000}}).Err()
	require.NoError(t, err)
	require.Less(t, time.Since(start), 3*time.Second, "dumboGC waited for a write parked on another database's transaction")
}
