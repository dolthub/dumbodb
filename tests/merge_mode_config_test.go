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

func TestMergeMode_OneSidedChangeGovernsSameMerge(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("mmo_%d", rand.Int64N(1_000_000))
	mainDB := env.Client.Database(dbName + "@main")

	require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "create", Value: "docs"}}).Err())
	_, err := mainDB.Collection("docs").InsertOne(ctx,
		bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: int32(1)}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "base")
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{
		{Key: "doltBranch", Value: 1}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"},
	}).Err())

	featureDB := env.Client.Database(dbName + "@feature")
	require.NoError(t, featureDB.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "docs"}, {Key: "mergeMode", Value: "fieldDivergent"},
	}).Err())
	_, err = featureDB.Collection("docs").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(2)}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@feature", "feature changes mode and value")

	_, err = mainDB.Collection("docs").UpdateOne(ctx, bson.D{{Key: "_id", Value: int32(1)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "v", Value: int32(2)}}}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName+"@main", "main changes value")

	raw := runCommandRaw(t, mainDB, bson.D{{Key: "doltMerge", Value: 1}, {Key: "mergeIn", Value: "feature"}})
	require.EqualValues(t, 1, raw["ok"], "the incoming fieldDivergent mode must govern its own one-sided change: %v", raw)
}

func TestMergeMode_DivergenceUsesResolvedMode(t *testing.T) {
	for _, tc := range []struct {
		name             string
		resolution       string
		wantDataConflict bool
	}{
		{name: "fieldDivergent", resolution: "theirs"},
		{name: "documentDivergent", resolution: "ours", wantDataConflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := startDumboDB(t)
			ctx := context.Background()
			dbName := fmt.Sprintf("mmtwo_%d", rand.Int64N(1_000_000))
			mainDB := env.Client.Database(dbName + "@main")
			require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "create", Value: "docs"}}).Err())
			_, err := mainDB.Collection("docs").InsertOne(ctx, bson.D{
				{Key: "_id", Value: 1}, {Key: "a", Value: "base"}, {Key: "b", Value: "base"},
			})
			require.NoError(t, err)
			dumboDBCommit(t, env, dbName+"@main", "base")
			require.NoError(t, mainDB.RunCommand(ctx, bson.D{
				{Key: "doltBranch", Value: 1}, {Key: "action", Value: "add"}, {Key: "branch", Value: "feature"},
			}).Err())

			featureDB := env.Client.Database(dbName + "@feature")
			require.NoError(t, featureDB.RunCommand(ctx, bson.D{
				{Key: "collMod", Value: "docs"}, {Key: "mergeMode", Value: "fieldDivergent"},
			}).Err())
			_, err = featureDB.Collection("docs").UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: "feature"}}}})
			require.NoError(t, err)
			dumboDBCommit(t, env, dbName+"@feature", "feature mode and edit")

			require.NoError(t, mainDB.RunCommand(ctx, bson.D{
				{Key: "collMod", Value: "docs"}, {Key: "mergeMode", Value: "documentDivergent"},
			}).Err())
			_, err = mainDB.Collection("docs").UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "b", Value: "main"}}}})
			require.NoError(t, err)
			dumboDBCommit(t, env, dbName+"@main", "main mode and edit")

			raw := runCommandRaw(t, mainDB, bson.D{{Key: "doltMerge", Value: 1}, {Key: "mergeIn", Value: "feature"}})
			require.EqualValues(t, 0, raw["ok"])
			var conflictResult bson.M
			require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltConflicts", Value: 1}}).Decode(&conflictResult))
			conflicts := conflictResult["conflicts"].(bson.A)
			require.Len(t, conflicts, 1, "phase one must expose metadata only")
			metadata := conflicts[0].(bson.M)
			require.Equal(t, "metadata", metadata["type"])
			assert.Equal(t, "documentDivergent", metadata["ours"].(bson.M)["mergeMode"])
			assert.Equal(t, "fieldDivergent", metadata["theirs"].(bson.M)["mergeMode"])
			var before bson.M
			require.NoError(t, mainDB.Collection("docs").FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&before))
			assert.Equal(t, "base", before["a"])
			assert.Equal(t, "main", before["b"])

			require.NoError(t, mainDB.RunCommand(ctx, bson.D{
				{Key: "doltResolveConflict", Value: 1}, {Key: "collection", Value: "docs"},
				{Key: "conflictId", Value: metadata["conflictId"]}, {Key: "resolution", Value: tc.resolution},
			}).Err())
			continued := runCommandRaw(t, mainDB, bson.D{{Key: "doltMerge", Value: 1}, {Key: "continue", Value: 1}})
			if tc.wantDataConflict {
				require.EqualValues(t, 0, continued["ok"], "documentDivergent must re-pause: %v", continued)
				require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltConflicts", Value: 1}}).Decode(&conflictResult))
				conflicts = conflictResult["conflicts"].(bson.A)
				require.Len(t, conflicts, 1)
				assert.Equal(t, "document", conflicts[0].(bson.M)["type"])
				return
			}
			require.EqualValues(t, 1, continued["ok"], "%v", continued)
			var merged bson.M
			require.NoError(t, mainDB.Collection("docs").FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&merged))
			assert.Equal(t, "feature", merged["a"])
			assert.Equal(t, "main", merged["b"])
		})
	}
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
