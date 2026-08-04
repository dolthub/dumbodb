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

package verify

// Automated twin of docs/verify/validators.md, focused on the DumboDB-only
// version-control behavior of document validators (durability, branch, merge).
// Enforcement parity is covered against MongoDB in the parity harness; the
// enforcement checks here are sanity checks that the validator is active.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const valDocValidationFailure = 121

// valNonNegAge is the validator used throughout: age >= 0.
var valNonNegAge = bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: int32(0)}}}}

// valErrCode extracts a code from a CommandError or WriteException.
func valErrCode(err error) int32 {
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		return ce.Code
	}
	var we mongo.WriteException
	if errors.As(err, &we) {
		if len(we.WriteErrors) > 0 {
			return int32(we.WriteErrors[0].Code)
		}
	}
	return 0
}

// validatorOf returns the validator document reported by listCollections for
// coll, or nil when none is set.
func validatorOf(t *testing.T, db *mongo.Database, coll string) bson.M {
	t.Helper()
	var lc bson.M
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "listCollections", Value: 1},
		{Key: "filter", Value: bson.D{{Key: "name", Value: coll}}},
	}).Decode(&lc))
	batch := lc["cursor"].(bson.M)["firstBatch"].(bson.A)
	require.Len(t, batch, 1, "collection %q must exist", coll)
	opts, _ := batch[0].(bson.M)["options"].(bson.M)
	v, _ := opts["validator"].(bson.M)
	return v
}

// ageGte builds an { age: { $gte: n } } validator.
func ageGte(n int32) bson.D {
	return bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: n}}}}
}

// jsonSchemaRequiring builds a { $jsonSchema: { required: [field] } } validator.
func jsonSchemaRequiring(field string) bson.D {
	return bson.D{{Key: "$jsonSchema", Value: bson.D{
		{Key: "bsonType", Value: "object"},
		{Key: "required", Value: bson.A{field}},
		{Key: "properties", Value: bson.D{{Key: field, Value: bson.D{{Key: "bsonType", Value: "string"}}}}},
	}}}
}

// requiredField returns the single required field of a $jsonSchema validator.
func requiredField(t *testing.T, v bson.M) string {
	t.Helper()
	js, ok := v["$jsonSchema"].(bson.M)
	require.True(t, ok, "not a $jsonSchema validator: %v", v)
	req, _ := js["required"].(bson.A)
	require.Len(t, req, 1, "expected one required field: %v", js)
	return req[0].(string)
}

// validationActionOf returns the validationAction reported by listCollections.
func validationActionOf(t *testing.T, db *mongo.Database, coll string) string {
	t.Helper()
	var lc bson.M
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "listCollections", Value: 1},
		{Key: "filter", Value: bson.D{{Key: "name", Value: coll}}},
	}).Decode(&lc))
	batch := lc["cursor"].(bson.M)["firstBatch"].(bson.A)
	require.Len(t, batch, 1)
	opts, _ := batch[0].(bson.M)["options"].(bson.M)
	s, _ := opts["validationAction"].(string)
	return s
}

// resolveMeta resolves the single metadata conflict on coll with the given
// resolution (and optional custom value), then completes the merge.
func resolveMeta(t *testing.T, mainDB *mongo.Database, coll, resolution string, value bson.D) {
	t.Helper()
	ctx := context.Background()
	mc := readMetaConflict(t, mainDB)
	require.Equal(t, coll, mc["name"])
	cmd := bson.D{
		{Key: "doltResolveConflict", Value: 1},
		{Key: "collection", Value: coll},
		{Key: "conflictId", Value: mc["conflictId"]},
		{Key: "resolution", Value: resolution},
	}
	if value != nil {
		cmd = append(cmd, bson.E{Key: "value", Value: value})
	}
	require.NoError(t, mainDB.RunCommand(ctx, cmd).Err())
	require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltMerge", Value: 1}, {Key: "continue", Value: 1}}).Err())
}

