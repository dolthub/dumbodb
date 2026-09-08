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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// convergentMergeConflicts runs the compare-and-swap race across two branches
// and reports whether the merge conflicted. Both sides move v from 1 to 2, so
// the documents end up identical -- the case a Touched mode refuses and a
// Divergent mode accepts.
func convergentMergeConflicts(t *testing.T, env *dumboDBTestEnv, dbName string) bool {
	t.Helper()
	ctx := context.Background()
	mainDB := env.Client.Database(dbName + "@main")

	_, err := mainDB.Collection("docs").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "baseline")
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: int32(1)},
		{Key: "action", Value: "add"},
		{Key: "branch", Value: "feature"},
	}).Err())

	bump := func(db *mongo.Database) {
		res, err := db.Collection("docs").UpdateOne(ctx,
			bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: int32(1)}},
			bson.D{{Key: "$inc", Value: bson.D{{Key: "v", Value: int32(1)}}}})
		require.NoError(t, err)
		require.EqualValues(t, 1, res.MatchedCount, "each branch's CAS must match on its own fork")
	}
	bump(env.Client.Database(dbName + "@feature"))
	dumboDBCommit(t, env, dbName+"@feature", "feature bump")
	bump(mainDB)
	dumboDBCommit(t, env, dbName+"@main", "main bump")

	mergeRaw := runCommandRaw(t, mainDB, bson.D{
		{Key: "doltMerge", Value: int32(1)}, {Key: "mergeIn", Value: "feature"},
	})
	return mergeRaw["ok"] == float64(0)
}

// A collection's mergeMode is settable at create and decides its merges.
func TestMergeMode_SetAtCreate(t *testing.T) {
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
			env := startDumboDB(t)
			ctx := context.Background()
			dbName := fmt.Sprintf("mmc_%d", rand.Int64N(1_000_000))

			require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
				{Key: "create", Value: "docs"},
				{Key: "mergeMode", Value: tc.mode},
			}).Err(), "create with mergeMode=%s must succeed", tc.mode)

			got := convergentMergeConflicts(t, env, dbName)
			assert.Equal(t, tc.wantConflict, got,
				"mergeMode=%s: convergent edit conflict=%t, want %t", tc.mode, got, tc.wantConflict)
		})
	}
}

// collMod changes the mode, and the change takes effect on the next merge.
func TestMergeMode_ChangedByCollMod(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mmm_%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)

	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "create", Value: "docs"},
		{Key: "mergeMode", Value: "fieldDivergent"},
	}).Err())

	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "docs"},
		{Key: "mergeMode", Value: "fieldTouched"},
	}).Err(), "collMod must accept mergeMode")

	assert.True(t, convergentMergeConflicts(t, env, dbName),
		"after collMod to fieldTouched, a convergent edit must conflict")
}

// A collection that declares nothing gets the default, which is the mode the
// compare-and-swap pattern needs.
func TestMergeMode_DefaultsToFieldTouched(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mmd_%d", rand.Int64N(1_000_000))
	require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "create", Value: "docs"},
	}).Err())

	assert.True(t, convergentMergeConflicts(t, env, dbName),
		"an unset mergeMode must resolve to fieldTouched, which refuses a convergent CAS")
}

// An unknown name is rejected rather than silently resolving to the default: a
// client that misspells a mode must not believe it got what it asked for.
func TestMergeMode_RejectsUnknownName(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	db := env.Client.Database(fmt.Sprintf("mmr_%d", rand.Int64N(1_000_000)))

	err := db.RunCommand(ctx, bson.D{
		{Key: "create", Value: "docs"},
		{Key: "mergeMode", Value: "fieldTouchd"},
	}).Err()
	require.Error(t, err, "a misspelled mergeMode must be rejected")
	assert.Contains(t, err.Error(), "mergeMode")

	require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "create", Value: "docs"}}).Err())
	err = db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "docs"},
		{Key: "mergeMode", Value: int32(2)},
	}).Err()
	require.Error(t, err, "a non-string mergeMode must be rejected")
}

// mergeMode is reported with the rest of a collection's configuration, so a
// client can read back what it set. It has no MongoDB counterpart, which is a
// deliberate deviation: reading configuration back matters more here than
// matching a surface MongoDB has no reason to carry.
func TestMergeMode_ReportedByListCollections(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	db := env.Client.Database(fmt.Sprintf("mml_%d", rand.Int64N(1_000_000)))

	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "create", Value: "declared"},
		{Key: "mergeMode", Value: "documentTouched"},
	}).Err())
	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "create", Value: "unset"},
	}).Err())

	cur, err := db.ListCollections(ctx, bson.D{})
	require.NoError(t, err)
	var specs []bson.M
	require.NoError(t, cur.All(ctx, &specs))

	byName := map[string]bson.M{}
	for _, spec := range specs {
		name, _ := spec["name"].(string)
		byName[name] = spec
	}
	require.Contains(t, byName, "declared")
	require.Contains(t, byName, "unset")

	declaredOpts, ok := byName["declared"]["options"].(bson.M)
	require.True(t, ok, "declared collection must report options")
	assert.Equal(t, "documentTouched", declaredOpts["mergeMode"],
		"a declared mergeMode must be reported alongside the other options")

	// An unset mode is left unreported rather than materialized, the same way
	// the validation defaults are.
	if unsetOpts, ok := byName["unset"]["options"].(bson.M); ok {
		assert.NotContains(t, unsetOpts, "mergeMode",
			"an unset mergeMode must not be materialized")
	}
}

// collMod's change is visible through listCollections.
func TestMergeMode_CollModVisibleInListCollections(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	db := env.Client.Database(fmt.Sprintf("mmv_%d", rand.Int64N(1_000_000)))

	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "create", Value: "docs"},
		{Key: "mergeMode", Value: "fieldDivergent"},
	}).Err())
	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "docs"},
		{Key: "mergeMode", Value: "fieldTouched"},
	}).Err())

	cur, err := db.ListCollections(ctx, bson.D{{Key: "name", Value: "docs"}})
	require.NoError(t, err)
	var specs []bson.M
	require.NoError(t, cur.All(ctx, &specs))
	require.Len(t, specs, 1)
	opts, ok := specs[0]["options"].(bson.M)
	require.True(t, ok)
	assert.Equal(t, "fieldTouched", opts["mergeMode"])
}
