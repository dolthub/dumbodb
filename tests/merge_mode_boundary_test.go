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

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// serializedConvergentWrites has two connections set the same field to the
// same value, then publishes them one after the other, and returns the second
// publish's error or nil.
//
// The fork has to outlive the command for this to be deterministic, so the
// server runs with --session-isolation: both writes land in their own overlay
// before either reconciles. Nothing is left to timing, and the second publish
// certainly reconciles against a branch that already changed the field, so the
// collection's merge mode alone decides whether it is accepted. That the fork
// outlives the command is the only thing --session-isolation contributes here;
// the boundary it reaches is the same boundary an ordinary write reaches.
//
// Convergent is the interesting edit because it is the one the modes disagree
// about: both sides arrive at v=2, which the Divergent modes read as agreement
// and the Touched modes refuse.
func serializedConvergentWrites(t *testing.T, env *dumboDBTestEnv, dbName, mode string) error {
	t.Helper()
	ctx := context.Background()

	seed := siClient(t, env).Database(dbName)
	require.NoError(t, seed.RunCommand(ctx, bson.D{
		{Key: "create", Value: "docs"},
		{Key: "mergeMode", Value: mode},
	}).Err())
	_, err := seed.Collection("docs").InsertOne(ctx,
		bson.D{{Key: "_id", Value: "cas"}, {Key: "v", Value: int32(1)}})
	require.NoError(t, err)
	require.NoError(t, seed.RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1}, {Key: "message", Value: "seed"},
	}).Err(), "both writers must fork from the same committed base")

	filter := bson.D{{Key: "_id", Value: "cas"}}
	update := bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(2)}}}}

	write := func(c *mongo.Client) {
		result, err := c.Database(dbName).Collection("docs").UpdateOne(ctx, filter, update)
		require.NoError(t, err)
		require.EqualValues(t, 1, result.MatchedCount, "each writer must match the seeded document")
	}
	publish := func(c *mongo.Client) error {
		return c.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltCommit", Value: 1}, {Key: "message", Value: "convergent write"},
		}).Err()
	}

	first, second := siClient(t, env), siClient(t, env)
	write(first)
	write(second)
	require.NoError(t, publish(first), "the first publish has nothing to reconcile against")
	return publish(second)
}

// The merge mode governs the boundary a write reconciles at, not only an
// explicit branch merge. Before one merge served both, this boundary ran
// dolt's row merge with no policy at all: a document is stored in a single
// cell, so every mode behaved alike and every concurrent edit conflicted.
func TestMergeMode_GovernsTheWriteBoundary(t *testing.T) {
	for _, tc := range []struct {
		mode         string
		wantConflict bool
	}{
		{"fieldTouched", true},
		{"documentTouched", true},
		{"fieldDivergent", false},
		{"documentDivergent", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			env := startDumboDB(t, "--session-isolation")
			dbName := fmt.Sprintf("mmb_%d", rand.Int64N(1_000_000))

			err := serializedConvergentWrites(t, env, dbName, tc.mode)
			if tc.wantConflict {
				require.Error(t, err,
					"mergeMode=%s must refuse a convergent edit at the write boundary", tc.mode)
				return
			}
			require.NoError(t, err,
				"mergeMode=%s must accept a convergent edit at the write boundary", tc.mode)
		})
	}
}