// setupMetaConflict creates a validated "items", diverges its validator on main
// (age>=21) and feature (age>=18), and merges feature into main -- leaving a
// paused metadata conflict. Returns the @main database.
func setupMetaConflict(t *testing.T, env *dumboDBTestEnv, dbName string) *mongo.Database {
	t.Helper()
	ctx := context.Background()
	db := env.Client.Database(dbName)
	require.NoError(t, db.Drop(ctx))
	require.NoError(t, db.CreateCollection(ctx, "items", options.CreateCollection().SetValidator(valNonNegAge)))
	dumboDBCommit(t, env, dbName, "create validated items", "alice <alice@acme.com>")
	vmBranch(t, env, dbName, "feature")

	require.NoError(t, env.Client.Database(dbName+"@feature").RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(18)},
	}).Err())
	dumboDBCommit(t, env, dbName+"@feature", "feature: age >= 18", "bob <bob@widgets.io>")

	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(21)},
	}).Err())
	dumboDBCommit(t, env, dbName, "main: age >= 21", "alice <alice@acme.com>")

	raw := vmMerge(t, env, dbName, "feature")
	require.EqualValues(t, 0, raw["ok"], "divergent metadata must surface a conflict: %v", raw)
	return env.Client.Database(dbName + "@main")
}

// readMetaConflict returns the single metadata conflict from doltConflicts,
// asserting it is attributed to a collection and never exposes the catalog.
func readMetaConflict(t *testing.T, mainDB *mongo.Database) bson.M {
	t.Helper()
	var rc bson.M
	require.NoError(t, mainDB.RunCommand(context.Background(), bson.D{{Key: "doltConflicts", Value: 1}}).Decode(&rc))
	all, ok := rc["conflicts"].(bson.A)
	require.True(t, ok, "doltConflicts missing conflicts array: %v", rc)
	var meta []bson.M
	for _, c := range all {
		e := c.(bson.M)
		assert.NotEqual(t, "__dumbo_catalog__", e["name"], "internal catalog must never be surfaced")
		if e["type"] == "metadata" {
			meta = append(meta, e)
		}
	}
	require.Len(t, meta, 1, "expected exactly one metadata conflict: %v", all)
	return meta[0]
}

// assertAgeGte checks a conflict side carries an { age: { $gte: n } } validator.
func assertAgeGte(t *testing.T, side interface{}, n int32) {
	t.Helper()
	m, ok := side.(bson.M)
	require.True(t, ok, "conflict side is not a document: %T", side)
	v, ok := m["validator"].(bson.M)
	require.True(t, ok, "side has no validator: %v", m)
	age, _ := v["age"].(bson.M)
	assert.EqualValues(t, n, age["$gte"])
}

