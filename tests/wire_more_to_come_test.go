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
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

// TestWireMoreToCome_UnacknowledgedWriteFollowedByAck regresses do-5wbv:
// at wire version 25, drivers send unacknowledged writes (w:0) using OP_MSG
// with the moreToCome flag set, signalling fire-and-forget. The server must
// NOT send a response, otherwise the next acknowledged operation reads the
// stale response and the driver fails with:
//
//	"Server ended moreToCome unexpectedly"
//
// This test reproduces an unacknowledged insert followed immediately by an
// acknowledged operation on the same connection, which is the exact pattern
// that triggered the bug in production.
func TestWireMoreToCome_UnacknowledgedWriteFollowedByAck(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	addr := fmt.Sprintf("mongodb://127.0.0.1:%d/", env.Port)

	// Connect with maxPoolSize=1 so the unacknowledged insert and the
	// acknowledged read MUST share the same wire connection.
	w0Client, err := mongo.Connect(ctx, options.Client().
		ApplyURI(addr).
		SetMaxPoolSize(1).
		SetWriteConcern(writeconcern.Unacknowledged()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = w0Client.Disconnect(ctx) })

	coll := w0Client.Database("testdb").Collection(fmt.Sprintf("mtc_%d", env.Port))
	t.Cleanup(func() { _ = coll.Drop(ctx) })

	// Fire-and-forget insert (driver sets moreToCome=1, expects no reply).
	// The Go driver returns mongo.ErrUnacknowledgedWrite as a sentinel for
	// w:0 writes; that is not a failure of the server.
	_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}})
	if err != nil && !errors.Is(err, mongo.ErrUnacknowledgedWrite) {
		t.Fatalf("unacknowledged insert should not error: %v", err)
	}

	// Acknowledged op on the SAME connection. Before the fix, the server's
	// stale response to the prior insert would be read here and the driver
	// would fail with "Server ended moreToCome unexpectedly" or similar
	// pool-cleared error.
	ackColl := coll.Database().Collection(coll.Name(), options.Collection().
		SetWriteConcern(writeconcern.Majority()))
	_, err = ackColl.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(2)}})
	require.NoError(t, err, "acknowledged insert after w:0 must succeed without wire desync")

	count, err := ackColl.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	require.Equal(t, int64(2), count, "both inserts should be visible")
}
