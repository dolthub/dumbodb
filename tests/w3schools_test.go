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
)

// TestW3S_AggAddFields_ComputedAvg verifies that $addFields can compute the average
// of an array field using the $avg expression operator. (DocudoltFull)
//
// Mirrors the w3schools MongoDB $addFields + $avg example:
// https://www.w3schools.com/mongodb/mongodb_aggregation_addfields.php
func TestW3S_AggAddFields_ComputedAvg(t *testing.T) {
	t.Parallel()
	env := startDocudolt(t)
	coll := env.collection(t)
	ctx := context.Background()

	// Insert student documents with exam score arrays.
	insertDocs(t, coll,
		d(e("_id", int32(1)), e("name", "Alice"), e("scores", bson.A{int32(85), int32(92), int32(78)})),
		d(e("_id", int32(2)), e("name", "Bob"), e("scores", bson.A{int32(70), int32(88), int32(95)})),
		d(e("_id", int32(3)), e("name", "Carol"), e("scores", bson.A{int32(60), int32(72), int32(84)})),
	)

	// Use $addFields with $avg to compute the average score per student.
	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{Key: "$addFields", Value: bson.D{{Key: "avgScore", Value: bson.D{{Key: "$avg", Value: "$scores"}}}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "_id", Value: int32(1)}}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// Alice: (85+92+78)/3 = 85.0
	aliceAvg, ok := results[0].Map()["avgScore"].(float64)
	require.True(t, ok, "avgScore for Alice must be float64")
	assert.InDelta(t, 85.0, aliceAvg, 0.001)

	// Bob: (70+88+95)/3 = 84.333...
	bobAvg, ok := results[1].Map()["avgScore"].(float64)
	require.True(t, ok, "avgScore for Bob must be float64")
	assert.InDelta(t, 84.333, bobAvg, 0.001)

	// Carol: (60+72+84)/3 = 72.0
	carolAvg, ok := results[2].Map()["avgScore"].(float64)
	require.True(t, ok, "avgScore for Carol must be float64")
	assert.InDelta(t, 72.0, carolAvg, 0.001)
}

// TestW3S_Indexing_AtlasSearch verifies that attempting to create an Atlas Search
// index returns a proper "not implemented" error rather than crashing or returning
// an unexpected response. (DocudoltFull)
//
// Atlas Search requires a MongoDB Atlas deployment and is not supported by Docudolt.
// The driver should receive a clear error code so callers can detect and handle
// the "not supported" case.
func TestW3S_Indexing_AtlasSearch(t *testing.T) {
	t.Parallel()
	env := startDocudolt(t)
	coll := env.collection(t)
	ctx := context.Background()

	// Attempt to create an Atlas Search index; expect a proper server error.
	_, err := coll.SearchIndexes().CreateOne(ctx, mongo.SearchIndexModel{
		Definition: bson.D{
			{Key: "mappings", Value: bson.D{
				{Key: "dynamic", Value: true},
			}},
		},
	})

	// The server must return an error — not a panic or a hang.
	require.Error(t, err, "createSearchIndexes must return an error on Docudolt")

	// The error should be a MongoDB command error with a useful message.
	var cmdErr mongo.CommandError
	require.ErrorAs(t, err, &cmdErr, "error must be a CommandError")
	assert.Contains(t, cmdErr.Message, "not supported",
		"error message should indicate Atlas Search is not supported")
}