func TestValidatorVerify(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	suffix := rand.Int64N(1_000_000)

	t.Run("Scenario1_EnforcementActive", func(t *testing.T) {
		db := env.Client.Database(fmt.Sprintf("valenf%d", suffix))
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge)))
		coll := db.Collection("items")

		_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(5)}})
		require.NoError(t, err, "valid insert passes")
		_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 2}, {Key: "age", Value: int32(-1)}})
		require.Error(t, err, "invalid insert rejected")
		assert.EqualValues(t, valDocValidationFailure, valErrCode(err))
	})

	t.Run("Scenario5_SurvivesRestart", func(t *testing.T) {
		// Own env: restarting it (and the cleanup Restart ties to this subtest)
		// must not disturb the shared server used by the other subtests.
		renv := startDumboDB(t)
		dbName := fmt.Sprintf("valrestart%d", suffix)
		db := renv.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge).SetValidationLevel("strict")))

		renv.Restart(t)
		db = renv.Client.Database(dbName) // client refreshed by Restart
		coll := db.Collection("items")

		assert.NotNil(t, validatorOf(t, db, "items"), "validator survives restart")
		_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 3}, {Key: "age", Value: int32(-1)}})
		require.Error(t, err, "validator still enforces after restart")
		assert.EqualValues(t, valDocValidationFailure, valErrCode(err))
		_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 4}, {Key: "age", Value: int32(5)}})
		require.NoError(t, err, "valid insert succeeds after restart")
	})

	t.Run("Scenario6_BranchCarriesValidator", func(t *testing.T) {
		dbName := fmt.Sprintf("valbranch%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge)))
		dumboDBCommit(t, env, dbName, "create validated items", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

		featDB := env.Client.Database(dbName + "@feature")
		assert.NotNil(t, validatorOf(t, featDB, "items"), "branch carries the validator")
		_, err := featDB.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(-1)}})
		require.Error(t, err, "validator enforces on the branch")
		assert.EqualValues(t, valDocValidationFailure, valErrCode(err))
	})

	t.Run("Scenario7_MergeCarriesAddedValidator", func(t *testing.T) {
		dbName := fmt.Sprintf("valmerge%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items"))
		dumboDBCommit(t, env, dbName, "create items", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

		// Feature adds the validator.
		featDB := env.Client.Database(dbName + "@feature")
		require.NoError(t, featDB.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"},
			{Key: "validator", Value: valNonNegAge},
			{Key: "validationLevel", Value: "strict"},
			{Key: "validationAction", Value: "error"},
		}).Err())
		dumboDBCommit(t, env, dbName+"@feature", "feature: add validator", "bob <bob@widgets.io>")

		// Main advances independently -> a real 3-way merge.
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 100}, {Key: "age", Value: int32(1)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: add a doc", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		assert.EqualValues(t, 1, raw["ok"], "merge completes: %v", raw)
		assert.NotEqual(t, "fast-forward", raw["message"], "must be a real merge, not fast-forward")

		mainDB := env.Client.Database(dbName + "@main")
		assert.NotNil(t, validatorOf(t, mainDB, "items"), "merged validator is active on main")
		_, err = mainDB.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 2}, {Key: "age", Value: int32(-1)}})
		require.Error(t, err, "merged validator enforces on main")
		assert.EqualValues(t, valDocValidationFailure, valErrCode(err))
	})

	// Scenario8/9: divergent validators on both branches surface as a resolvable
	// metadata conflict ON THE OWNING COLLECTION (never __dumbo_catalog__),
	// resolved via doltConflicts / doltResolveConflict, then doltMerge continue
	// completes with the chosen validator.
	t.Run("Scenario8_DivergentValidators_ResolveTheirs", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict8_%d", suffix)
		mainDB := setupMetaConflict(t, env, dbName)

		mc := readMetaConflict(t, mainDB)
		assert.Equal(t, "items", mc["name"], "conflict surfaced on the owning collection")
		assertAgeGte(t, mc["ours"], 21)
		assertAgeGte(t, mc["theirs"], 18)

		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "doltResolveConflict", Value: 1},
			{Key: "collection", Value: "items"},
			{Key: "conflictId", Value: mc["conflictId"]},
			{Key: "resolution", Value: "theirs"},
		}).Err())
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{{Key: "doltMerge", Value: 1}, {Key: "continue", Value: 1}}).Err())

		age, _ := validatorOf(t, env.Client.Database(dbName+"@main"), "items")["age"].(bson.M)
		assert.EqualValues(t, 18, age["$gte"], "theirs (age >= 18) won after resolution")
	})

	t.Run("Scenario9_DivergentValidators_ResolveCustom", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict9_%d", suffix)
		mainDB := setupMetaConflict(t, env, dbName)
		resolveMeta(t, mainDB, "items", "custom", bson.D{{Key: "validator", Value: ageGte(5)}})

		age, _ := validatorOf(t, env.Client.Database(dbName+"@main"), "items")["age"].(bson.M)
		assert.EqualValues(t, 5, age["$gte"], "custom validator (age >= 5) applied after resolution")
	})

	// Scenario10: structurally-different $jsonSchema validators diverge; resolve
	// "ours" keeps main's schema.
	t.Run("Scenario10_JsonSchemaDivergence_ResolveOurs", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict10_%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(jsonSchemaRequiring("name"))))
		dumboDBCommit(t, env, dbName, "create items requiring name", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

		require.NoError(t, env.Client.Database(dbName+"@feature").RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: jsonSchemaRequiring("email")},
		}).Err())
		dumboDBCommit(t, env, dbName+"@feature", "feature: require email", "bob <bob@widgets.io>")
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: jsonSchemaRequiring("age")},
		}).Err())
		dumboDBCommit(t, env, dbName, "main: require age", "alice <alice@acme.com>")

		mainDB := env.Client.Database(dbName + "@main")
		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "divergent $jsonSchema must conflict: %v", raw)

		resolveMeta(t, mainDB, "items", "ours", nil)
		assert.Equal(t, "age", requiredField(t, validatorOf(t, mainDB, "items")),
			"resolving ours keeps main's $jsonSchema (required: age)")
	})

	// Scenario11: divergent validationAction (with validator) resolves via theirs;
	// the action follows the chosen side.
	t.Run("Scenario11_DivergentValidationAction_ResolveTheirs", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict11_%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge).SetValidationAction("error")))
		dumboDBCommit(t, env, dbName, "create validated items", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

		// Feature: validator age>=1, action warn.
		require.NoError(t, env.Client.Database(dbName+"@feature").RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(1)}, {Key: "validationAction", Value: "warn"},
		}).Err())
		dumboDBCommit(t, env, dbName+"@feature", "feature: age>=1 warn", "bob <bob@widgets.io>")
		// Main: validator age>=2, action error.
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(2)}, {Key: "validationAction", Value: "error"},
		}).Err())
		dumboDBCommit(t, env, dbName, "main: age>=2 error", "alice <alice@acme.com>")

		mainDB := env.Client.Database(dbName + "@main")
		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "divergent metadata must conflict: %v", raw)

		resolveMeta(t, mainDB, "items", "theirs", nil)
		age, _ := validatorOf(t, mainDB, "items")["age"].(bson.M)
		assert.EqualValues(t, 1, age["$gte"], "theirs validator (age>=1) applied")
		assert.Equal(t, "warn", validationActionOf(t, mainDB, "items"), "theirs validationAction (warn) applied")
	})

	// Scenario12: both branches CREATE the same collection with different
	// validators (add/add). base is null; resolve theirs.
	t.Run("Scenario12_BothBranchCreate_ResolveTheirs", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict12_%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "seed")) // base collection so the catalog exists
		dumboDBCommit(t, env, dbName, "seed", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

		require.NoError(t, env.Client.Database(dbName+"@feature").CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(ageGte(18))))
		dumboDBCommit(t, env, dbName+"@feature", "feature: create items age>=18", "bob <bob@widgets.io>")
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(ageGte(21))))
		dumboDBCommit(t, env, dbName, "main: create items age>=21", "alice <alice@acme.com>")

		mainDB := env.Client.Database(dbName + "@main")
		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "both-branch create with divergent validators must conflict: %v", raw)

		mc := readMetaConflict(t, mainDB)
		assert.Nil(t, mc["base"], "base side is null for an add/add metadata conflict")

		resolveMeta(t, mainDB, "items", "theirs", nil)
		age, _ := validatorOf(t, mainDB, "items")["age"].(bson.M)
		assert.EqualValues(t, 18, age["$gte"], "theirs (age>=18) won for the concurrently-created collection")
	})

	// Scenario13: one branch drops the collection while the other modifies its
	// metadata -- a modify/delete metadata conflict (theirs side null). Resolving
	// theirs applies the deletion: the collection is gone, with no orphaned
	// metadata left behind.
	t.Run("Scenario13_DropVsMetadata_ResolveTheirs", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict13_%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge)))
		dumboDBCommit(t, env, dbName, "create validated items", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

		// Feature drops items; main changes its validator.
		require.NoError(t, env.Client.Database(dbName+"@feature").Collection("items").Drop(ctx))
		dumboDBCommit(t, env, dbName+"@feature", "feature: drop items", "bob <bob@widgets.io>")
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(21)},
		}).Err())
		dumboDBCommit(t, env, dbName, "main: age >= 21", "alice <alice@acme.com>")

		mainDB := env.Client.Database(dbName + "@main")
		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "drop-vs-metadata must conflict: %v", raw)

		mc := readMetaConflict(t, mainDB)
		assert.Nil(t, mc["theirs"], "theirs dropped the collection -> theirs side is null")

		resolveMeta(t, mainDB, "items", "theirs", nil)

		// items is gone, and it does not reappear (no orphaned metadata).
		var lc bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "listCollections", Value: 1},
			{Key: "filter", Value: bson.D{{Key: "name", Value: "items"}}},
		}).Decode(&lc))
		batch := lc["cursor"].(bson.M)["firstBatch"].(bson.A)
		assert.Len(t, batch, 0, "resolving theirs (drop) leaves no items collection")
	})

	// Scenario14: same modify/delete conflict, but resolve OURS -- keep the
	// collection with main's modified validator. The dropped DTBL is restored so
	// existence and metadata agree.
	t.Run("Scenario14_DropVsMetadata_ResolveOurs", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict14_%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge)))
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(5)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "create validated items + doc", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

		require.NoError(t, env.Client.Database(dbName+"@feature").Collection("items").Drop(ctx))
		dumboDBCommit(t, env, dbName+"@feature", "feature: drop items", "bob <bob@widgets.io>")
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(21)},
		}).Err())
		dumboDBCommit(t, env, dbName, "main: age >= 21", "alice <alice@acme.com>")

		mainDB := env.Client.Database(dbName + "@main")
		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "drop-vs-metadata must conflict: %v", raw)

		resolveMeta(t, mainDB, "items", "ours", nil)

		// The collection is restored with main's validator and its document.
		age, _ := validatorOf(t, mainDB, "items")["age"].(bson.M)
		assert.EqualValues(t, 21, age["$gte"], "ours validator (age>=21) kept")
		n, err := mainDB.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 1, n, "resolving ours restores the collection and its data")
	})

	// Scenario15: both branches make the SAME validator change. A divergent
	// change conflicts (Scenarios 8-14); an identical one converges and merges
	// cleanly with no conflict -- the only case a metadata change is resolved
	// without asking.
	t.Run("Scenario15_ConvergentValidatorChange_NoConflict", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict15_%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge)))
		dumboDBCommit(t, env, dbName, "create validated items", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

		// Both sides tighten the validator to the IDENTICAL definition age >= 10.
		require.NoError(t, env.Client.Database(dbName+"@feature").RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(10)},
		}).Err())
		dumboDBCommit(t, env, dbName+"@feature", "feature: age >= 10", "bob <bob@widgets.io>")
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(10)},
		}).Err())
		dumboDBCommit(t, env, dbName, "main: age >= 10", "alice <alice@acme.com>")

		mainDB := env.Client.Database(dbName + "@main")
		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 1, raw["ok"], "identical validator change must merge cleanly: %v", raw)

		age, _ := validatorOf(t, mainDB, "items")["age"].(bson.M)
		assert.EqualValues(t, 10, age["$gte"], "converged validator (age>=10) active on main")
		_, err := mainDB.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(5)}})
		require.Error(t, err, "converged validator enforces (age 5 < 10 rejected)")
	})

	// Scenario16: doltDiff surfaces a validator on an added collection under the
	// change's `metadata` field (from: null, to: the validator/options).
	t.Run("Scenario16_DiffShowsValidatorOnAddedCollection", func(t *testing.T) {
		dbName := fmt.Sprintf("valdiff16_%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge).SetValidationLevel("strict")))
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 2}, {Key: "age", Value: int32(1)}})
		require.NoError(t, err)

		diff := runCommandRaw(t, db, bson.D{{Key: "doltDiff", Value: 1}})
		changes := diff["changes"].(bson.A)
		require.Len(t, changes, 1)
		ch := changes[0].(bson.M)
		assert.Equal(t, "items", ch["name"])
		assert.Equal(t, "added", ch["status"])

		meta, ok := ch["metadata"].(bson.M)
		require.True(t, ok, "metadata present: %v", ch["metadata"])
		assert.Nil(t, meta["from"], "added collection has no prior metadata")
		to, ok := meta["to"].(bson.M)
		require.True(t, ok, "metadata.to present: %v", meta)
		v, _ := to["validator"].(bson.M)
		age, _ := v["age"].(bson.M)
		assert.EqualValues(t, 0, age["$gte"], "diff shows the validator (age>=0)")
		assert.Equal(t, "strict", to["validationLevel"])
	})

	// Scenario17: a collMod that only changes the validator (no doc/index change)
	// surfaces as a modified collection with metadata {from, to}.
	t.Run("Scenario17_DiffShowsValidatorChange", func(t *testing.T) {
		dbName := fmt.Sprintf("valdiff17_%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(ageGte(0))))
		dumboDBCommit(t, env, dbName, "create validated items", "alice <alice@acme.com>")

		// collMod only -- no document or index change.
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(10)},
		}).Err())

		diff := runCommandRaw(t, db, bson.D{{Key: "doltDiff", Value: 1}})
		changes := diff["changes"].(bson.A)
		require.Len(t, changes, 1, "a validator-only change must still surface: %v", diff)
		ch := changes[0].(bson.M)
		assert.Equal(t, "modified", ch["status"])
		meta := ch["metadata"].(bson.M)
		from := meta["from"].(bson.M)
		to := meta["to"].(bson.M)
		fromAge, _ := from["validator"].(bson.M)["age"].(bson.M)
		toAge, _ := to["validator"].(bson.M)["age"].(bson.M)
		assert.EqualValues(t, 0, fromAge["$gte"], "from validator age>=0")
		assert.EqualValues(t, 10, toAge["$gte"], "to validator age>=10")
	})
}
