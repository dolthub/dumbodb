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

// serializedWrites has two connections write to one document, then publishes
// them one after the other, and returns the second publish's error or nil.
//
// The fork has to outlive the command for this to be deterministic, so the
// server runs with --session-isolation: both writes land in their own overlay
// before either reconciles. Nothing is left to timing, and the second publish
// certainly reconciles against a branch that already changed the document, so
// the collection's merge mode alone decides whether it is accepted. That the
// fork outlives the command is the only thing --session-isolation contributes
// here; the boundary it reaches is the boundary an ordinary write reaches.
func serializedWrites(t *testing.T, env *dumboDBTestEnv, dbName, mode string, edit writeShape) error {
	t.Helper()
	ctx := context.Background()

	seed := siClient(t, env).Database(dbName)
	require.NoError(t, seed.RunCommand(ctx, bson.D{
		{Key: "create", Value: "docs"},
		{Key: "mergeMode", Value: mode},
	}).Err())
	_, err := seed.Collection("docs").InsertOne(ctx, bson.D{
		{Key: "_id", Value: "d"},
		{Key: "v", Value: int32(1)},
		{Key: "a", Value: "seed"},
		{Key: "b", Value: "seed"},
	})
	require.NoError(t, err)
	require.NoError(t, seed.RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: 1}, {Key: "message", Value: "seed"},
	}).Err(), "both writers must fork from the same committed base")

	write := func(c *mongo.Client, field string, value any) {
		result, err := c.Database(dbName).Collection("docs").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: "d"}},
			bson.D{{Key: "$set", Value: bson.D{{Key: field, Value: value}}}})
		require.NoError(t, err)
		require.EqualValues(t, 1, result.MatchedCount, "each writer must match the seeded document")
	}
	publish := func(c *mongo.Client) error {
		return c.Database(dbName).RunCommand(ctx, bson.D{
			{Key: "doltCommit", Value: 1}, {Key: "message", Value: "write"},
		}).Err()
	}

	first, second := siClient(t, env), siClient(t, env)
	write(first, edit.firstField, edit.firstValue)
	write(second, edit.secondField, edit.secondValue)
	require.NoError(t, publish(first), "the first publish has nothing to reconcile against")
	return publish(second)
}

// writeShape is what the two writers do to the document. Convergent means both
// reach the same value in the same field, which the Divergent modes read as
// agreement and the Touched modes refuse. Disjoint means each owns its own
// field, which the field-granular modes allow and the document-granular modes
// refuse. Together the two shapes separate all four modes: each mode is the
// only one with its pair of answers.
type writeShape struct {
	name                    string
	firstField, secondField string
	firstValue, secondValue any
}

var (
	convergentEdit = writeShape{"convergent", "v", "v", int32(2), int32(2)}
	disjointEdit   = writeShape{"disjoint", "a", "b", "A", "B"}
)

// The merge mode governs the boundary a write reconciles at, not only an
// explicit branch merge. Before one merge served both, this boundary ran
// dolt's row merge with no policy at all: a document is stored in a single
// cell, so every mode behaved alike and every concurrent edit conflicted.
//
// The default, fieldTouched, is the row that makes the MongoDB
// compare-and-swap pattern work: it refuses two writers who both land on
// v=2, and it still lets writers who own different fields through.
func TestMergeMode_GovernsTheWriteBoundary(t *testing.T) {
	for _, tc := range []struct {
		mode         string
		onConvergent bool
		onDisjoint   bool
	}{
		{"documentTouched", true, true},
		{"fieldTouched", true, false},
		{"fieldDivergent", false, false},
		{"documentDivergent", false, true},
	} {
		for _, edit := range []writeShape{convergentEdit, disjointEdit} {
			wantConflict := tc.onConvergent
			if edit.name == disjointEdit.name {
				wantConflict = tc.onDisjoint
			}
			t.Run(tc.mode+"/"+edit.name, func(t *testing.T) {
				env := startDumboDB(t, "--session-isolation")
				dbName := fmt.Sprintf("mmb_%d", rand.Int64N(1_000_000))

				err := serializedWrites(t, env, dbName, tc.mode, edit)
				if wantConflict {
					require.Error(t, err,
						"mergeMode=%s must refuse a %s edit at the write boundary", tc.mode, edit.name)
					return
				}
				require.NoError(t, err,
					"mergeMode=%s must accept a %s edit at the write boundary", tc.mode, edit.name)
			})
		}
	}
}
