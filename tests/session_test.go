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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func newClient(t *testing.T, env *dumboDBTestEnv) *mongo.Client {
	t.Helper()
	uri := fmt.Sprintf("mongodb://127.0.0.1:%d", env.Port)
	c, err := mongo.Connect(options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })
	return c
}

func mongoCommandCode(err error) int32 {
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) {
		return cmdErr.Code
	}
	var writeExc mongo.WriteException
	if errors.As(err, &writeExc) {
		if writeExc.WriteConcernError != nil {
			return int32(writeExc.WriteConcernError.Code)
		}
		for _, we := range writeExc.WriteErrors {
			return int32(we.Code)
		}
	}
	return 0
}

// Two connections may hold one lsid. Drivers pool logical sessions
// independently of connections, so this is ordinary traffic, and neither
// connection may lose its session to the other. This replaced an earlier
// contract where the second connection evicted the first and the first's next
// command was answered with code 225, which cost a 30-minute soak 1,550
// indeterminate outcomes.
func TestSession_LsidSharedByTwoConnections(t *testing.T) {
	env := startDumboDB(t)

	a := dialWire(t, env)
	b := dialWire(t, env)

	dbName := fmt.Sprintf("supersede_%d", env.Port)
	lsid := freshLsid()

	resA, err := a.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "from-A"}}}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	assert.Equal(t, 1.0, resA["ok"])

	resB, err := b.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "from-B"}}}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	assert.Equal(t, 1.0, resB["ok"])

	// A keeps its session and sees both writes: one lsid is one session,
	// however many connections are holding it.
	resA2, err := a.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resA2["ok"], "a second connection on this lsid must not cost A its session")
	assert.Len(t, firstBatchFromFindResponse(t, resA2), 2)
}

func TestSession_DoltCommitDurabilityWithAnotherConnectionOnTheLsid(t *testing.T) {
	env := startDumboDB(t)
	dbName := fmt.Sprintf("commitfence_%d", env.Port)
	lsid := freshLsid()

	a := dialWire(t, env)

	_, err := a.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "from-A"}}}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)

	commitDone := make(chan struct{})
	bDone := make(chan bson.M, 1)
	go func() {
		<-commitDone
		b := dialWire(t, env)
		res, err := b.run(bson.D{
			{Key: "find", Value: "c"},
			{Key: "filter", Value: bson.D{}},
			{Key: "lsid", Value: lsid},
			{Key: "$db", Value: dbName},
		})
		require.NoError(t, err)
		bDone <- res
	}()

	close(commitDone)
	resCommit, err := a.run(bson.D{
		{Key: "doltCommit", Value: int32(1)},
		{Key: "message", Value: "A commits"},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resCommit["ok"])

	resFind := <-bDone
	require.Equal(t, 1.0, resFind["ok"])
	first := firstBatchFromFindResponse(t, resFind)
	require.Len(t, first, 1)
	doc, ok := first[0].(bson.D)
	require.True(t, ok)
	assert.Equal(t, "from-A", bsonDLookup(doc, "_id"))

	// The durable commit is the point of this test. A also keeps working:
	// B arriving on the same lsid is not a takeover.
	resA2, err := a.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	assert.Equal(t, 1.0, resA2["ok"])
}

// Default-mode reconnect: a multi-document transaction started by
// conn1 continues on conn2 when conn2 carries the same lsid +
// txnNumber + autocommit:false. The dispatch observes autocommit:false
// on every frame (not just the first via startTransaction:true), so
// the second TCP connection's InTransaction flag reflects the wire
// frame and writes/reads route through the lsid-keyed pending overlay.
// This is the property the parity-side P6 lsid_reconnect_wire test
// covers against an RS Mongo; this local version locks it in for
// DumboDB CI runs where MONGO_RS_URI may not be set.
func TestTransaction_WireReconnectResumesTransactionState(t *testing.T) {
	env := startDumboDB(t)
	dbName := fmt.Sprintf("txnreconnect_%d", env.Port)
	lsid := freshLsid()
	const txnNum = int64(1)

	conn1 := dialWire(t, env)
	resInsert, err := conn1.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "in-txn"}}}},
		{Key: "lsid", Value: lsid},
		{Key: "txnNumber", Value: txnNum},
		{Key: "startTransaction", Value: true},
		{Key: "autocommit", Value: false},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resInsert["ok"])
	_ = conn1.c.Close()

	conn2 := dialWire(t, env)
	resFind, err := conn2.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "lsid", Value: lsid},
		{Key: "txnNumber", Value: txnNum},
		{Key: "autocommit", Value: false},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resFind["ok"])
	first := firstBatchFromFindResponse(t, resFind)
	require.Len(t, first, 1, "conn2 must see conn1's uncommitted in-txn write via the lsid-keyed overlay")
	doc, ok := first[0].(bson.D)
	require.True(t, ok)
	assert.Equal(t, "in-txn", bsonDLookup(doc, "_id"))

	resCommit, err := conn2.run(bson.D{
		{Key: "commitTransaction", Value: int32(1)},
		{Key: "lsid", Value: lsid},
		{Key: "txnNumber", Value: txnNum},
		{Key: "autocommit", Value: false},
		{Key: "$db", Value: "admin"},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resCommit["ok"])

	// A fresh lsid sees the now-durable write.
	conn3 := dialWire(t, env)
	resFind2, err := conn3.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "lsid", Value: freshLsid()},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	first = firstBatchFromFindResponse(t, resFind2)
	require.Len(t, first, 1)
	doc, ok = first[0].(bson.D)
	require.True(t, ok)
	assert.Equal(t, "in-txn", bsonDLookup(doc, "_id"))
}

func TestSession_ReconnectResumesCommittedState(t *testing.T) {
	env := startDumboDB(t)

	a := dialWire(t, env)
	dbName := fmt.Sprintf("reconnect_%d", env.Port)
	lsid := freshLsid()

	resInsert, err := a.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "from-A"}}}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resInsert["ok"])

	resCommit, err := a.run(bson.D{
		{Key: "doltCommit", Value: int32(1)},
		{Key: "message", Value: "A commits"},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resCommit["ok"])

	_ = a.c.Close()

	b := dialWire(t, env)
	resFind, err := b.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "lsid", Value: lsid},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	require.Equal(t, 1.0, resFind["ok"])

	first := firstBatchFromFindResponse(t, resFind)
	require.Len(t, first, 1)
	doc, ok := first[0].(bson.D)
	require.True(t, ok)
	assert.Equal(t, "from-A", bsonDLookup(doc, "_id"))
}

func firstBatchFromFindResponse(t *testing.T, res bson.M) bson.A {
	t.Helper()
	cursor, ok := res["cursor"].(bson.D)
	require.True(t, ok, "find response must carry cursor; got %#v", res)
	first, ok := bsonDLookup(cursor, "firstBatch").(bson.A)
	require.True(t, ok, "cursor must have firstBatch; got %#v", cursor)
	return first
}

func bsonDLookup(d bson.D, key string) interface{} {
	for _, e := range d {
		if e.Key == key {
			return e.Value
		}
	}
	return nil
}
