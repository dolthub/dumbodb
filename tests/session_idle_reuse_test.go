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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// TestSession_IdleReap_ReuseRecovers pins the fix for a wedge where a client
// reusing a session after the idle sweep reaped it got a permanent code-251
// error (as mongo-express did after sitting idle). A plain command must recover
// on a fresh session, matching MongoDB, which recreates reaped idle sessions.
func TestSession_IdleReap_ReuseRecovers(t *testing.T) {
	env := startDumboDB(t, "--session-timeout", "1s", "--session-sweep-period", "1s")
	ctx := context.Background()
	coll := env.Collection(t)

	sess, err := env.Client.StartSession()
	require.NoError(t, err)
	defer sess.EndSession(ctx)
	sc := mongo.NewSessionContext(ctx, sess)

	_, err = coll.InsertOne(sc, bson.D{{Key: "_id", Value: "before-reap"}})
	require.NoError(t, err, "first write establishes the session")

	time.Sleep(3 * time.Second)

	_, err = coll.InsertOne(sc, bson.D{{Key: "_id", Value: "after-reap"}})
	require.NoError(t, err, "reusing an idle-reaped session must recover, not fail with 251")

	_, err = coll.InsertOne(sc, bson.D{{Key: "_id", Value: "after-reap-2"}})
	require.NoError(t, err, "recovery must be permanent, not one-shot")

	n, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	require.EqualValues(t, 3, n)
}
