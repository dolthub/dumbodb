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
)

// assertExplainResponse checks that the explain response contains the expected
// fields for the given verbosity level.
func assertExplainResponse(t *testing.T, res bson.D, verbosity string) {
	t.Helper()

	m := res.Map()

	assert.Equal(t, float64(1), m["ok"], "ok must be 1")
	assert.NotEmpty(t, m["queryPlanner"], "queryPlanner must be present and non-empty")
	assert.IsType(t, bson.D{}, m["queryPlanner"], "queryPlanner must be a document")
	assert.NotNil(t, m["serverInfo"], "serverInfo must be present")
	assert.NotNil(t, m["command"], "command must be present")

	switch verbosity {
	case "executionStats", "allPlansExecution":
		assert.NotNil(t, m["executionStats"], "executionStats must be present for verbosity %q", verbosity)
	default:
		// queryPlanner verbosity: executionStats may or may not be present; don't assert its absence
	}

	if verbosity == "allPlansExecution" {
		assert.NotNil(t, m["allPlansExecution"], "allPlansExecution must be present for verbosity %q", verbosity)
	}
}

// TestExplain_Find_QueryPlanner tests explain for find with queryPlanner verbosity. (DongoFull)
func TestExplain_Find_QueryPlanner(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{{Key: "find", Value: coll.Name()}}},
		{Key: "verbosity", Value: "queryPlanner"},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "queryPlanner")
}

// TestExplain_Find_ExecutionStats tests explain for find with executionStats verbosity. (DongoFull)
func TestExplain_Find_ExecutionStats(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{{Key: "find", Value: coll.Name()}}},
		{Key: "verbosity", Value: "executionStats"},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "executionStats")
}

// TestExplain_Find_AllPlansExecution tests explain for find with allPlansExecution verbosity. (DongoFull)
func TestExplain_Find_AllPlansExecution(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{{Key: "find", Value: coll.Name()}}},
		{Key: "verbosity", Value: "allPlansExecution"},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "allPlansExecution")
}

// TestExplain_Aggregate tests explain for aggregate command. (DongoFull)
func TestExplain_Aggregate(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{
			{Key: "aggregate", Value: coll.Name()},
			{Key: "pipeline", Value: bson.A{}},
			{Key: "cursor", Value: bson.D{}},
		}},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "queryPlanner")
}

// TestExplain_Count tests explain for count command. (DongoFull)
func TestExplain_Count(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{{Key: "count", Value: coll.Name()}}},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "queryPlanner")
}

// TestExplain_Update tests explain for update command. (DongoFull)
func TestExplain_Update(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{
			{Key: "update", Value: coll.Name()},
			{Key: "updates", Value: bson.A{
				bson.D{
					{Key: "q", Value: bson.D{{Key: "v", Value: int32(1)}}},
					{Key: "u", Value: bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(2)}}}}},
				},
			}},
		}},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "queryPlanner")
}

// TestExplain_Delete tests explain for delete command. (DongoFull)
func TestExplain_Delete(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{
			{Key: "delete", Value: coll.Name()},
			{Key: "deletes", Value: bson.A{
				bson.D{
					{Key: "q", Value: bson.D{{Key: "v", Value: int32(1)}}},
					{Key: "limit", Value: int32(0)},
				},
			}},
		}},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "queryPlanner")
}

// TestExplain_Distinct tests explain for distinct command. (DongoFull)
func TestExplain_Distinct(t *testing.T) {
	env := startDongo(t)
	ctx := context.Background()
	coll := env.collection(t)

	insertDocs(t, coll, bson.D{{Key: "v", Value: int32(1)}})

	var res bson.D
	err := coll.Database().RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{
			{Key: "distinct", Value: coll.Name()},
			{Key: "key", Value: "v"},
		}},
	}).Decode(&res)
	require.NoError(t, err)
	require.NotNil(t, res)

	assertExplainResponse(t, res, "queryPlanner")
}
