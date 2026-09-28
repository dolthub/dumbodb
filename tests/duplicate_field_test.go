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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// A malformed command is a client error, never a dead connection. BSON permits
// a document to repeat a key, so a client can send one whether or not it means
// to -- a driver that attaches its own lsid to a command that already carries
// one produces exactly this.
//
// Before the fix, types.Document.Get panicked on the duplicate and the
// connection was torn down: the client saw "incomplete read of message header"
// with no error document at all.
func TestWire_DuplicateTopLevelFieldIsRejected(t *testing.T) {
	env := startDumboDB(t)
	dbName := fmt.Sprintf("dupfield_%d", env.Port)
	conn := dialWire(t, env)

	res, err := conn.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "x"}}}},
		{Key: "lsid", Value: freshLsid()},
		{Key: "lsid", Value: freshLsid()},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err, "a duplicated field must not kill the connection")
	assert.Equal(t, 0.0, res["ok"], "a duplicated field is a client error")
	if msg, ok := res["errmsg"].(string); ok {
		assert.Contains(t, msg, "lsid", "the error should name the offending field")
	}

	// The connection survives and still serves commands.
	after, err := conn.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: "y"}}}},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err, "the connection must remain usable")
	assert.Equal(t, 1.0, after["ok"])
}

// The same holds for a duplicate that is not a field the server itself reads,
// so the check cannot be a special case for lsid.
func TestWire_DuplicateFieldInAnyPositionIsRejected(t *testing.T) {
	env := startDumboDB(t)
	dbName := fmt.Sprintf("dupfield2_%d", env.Port)
	conn := dialWire(t, env)

	res, err := conn.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "filter", Value: bson.D{}},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err, "a duplicated field must not kill the connection")
	assert.Equal(t, 0.0, res["ok"])

	after, err := conn.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err, "the connection must remain usable")
	assert.Equal(t, 1.0, after["ok"])
}

// A duplicate nested inside a subdocument is the same hazard: the filter is
// read with Get like any other field. Before the fix this killed the
// connection while the top-level check would have let it through.
func TestWire_DuplicateFieldNestedInSubdocumentIsRejected(t *testing.T) {
	env := startDumboDB(t)
	dbName := fmt.Sprintf("dupnested_%d", env.Port)
	conn := dialWire(t, env)

	res, err := conn.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{{Key: "a", Value: 1}, {Key: "a", Value: 2}}},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err, "a nested duplicate must not kill the connection")
	assert.Equal(t, 0.0, res["ok"])
	if msg, ok := res["errmsg"].(string); ok {
		assert.Contains(t, msg, "filter.a", "the error should name the path to the offending field")
	}

	after, err := conn.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err, "the connection must remain usable")
	assert.Equal(t, 1.0, after["ok"])
}

// A duplicate inside an array element is the same hazard and was the one a
// top-level-only check would miss: with the recursive check disabled, this
// command killed the connection.
func TestWire_DuplicateFieldNestedInArrayIsRejected(t *testing.T) {
	env := startDumboDB(t)
	dbName := fmt.Sprintf("duparray_%d", env.Port)
	conn := dialWire(t, env)

	res, err := conn.run(bson.D{
		{Key: "update", Value: "c"},
		{Key: "updates", Value: bson.A{bson.D{
			{Key: "q", Value: bson.D{{Key: "a", Value: 1}, {Key: "a", Value: 2}}},
			{Key: "u", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "b", Value: 1}}}}},
		}}},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err, "a duplicate inside an array element must not kill the connection")
	assert.Equal(t, 0.0, res["ok"])
	if msg, ok := res["errmsg"].(string); ok {
		assert.Contains(t, msg, "updates.0.q.a", "the error should name the path to the offending field")
	}

	after, err := conn.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err, "the connection must remain usable")
	assert.Equal(t, 1.0, after["ok"])
}

// An insert reports a duplicate inside a document it was asked to store as a
// per-document write error, so a batch fails only the malformed document. That
// is a better answer than rejecting the command, and the boundary check is
// deliberately excepted there so it survives.
func TestWire_DuplicateInsideInsertedDocumentStaysAWriteError(t *testing.T) {
	env := startDumboDB(t)
	dbName := fmt.Sprintf("dupinsert_%d", env.Port)
	conn := dialWire(t, env)

	res, err := conn.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{
			bson.D{{Key: "_id", Value: "good"}},
			bson.D{{Key: "_id", Value: "bad"}, {Key: "a", Value: 1}, {Key: "a", Value: 2}},
		}},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	assert.Equal(t, 1.0, res["ok"], "the command succeeds; the malformed document fails on its own")
	require.Contains(t, res, "writeErrors")

	// The well-formed document in the same batch was stored.
	after, err := conn.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{{Key: "_id", Value: "good"}}},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)
	assert.Len(t, firstBatchFromFindResponse(t, after), 1)
}

// A duplicated sort key also reached a panicking Get.
func TestWire_DuplicateSortKeyIsRejected(t *testing.T) {
	env := startDumboDB(t)
	dbName := fmt.Sprintf("dupsort_%d", env.Port)
	conn := dialWire(t, env)

	res, err := conn.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "sort", Value: bson.D{{Key: "a", Value: 1}, {Key: "a", Value: -1}}},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err, "a duplicated sort key must not kill the connection")
	assert.Equal(t, 0.0, res["ok"])
}

// A duplicated property name inside $jsonSchema used to kill the connection:
// the operator's own duplicate check covers the keywords it walks, not the
// property names under "properties". The boundary check has to cover it.
func TestWire_DuplicateJSONSchemaPropertyNameIsRejected(t *testing.T) {
	env := startDumboDB(t)
	dbName := fmt.Sprintf("dupschema_%d", env.Port)
	conn := dialWire(t, env)

	_, err := conn.run(bson.D{
		{Key: "insert", Value: "c"},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: 1}, {Key: "x", Value: 1}}}},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err)

	res, err := conn.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{{Key: "$jsonSchema", Value: bson.D{
			{Key: "properties", Value: bson.D{
				{Key: "x", Value: bson.D{{Key: "type", Value: "int"}}},
				{Key: "x", Value: bson.D{{Key: "type", Value: "string"}}},
			}},
		}}}},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err, "a duplicated property name must not kill the connection")
	assert.Equal(t, 0.0, res["ok"])

	after, err := conn.run(bson.D{
		{Key: "find", Value: "c"},
		{Key: "filter", Value: bson.D{}},
		{Key: "$db", Value: dbName},
	})
	require.NoError(t, err, "the connection must remain usable")
	assert.Equal(t, 1.0, after["ok"])
}
