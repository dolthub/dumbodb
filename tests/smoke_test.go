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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// TestTransactionSmoke verifies the basic session and transaction lifecycle:
// startSession → StartTransaction → insert documents → CommitTransaction → EndSession.
// (DumboDBFull)
//
// DumboDB does not provide multi-document ACID isolation, but the driver-level
// transaction API must not produce a crash or "no such command" error. The
// round-trip is accepted and inserts are applied immediately.
func TestTransactionSmoke(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	// Start a session.
	sess, err := env.client.StartSession()
	require.NoError(t, err, "StartSession must succeed")
	defer sess.EndSession(ctx)

	// Execute the transaction body via WithTransaction so the driver
	// handles StartTransaction / CommitTransaction automatically.
	_, err = sess.WithTransaction(ctx, func(sessCtx mongo.SessionContext) (interface{}, error) {
		_, insErr := coll.InsertOne(sessCtx, bson.D{
			{Key: "txn", Value: "commit"},
			{Key: "n", Value: int32(1)},
		})
		return nil, insErr
	}, options.Transaction().SetReadPreference(readpref.Primary()))
	require.NoError(t, err, "WithTransaction (commit path) must succeed")

	// The document must be visible after commit.
	count, err := coll.CountDocuments(ctx, bson.D{{Key: "txn", Value: "commit"}})
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "inserted document must be visible after commit")
}

// TestTransactionSmoke_Abort verifies that aborting a transaction does not crash
// the server. Because DumboDB does not provide ACID isolation, the inserted
// documents remain visible — but the server must not panic or return an error
// on abortTransaction. (DumboDBFull)
func TestTransactionSmoke_Abort(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := env.collection(t)

	sess, err := env.client.StartSession()
	require.NoError(t, err, "StartSession must succeed")
	defer sess.EndSession(ctx)

	// Start and immediately abort a transaction.
	require.NoError(t, sess.StartTransaction(), "StartTransaction must succeed")

	mongoCtx := mongo.NewSessionContext(ctx, sess)
	_, _ = coll.InsertOne(mongoCtx, bson.D{{Key: "txn", Value: "abort"}, {Key: "n", Value: int32(2)}})

	// AbortTransaction must not return an error even though DumboDB does not
	// actually roll back the insert.
	require.NoError(t, sess.AbortTransaction(ctx), "AbortTransaction must not error")
}
