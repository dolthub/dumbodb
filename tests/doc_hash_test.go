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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// docHashEntries reads the dumboDocHashes array from a write acknowledgment.
func docHashEntries(t *testing.T, res bson.M) []bson.M {
	t.Helper()

	raw, ok := res["dumboDocHashes"]
	require.True(t, ok, "acknowledgment must carry dumboDocHashes: %v", res)

	arr, ok := raw.(bson.A)
	require.True(t, ok, "dumboDocHashes must be an array, got %T", raw)

	out := make([]bson.M, 0, len(arr))
	for _, entry := range arr {
		out = append(out, dmap(entry))
	}
	return out
}

func docHashOf(t *testing.T, entry bson.M) string {
	t.Helper()

	h, ok := entry["hash"].(string)
	require.True(t, ok, "entry must carry a string hash, got %T", entry["hash"])
	require.Len(t, h, 32, "hashes render as 32-character base32, like commit hashes")
	return h
}

// insert with dumboDocHashes:true names every stored document and its content
// hash on the acknowledgment; without the flag the reply is untouched.
func TestDocHashes_InsertAck(t *testing.T) {
	env := startDumboDB(t)
	db := env.Client.Database("testdb")
	coll := env.Collection(t)

	res := runCommandRaw(t, db, bson.D{
		{Key: "insert", Value: coll.Name()},
		{Key: "documents", Value: bson.A{
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "a", Value: "x"}},
			bson.D{{Key: "_id", Value: int32(2)}, {Key: "a", Value: "y"}},
		}},
		{Key: "dumboDocHashes", Value: true},
	})
	require.Equal(t, float64(1), res["ok"])

	entries := docHashEntries(t, res)
	require.Len(t, entries, 2)
	assert.Equal(t, int32(0), entries[0]["index"])
	assert.Equal(t, int32(1), entries[0]["_id"])
	assert.Equal(t, int32(1), entries[1]["index"])
	assert.Equal(t, int32(2), entries[1]["_id"])
	assert.NotEqual(t, docHashOf(t, entries[0]), docHashOf(t, entries[1]),
		"different documents must hash differently")

	plain := runCommandRaw(t, db, bson.D{
		{Key: "insert", Value: coll.Name()},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: int32(3)}}}},
	})
	require.Equal(t, float64(1), plain["ok"])
	_, present := plain["dumboDocHashes"]
	assert.False(t, present, "an insert that did not ask must answer exactly as MongoDB does")
}

// The hash tracks stored content across writes: it moves when a field changes
// and comes back when the earlier content is written again.
func TestDocHashes_UpdateAckFollowsContent(t *testing.T) {
	env := startDumboDB(t)
	db := env.Client.Database("testdb")
	coll := env.Collection(t)

	inserted := runCommandRaw(t, db, bson.D{
		{Key: "insert", Value: coll.Name()},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: int32(1)}, {Key: "a", Value: "x"}}}},
		{Key: "dumboDocHashes", Value: true},
	})
	original := docHashOf(t, docHashEntries(t, inserted)[0])

	changed := runCommandRaw(t, db, bson.D{
		{Key: "update", Value: coll.Name()},
		{Key: "updates", Value: bson.A{bson.D{
			{Key: "q", Value: bson.D{{Key: "_id", Value: int32(1)}}},
			{Key: "u", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: "y"}}}}},
		}}},
		{Key: "dumboDocHashes", Value: true},
	})
	require.Equal(t, float64(1), changed["ok"])

	entries := docHashEntries(t, changed)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(1), entries[0]["_id"])
	assert.NotEqual(t, original, docHashOf(t, entries[0]), "changed content must change the hash")

	restored := runCommandRaw(t, db, bson.D{
		{Key: "update", Value: coll.Name()},
		{Key: "updates", Value: bson.A{bson.D{
			{Key: "q", Value: bson.D{{Key: "_id", Value: int32(1)}}},
			{Key: "u", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: "x"}}}}},
		}}},
		{Key: "dumboDocHashes", Value: true},
	})
	assert.Equal(t, original, docHashOf(t, docHashEntries(t, restored)[0]),
		"identical content must hash identically, whatever route wrote it")
}

// A commit does not touch document content, so the hashes a client holds stay
// valid across it: document identity is not database version.
func TestDocHashes_SurviveCommit(t *testing.T) {
	env := startDumboDB(t)
	db := env.Client.Database("testdb")
	coll := env.Collection(t)

	inserted := runCommandRaw(t, db, bson.D{
		{Key: "insert", Value: coll.Name()},
		{Key: "documents", Value: bson.A{bson.D{{Key: "_id", Value: int32(1)}, {Key: "a", Value: "x"}}}},
		{Key: "dumboDocHashes", Value: true},
	})
	original := docHashOf(t, docHashEntries(t, inserted)[0])

	dumboDBCommit(t, env, "testdb", "hold the document")

	// A write that changes nothing stores nothing, so it names no document.
	noop := runCommandRaw(t, db, bson.D{
		{Key: "update", Value: coll.Name()},
		{Key: "updates", Value: bson.A{bson.D{
			{Key: "q", Value: bson.D{{Key: "_id", Value: int32(1)}}},
			{Key: "u", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: "x"}}}}},
		}}},
		{Key: "dumboDocHashes", Value: true},
	})
	assert.Empty(t, docHashEntries(t, noop), "an update that stores nothing reports nothing")

	runCommandRaw(t, db, bson.D{
		{Key: "update", Value: coll.Name()},
		{Key: "updates", Value: bson.A{bson.D{
			{Key: "q", Value: bson.D{{Key: "_id", Value: int32(1)}}},
			{Key: "u", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: "y"}}}}},
		}}},
	})

	rewritten := runCommandRaw(t, db, bson.D{
		{Key: "update", Value: coll.Name()},
		{Key: "updates", Value: bson.A{bson.D{
			{Key: "q", Value: bson.D{{Key: "_id", Value: int32(1)}}},
			{Key: "u", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: "x"}}}}},
		}}},
		{Key: "dumboDocHashes", Value: true},
	})
	assert.Equal(t, original, docHashOf(t, docHashEntries(t, rewritten)[0]),
		"a commit between writes must not move a document's hash")
}

// An upsert stores a document, so it reports one too.
func TestDocHashes_Upsert(t *testing.T) {
	env := startDumboDB(t)
	db := env.Client.Database("testdb")
	coll := env.Collection(t)

	res := runCommandRaw(t, db, bson.D{
		{Key: "update", Value: coll.Name()},
		{Key: "updates", Value: bson.A{bson.D{
			{Key: "q", Value: bson.D{{Key: "_id", Value: int32(7)}}},
			{Key: "u", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: "x"}}}}},
			{Key: "upsert", Value: true},
		}}},
		{Key: "dumboDocHashes", Value: true},
	})
	require.Equal(t, float64(1), res["ok"])

	entries := docHashEntries(t, res)
	require.Len(t, entries, 1)
	assert.Equal(t, int32(7), entries[0]["_id"])
	assert.NotEmpty(t, docHashOf(t, entries[0]))
}
