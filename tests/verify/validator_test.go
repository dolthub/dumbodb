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

var valNonNegAge = bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: int32(0)}}}}

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

func ageGte(n int32) bson.D {
	return bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: n}}}}
}

func jsonSchemaRequiring(field string) bson.D {
	return bson.D{{Key: "$jsonSchema", Value: bson.D{
		{Key: "bsonType", Value: "object"},
		{Key: "required", Value: bson.A{field}},
		{Key: "properties", Value: bson.D{{Key: field, Value: bson.D{{Key: "bsonType", Value: "string"}}}}},
	}}}
}

func requiredField(t *testing.T, v bson.M) string {
	t.Helper()
	js, ok := v["$jsonSchema"].(bson.M)
	require.True(t, ok, "not a $jsonSchema validator: %v", v)
	req, _ := js["required"].(bson.A)
	require.Len(t, req, 1, "expected one required field: %v", js)
	return req[0].(string)
}

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

	t.Run("Scenario1_SurvivesRestart", func(t *testing.T) {
		renv := startDumboDB(t)
		dbName := fmt.Sprintf("valrestart%d", suffix)
		db := renv.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge).SetValidationLevel("strict")))

		_, err := renv.Client.Database(dbName).Collection("items").
			InsertOne(ctx, bson.D{{Key: "_id", Value: 2}, {Key: "age", Value: int32(-1)}})
		require.Error(t, err, "validator active before restart")
		assert.EqualValues(t, valDocValidationFailure, valErrCode(err))

		renv.Restart(t)
		db = renv.Client.Database(dbName)
		coll := db.Collection("items")

		assert.NotNil(t, validatorOf(t, db, "items"), "validator survives restart")
		_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 3}, {Key: "age", Value: int32(-1)}})
		require.Error(t, err, "validator still enforces after restart")
		assert.EqualValues(t, valDocValidationFailure, valErrCode(err))
		_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 4}, {Key: "age", Value: int32(5)}})
		require.NoError(t, err, "valid insert succeeds after restart")
	})

	t.Run("Scenario2_BranchCarriesValidator", func(t *testing.T) {
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

	t.Run("Scenario3_MergeCarriesAddedValidator", func(t *testing.T) {
		dbName := fmt.Sprintf("valmerge%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items"))
		dumboDBCommit(t, env, dbName, "create items", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

		featDB := env.Client.Database(dbName + "@feature")
		require.NoError(t, featDB.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"},
			{Key: "validator", Value: valNonNegAge},
			{Key: "validationLevel", Value: "strict"},
			{Key: "validationAction", Value: "error"},
		}).Err())
		dumboDBCommit(t, env, dbName+"@feature", "feature: add validator", "bob <bob@widgets.io>")

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

	// Divergent validators on both branches surface as a resolvable metadata
	// conflict on the owning collection, never __dumbo_catalog__.
	t.Run("Scenario4_DivergentValidators_ResolveTheirs", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict8_%d", suffix)
		mainDB := setupMetaConflict(t, env, dbName)

		mc := readMetaConflict(t, mainDB)
		assert.Equal(t, "items", mc["name"], "conflict surfaced on the owning collection")
		assertAgeGte(t, mc["ours"], 21)
		assertAgeGte(t, mc["theirs"], 18)

		reason := mc["reason"].(bson.M)
		assert.Equal(t, "bothModified", reason["code"], "both branches changed the validator")
		assert.Contains(t, reason["message"], "both changed the validator/options",
			"reason names the divergence: %v", reason["message"])

		ours := mc["ours"].(bson.M)
		assert.Equal(t, "strict", ours["validationLevel"], "effective default level")
		assert.Equal(t, "error", ours["validationAction"], "effective default action")

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

	t.Run("Scenario4_DivergentValidators_ResolveCustom", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict9_%d", suffix)
		mainDB := setupMetaConflict(t, env, dbName)
		resolveMeta(t, mainDB, "items", "custom", bson.D{{Key: "validator", Value: ageGte(5)}})

		age, _ := validatorOf(t, env.Client.Database(dbName+"@main"), "items")["age"].(bson.M)
		assert.EqualValues(t, 5, age["$gte"], "custom validator (age >= 5) applied after resolution")
	})

	t.Run("Scenario4_JsonSchemaDivergence_ResolveOurs", func(t *testing.T) {
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

	t.Run("Scenario4_DivergentValidationAction_ResolveTheirs", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict11_%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge).SetValidationAction("error")))
		dumboDBCommit(t, env, dbName, "create validated items", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

		require.NoError(t, env.Client.Database(dbName+"@feature").RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(1)}, {Key: "validationAction", Value: "warn"},
		}).Err())
		dumboDBCommit(t, env, dbName+"@feature", "feature: age>=1 warn", "bob <bob@widgets.io>")
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

	// Both branches create the collection (add/add): the conflict's base is null.
	t.Run("Scenario4_BothBranchCreate_ResolveTheirs", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict12_%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "seed"))
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

	// Modify/delete metadata conflict (theirs side null): resolving theirs deletes
	// the collection, leaving no orphaned metadata.
	t.Run("Scenario4_DropVsMetadata_ResolveTheirs", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict13_%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge)))
		dumboDBCommit(t, env, dbName, "create validated items", "alice <alice@acme.com>")
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

		mc := readMetaConflict(t, mainDB)
		assert.Nil(t, mc["theirs"], "theirs dropped the collection -> theirs side is null")

		resolveMeta(t, mainDB, "items", "theirs", nil)

		var lc bson.M
		require.NoError(t, mainDB.RunCommand(ctx, bson.D{
			{Key: "listCollections", Value: 1},
			{Key: "filter", Value: bson.D{{Key: "name", Value: "items"}}},
		}).Decode(&lc))
		batch := lc["cursor"].(bson.M)["firstBatch"].(bson.A)
		assert.Len(t, batch, 0, "resolving theirs (drop) leaves no items collection")
	})

	// Same modify/delete conflict resolved OURS: the dropped table is restored so
	// existence and metadata agree.
	t.Run("Scenario4_DropVsMetadata_ResolveOurs", func(t *testing.T) {
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

		age, _ := validatorOf(t, mainDB, "items")["age"].(bson.M)
		assert.EqualValues(t, 21, age["$gte"], "ours validator (age>=21) kept")
		n, err := mainDB.Collection("items").CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 1, n, "resolving ours restores the collection and its data")
	})

	// Both branches make the IDENTICAL validator change: it converges and merges
	// cleanly -- the only case a metadata change resolves without asking.
	t.Run("Scenario4_ConvergentValidatorChange_NoConflict", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict15_%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge)))
		dumboDBCommit(t, env, dbName, "create validated items", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

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

	// A collMod that changes ONLY the validator still surfaces under the metadata
	// field in doltDiff, doltStatus, and doltLog.
	t.Run("Scenario8_ValidatorVisibleInDiffStatusLog", func(t *testing.T) {
		dbName := fmt.Sprintf("valobserve%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))

		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge).SetValidationLevel("strict")))

		// metaEntries returns the metadata field diffs of the sole change entry.
		metaEntries := func(changes bson.A, wantStatus string) []bson.M {
			require.Len(t, changes, 1)
			ch := changes[0].(bson.M)
			assert.Equal(t, "items", ch["name"])
			assert.Equal(t, wantStatus, ch["status"])
			raw, _ := ch["metadata"].(bson.M)["diff"].(bson.A)
			out := make([]bson.M, 0, len(raw))
			for _, e := range raw {
				out = append(out, e.(bson.M))
			}
			return out
		}

		// taggedPaths renders each entry as "<type> <path>" for comparison.
		taggedPaths := func(entries []bson.M) []string {
			out := make([]string, 0, len(entries))
			for _, e := range entries {
				out = append(out, fmt.Sprintf("%s %s", e["type"], e["path"]))
			}
			return out
		}

		addedPaths := []string{"added $.validator", "added $.validationLevel", "added $.validationAction"}

		status := runCommandRaw(t, db, bson.D{{Key: "doltStatus", Value: 1}})
		assert.Equal(t, true, status["dirty"])
		statusAdded := metaEntries(status["changes"].(bson.A), "added")
		assert.Equal(t, addedPaths, taggedPaths(statusAdded))
		assert.NotContains(t, statusAdded[0], "to", "summary verbosity names paths only")

		diff := runCommandRaw(t, db, bson.D{{Key: "doltDiff", Value: 1}})
		diffAdded := metaEntries(diff["changes"].(bson.A), "added")
		assert.Equal(t, addedPaths, taggedPaths(diffAdded))
		assert.NotContains(t, diffAdded[0], "from", "added collection has no prior metadata")
		age, _ := diffAdded[0]["to"].(bson.M)["age"].(bson.M)
		assert.EqualValues(t, 0, age["$gte"])
		assert.Equal(t, "strict", diffAdded[1]["to"])

		dumboDBCommit(t, env, dbName, "create validated items", "alice <alice@acme.com>")

		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(10)},
		}).Err())

		// Only the validator moved, so the untouched level and action drop out.
		modifiedPaths := []string{"modified $.validator"}

		// assertValidatorValues checks the from/to sides carried at full verbosity.
		assertValidatorValues := func(entry bson.M) {
			fromAge, _ := entry["from"].(bson.M)["age"].(bson.M)
			toAge, _ := entry["to"].(bson.M)["age"].(bson.M)
			assert.EqualValues(t, 0, fromAge["$gte"])
			assert.EqualValues(t, 10, toAge["$gte"])
		}

		status = runCommandRaw(t, db, bson.D{{Key: "doltStatus", Value: 1}})
		assert.Equal(t, true, status["dirty"], "validator-only change makes the workspace dirty")
		statusModified := metaEntries(status["changes"].(bson.A), "modified")
		assert.Equal(t, modifiedPaths, taggedPaths(statusModified))
		assert.NotContains(t, statusModified[0], "from", "summary verbosity names paths only")

		diff = runCommandRaw(t, db, bson.D{{Key: "doltDiff", Value: 1}})
		diffModified := metaEntries(diff["changes"].(bson.A), "modified")
		assert.Equal(t, modifiedPaths, taggedPaths(diffModified))
		assertValidatorValues(diffModified[0])

		dumboDBCommit(t, env, dbName, "tighten validator to age >= 10", "alice <alice@acme.com>")
		for _, verbosity := range []string{"stat", "patch"} {
			log := runCommandRaw(t, db, bson.D{
				{Key: "doltLog", Value: 1}, {Key: "limit", Value: 1}, {Key: verbosity, Value: true},
			})
			commits := log["commits"].(bson.A)
			require.Len(t, commits, 1)
			entries := metaEntries(commits[0].(bson.M)["changes"].(bson.A), "modified")
			assert.Equal(t, modifiedPaths, taggedPaths(entries), verbosity)
			if verbosity == "patch" {
				assertValidatorValues(entries[0])
			} else {
				assert.NotContains(t, entries[0], "from", "stat names paths only")
			}
		}
	})
}
