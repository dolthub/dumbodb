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
	require.Equal(t, coll, mc["collection"])
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
		assert.NotEqual(t, "__dumbo_catalog__", e["collection"], "internal catalog must never be surfaced")
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
		assert.Equal(t, "items", mc["collection"], "conflict surfaced on the owning collection")
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

		// Only the one leaf inside the validator moved, so the untouched level
		// and action drop out, as does the rest of the expression.
		modifiedPaths := []string{"modified $.validator.age.$gte"}

		// assertValidatorValues checks the from/to sides carried at full verbosity.
		assertValidatorValues := func(entry bson.M) {
			assert.EqualValues(t, 0, entry["from"])
			assert.EqualValues(t, 10, entry["to"])
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

// Merge cross-validation scenarios exercise configuration-first ordering and
// the document, index, and validation conflicts created by phase two.
func conflictsByType(t *testing.T, mainDB *mongo.Database, typ string) []bson.M {
	t.Helper()
	var rc bson.M
	require.NoError(t, mainDB.RunCommand(context.Background(), bson.D{{Key: "doltConflicts", Value: 1}}).Decode(&rc))
	all, _ := rc["conflicts"].(bson.A)
	var out []bson.M
	for _, c := range all {
		e := c.(bson.M)
		assert.NotEqual(t, "__dumbo_catalog__", e["collection"], "internal catalog must never surface")
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

func resolveConflict(t *testing.T, mainDB *mongo.Database, coll, conflictID, resolution string, value bson.D) error {
	cmd := bson.D{
		{Key: "doltResolveConflict", Value: 1},
		{Key: "collection", Value: coll},
		{Key: "conflictId", Value: conflictID},
		{Key: "resolution", Value: resolution},
	}
	if value != nil {
		cmd = append(cmd, bson.E{Key: "value", Value: value})
	}
	return mainDB.RunCommand(context.Background(), cmd).Err()
}

func continueMerge(t *testing.T, mainDB *mongo.Database) {
	t.Helper()
	require.NoError(t, mainDB.RunCommand(context.Background(),
		bson.D{{Key: "doltMerge", Value: 1}, {Key: "continue", Value: 1}}).Err())
}

func ageOf(t *testing.T, coll *mongo.Collection, id int) (int32, bool) {
	t.Helper()
	var got bson.M
	err := coll.FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Decode(&got)
	if err != nil {
		return 0, false
	}
	v, _ := got["age"].(int32)
	return v, true
}

func TestValidatorMergeCrossValidation(t *testing.T) {
	suiteT := t
	env := startDumboDB(t)
	ctx := context.Background()
	suffix := rand.Int64N(1_000_000)
	newDB := func(tag string) (string, *mongo.Database) {
		name := fmt.Sprintf("xval%s%d", tag, suffix)
		db := env.Client.Database(name)
		require.NoError(t, db.Drop(ctx))
		return name, db
	}

	addValidatorOnFeature := func(t *testing.T, dbName string, action string) {
		vmBranch(t, env, dbName, "feature")
		collmod := bson.D{{Key: "collMod", Value: "items"}, {Key: "validator", Value: valNonNegAge}}
		if action != "" {
			collmod = append(collmod, bson.E{Key: "validationAction", Value: action})
		}
		require.NoError(t, env.Client.Database(dbName+"@feature").RunCommand(ctx, collmod).Err())
		dumboDBCommit(t, env, dbName+"@feature", "feature: require age>=0", "bob <bob@widgets.io>")
	}

	t.Run("Scenario5_DataViolation_ResolveCustom", func(t *testing.T) {
		dbName, db := newDB("s5")
		require.NoError(t, db.CreateCollection(ctx, "items"))
		dumboDBCommit(t, env, dbName, "create items", "alice <alice@acme.com>")
		addValidatorOnFeature(t, dbName, "error")

		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(-5)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: insert age -5", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "violating insert must conflict: %v", raw)

		mainDB := env.Client.Database(dbName + "@main")
		vals := conflictsByType(t, mainDB, "validation")
		require.Len(t, vals, 1)
		vc := vals[0]
		assert.Equal(t, "items", vc["collection"])
		assert.EqualValues(t, 1, vc["documentId"])
		require.NotNil(t, vc["validator"], "conflict carries the violated validator")

		require.Error(t, resolveConflict(t, mainDB, "items", vc["conflictId"].(string), "custom",
			bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(-1)}}),
			"a still-violating custom value must be rejected")

		require.NoError(t, resolveConflict(t, mainDB, "items", vc["conflictId"].(string), "custom",
			bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(0)}}))
		continueMerge(t, mainDB)
		age, ok := ageOf(t, mainDB.Collection("items"), 1)
		require.True(t, ok)
		assert.EqualValues(t, 0, age)
	})

	t.Run("Cell6a_InsertViolator_ResolveCustom", func(t *testing.T) {
		dbName, db := newDB("c6a")
		require.NoError(t, db.CreateCollection(ctx, "items"))
		dumboDBCommit(t, env, dbName, "create items", "alice <alice@acme.com>")
		addValidatorOnFeature(t, dbName, "")

		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(-5)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: insert age -5", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "violating insert must conflict: %v", raw)

		mainDB := env.Client.Database(dbName + "@main")
		vals := conflictsByType(t, mainDB, "validation")
		require.Len(t, vals, 1)
		assert.EqualValues(t, 1, vals[0]["documentId"])
		require.NoError(t, resolveConflict(t, mainDB, "items", vals[0]["conflictId"].(string), "custom",
			bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(5)}}))
		continueMerge(t, mainDB)
		age, ok := ageOf(t, mainDB.Collection("items"), 1)
		require.True(t, ok)
		assert.EqualValues(t, 5, age)
	})

	t.Run("CleanInsertConforming_NoConflict", func(t *testing.T) {
		dbName, db := newDB("insok")
		require.NoError(t, db.CreateCollection(ctx, "items"))
		dumboDBCommit(t, env, dbName, "create items", "alice <alice@acme.com>")
		addValidatorOnFeature(t, dbName, "")

		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(5)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: insert age 5", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 1, raw["ok"], "conforming insert must merge cleanly: %v", raw)
	})

	t.Run("ModifyConformingToViolating_Conflict_ResolveDrop", func(t *testing.T) {
		dbName, db := newDB("mod2viol")
		require.NoError(t, db.CreateCollection(ctx, "items"))
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(5)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "create items with age 5", "alice <alice@acme.com>")
		addValidatorOnFeature(t, dbName, "")

		_, err = db.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(-5)}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: age -> -5", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "modify-to-violating must conflict: %v", raw)

		mainDB := env.Client.Database(dbName + "@main")
		vals := conflictsByType(t, mainDB, "validation")
		require.Len(t, vals, 1)
		require.NoError(t, resolveConflict(t, mainDB, "items", vals[0]["conflictId"].(string), "drop", nil))
		continueMerge(t, mainDB)
		_, ok := ageOf(t, mainDB.Collection("items"), 1)
		assert.False(t, ok, "dropped document is gone")
	})

	t.Run("GrandfatherBaseViolator_CleanCarryForward_NoConflict", func(t *testing.T) {
		dbName, db := newDB("grand")
		require.NoError(t, db.CreateCollection(ctx, "items"))
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(-5)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "create items with grandfathered age -5", "alice <alice@acme.com>")
		addValidatorOnFeature(t, dbName, "")

		_, err = db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 2}, {Key: "age", Value: int32(9)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: add conforming doc", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 1, raw["ok"], "grandfathered base violator must not conflict: %v", raw)
		mainDB := env.Client.Database(dbName + "@main")
		age, ok := ageOf(t, mainDB.Collection("items"), 1)
		require.True(t, ok, "grandfathered doc survives the merge")
		assert.EqualValues(t, -5, age)
	})

	// Grandfathering under validationAction "error" is limited to byte-for-byte
	// unchanged documents: re-authoring a base violator to another violating value
	// conflicts.
	t.Run("BaseViolator_OneSidedChange_StillViolating_Error_Conflict", func(t *testing.T) {
		dbName, db := newDB("x1sideErr")
		require.NoError(t, db.CreateCollection(ctx, "items"))
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(-5)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "create items with age -5", "alice <alice@acme.com>")
		addValidatorOnFeature(t, dbName, "")

		_, err = db.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(-9)}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: age -> -9", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "re-authored violating value conflicts under error: %v", raw)
		mainDB := env.Client.Database(dbName + "@main")
		vals := conflictsByType(t, mainDB, "validation")
		require.Len(t, vals, 1)
		require.NoError(t, resolveConflict(t, mainDB, "items", vals[0]["conflictId"].(string), "drop", nil))
		continueMerge(t, mainDB)
		_, ok := ageOf(t, mainDB.Collection("items"), 1)
		assert.False(t, ok, "offender dropped")
	})

	t.Run("BaseViolator_OneSidedChange_StillViolating_Warn_Allowed", func(t *testing.T) {
		dbName, db := newDB("x1sideWarn")
		require.NoError(t, db.CreateCollection(ctx, "items"))
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(-5)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "create items with age -5", "alice <alice@acme.com>")
		addValidatorOnFeature(t, dbName, "warn")

		_, err = db.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(-9)}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: age -> -9", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 1, raw["ok"], "warn allows the re-authored violating value: %v", raw)
		mainDB := env.Client.Database(dbName + "@main")
		age, ok := ageOf(t, mainDB.Collection("items"), 1)
		require.True(t, ok)
		assert.EqualValues(t, -9, age)
	})

	t.Run("WarnAction_SuppressesConflict", func(t *testing.T) {
		dbName, db := newDB("warn")
		require.NoError(t, db.CreateCollection(ctx, "items"))
		dumboDBCommit(t, env, dbName, "create items", "alice <alice@acme.com>")
		addValidatorOnFeature(t, dbName, "warn")

		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(-5)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: insert age -5", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 1, raw["ok"], "warn action must let the merge through: %v", raw)
		mainDB := env.Client.Database(dbName + "@main")
		age, ok := ageOf(t, mainDB.Collection("items"), 1)
		require.True(t, ok)
		assert.EqualValues(t, -5, age, "violating doc kept under warn")
	})

	t.Run("Trigger2_DataConflictResolvedToViolating_Rejected", func(t *testing.T) {
		dbName, db := newDB("t2")
		require.NoError(t, db.CreateCollection(ctx, "items"))
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(5)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "create items age 5", "alice <alice@acme.com>")

		vmBranch(t, env, dbName, "feature")
		featDB := env.Client.Database(dbName + "@feature")
		require.NoError(t, featDB.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: valNonNegAge},
		}).Err())
		_, err = featDB.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(7)}, {Key: "tag", Value: "f"}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature: validator + age 7", "bob <bob@widgets.io>")

		_, err = db.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(-5)}, {Key: "tag", Value: "m"}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: age -5", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "divergent modify is a data conflict: %v", raw)

		mainDB := env.Client.Database(dbName + "@main")
		docs := conflictsByType(t, mainDB, "document")
		require.Len(t, docs, 1, "expected a document conflict on _id:1")
		cid := docs[0]["conflictId"].(string)

		require.Error(t, resolveConflict(t, mainDB, "items", cid, "ours", nil),
			"resolving a data conflict to a violating value must be rejected")

		require.NoError(t, resolveConflict(t, mainDB, "items", cid, "theirs", nil))
		continueMerge(t, mainDB)
		age, ok := ageOf(t, mainDB.Collection("items"), 1)
		require.True(t, ok)
		assert.EqualValues(t, 7, age)
	})

	t.Run("Trigger2_BaseViolator_DataConflict_ResolvedToViolating_Rejected", func(t *testing.T) {
		dbName, db := newDB("t2x")
		require.NoError(t, db.CreateCollection(ctx, "items"))
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(-5)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "create items age -5", "alice <alice@acme.com>")

		vmBranch(t, env, dbName, "feature")
		featDB := env.Client.Database(dbName + "@feature")
		_, err = featDB.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(-3)}, {Key: "tag", Value: "f"}}}})
		require.NoError(t, err)
		require.NoError(t, featDB.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: valNonNegAge},
		}).Err())
		dumboDBCommit(t, env, dbName+"@feature", "feature: age -3 + validator", "bob <bob@widgets.io>")

		_, err = db.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(-7)}, {Key: "tag", Value: "m"}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: age -7", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "divergent modify is a data conflict: %v", raw)

		mainDB := env.Client.Database(dbName + "@main")
		docs := conflictsByType(t, mainDB, "document")
		require.Len(t, docs, 1)
		cid := docs[0]["conflictId"].(string)

		require.Error(t, resolveConflict(t, mainDB, "items", cid, "ours", nil))
		require.Error(t, resolveConflict(t, mainDB, "items", cid, "theirs", nil))

		require.NoError(t, resolveConflict(t, mainDB, "items", cid, "custom",
			bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(2)}}))
		continueMerge(t, mainDB)
		age, ok := ageOf(t, mainDB.Collection("items"), 1)
		require.True(t, ok)
		assert.EqualValues(t, 2, age)
	})

	// workspace-h0w.5: when the validator DEFINITION conflicts AND a document
	// violates the resolved validator, the definition conflict resolves first, then
	// continuing re-pauses on the now-detectable document violation.
	t.Run("MetaConflictThenValidationConflict_TwoPhase", func(t *testing.T) {
		dbName, db := newDB("metathenval")
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge)))
		_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(5)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "create validated items + doc", "alice <alice@acme.com>")

		vmBranch(t, env, dbName, "feature")
		require.NoError(t, env.Client.Database(dbName+"@feature").RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(10)},
		}).Err())
		dumboDBCommit(t, env, dbName+"@feature", "feature: age >= 10", "bob <bob@widgets.io>")

		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(3)},
		}).Err())
		_, err = db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 2}, {Key: "age", Value: int32(5)}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: age >= 3 + doc age 5", "alice <alice@acme.com>")

		mainDB := env.Client.Database(dbName + "@main")
		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "divergent validator must conflict: %v", raw)

		require.Empty(t, conflictsByType(t, mainDB, "validation"), "doc check deferred until validator pinned")
		mc := conflictsByType(t, mainDB, "metadata")
		require.Len(t, mc, 1)
		require.NoError(t, resolveConflict(t, mainDB, "items", mc[0]["conflictId"].(string), "theirs", nil))

		cont := runCommandRaw(t, mainDB, bson.D{{Key: "doltMerge", Value: 1}, {Key: "continue", Value: 1}})
		require.EqualValues(t, 0, cont["ok"], "continue must re-pause on the validation conflict: %v", cont)

		vals := conflictsByType(t, mainDB, "validation")
		require.Len(t, vals, 1, "the deferred document violation surfaces after the validator is pinned")
		assert.EqualValues(t, 2, vals[0]["documentId"])

		require.NoError(t, resolveConflict(t, mainDB, "items", vals[0]["conflictId"].(string), "custom",
			bson.D{{Key: "_id", Value: 2}, {Key: "age", Value: int32(12)}}))
		continueMerge(t, mainDB)

		age2, ok := ageOf(t, mainDB.Collection("items"), 2)
		require.True(t, ok)
		assert.EqualValues(t, 12, age2, "violating doc fixed")
		age1, ok := ageOf(t, mainDB.Collection("items"), 1)
		require.True(t, ok)
		assert.EqualValues(t, 5, age1, "grandfathered unchanged doc survives")
	})

	t.Run("ValidatorConflictDefersCleanDocumentMerge", func(t *testing.T) {
		dbName, db := newDB("metadeferclean")
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge)))
		_, err := db.Collection("items").InsertOne(ctx, bson.D{
			{Key: "_id", Value: 1},
			{Key: "age", Value: int32(25)},
			{Key: "a", Value: "base"},
			{Key: "b", Value: "base"},
		})
		require.NoError(t, err)
		_, err = db.Collection("unaffected").InsertOne(ctx, bson.D{{Key: "_id", Value: 0}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "create validated items + doc", "alice <alice@acme.com>")

		vmBranch(t, env, dbName, "feature")
		featureDB := env.Client.Database(dbName + "@feature")
		require.NoError(t, featureDB.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(10)},
		}).Err())
		_, err = featureDB.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: "feature"}}}})
		require.NoError(t, err)
		_, err = featureDB.Collection("items").InsertOne(ctx,
			bson.D{{Key: "_id", Value: 2}, {Key: "age", Value: int32(12)}})
		require.NoError(t, err)
		_, err = featureDB.Collection("unaffected").InsertOne(ctx, bson.D{{Key: "_id", Value: 2}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature: validator + documents", "bob <bob@widgets.io>")

		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(3)},
		}).Err())
		_, err = db.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "b", Value: "main"}}}})
		require.NoError(t, err)
		_, err = db.Collection("unaffected").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: validator + document", "alice <alice@acme.com>")

		mainDB := env.Client.Database(dbName + "@main")
		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "divergent validator must conflict: %v", raw)
		require.Len(t, conflictsByType(t, mainDB, "metadata"), 1)
		require.Empty(t, conflictsByType(t, mainDB, "document"),
			"document merge must wait for validator resolution")
		require.Empty(t, conflictsByType(t, mainDB, "validation"))

		var doc1 bson.M
		require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&doc1))
		assert.Equal(t, "base", doc1["a"], "feature edit must not be visible before validator resolution")
		assert.Equal(t, "main", doc1["b"])
		err = mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: 2}}).Err()
		assert.ErrorIs(t, err, mongo.ErrNoDocuments,
			"feature-only insert must not be visible before validator resolution")
		require.NoError(t, mainDB.Collection("unaffected").FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Err())
		require.NoError(t, mainDB.Collection("unaffected").FindOne(ctx, bson.D{{Key: "_id", Value: 2}}).Err(),
			"collections without governing metadata conflicts still merge during phase one")

		env.Restart(suiteT)
		mainDB = env.Client.Database(dbName + "@main")
		mc := conflictsByType(t, mainDB, "metadata")[0]
		require.NoError(t, resolveConflict(t, mainDB, "items", mc["conflictId"].(string), "theirs", nil))
		env.Restart(suiteT)
		mainDB = env.Client.Database(dbName + "@main")
		continueMerge(t, mainDB)

		require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&doc1))
		assert.Equal(t, "feature", doc1["a"])
		assert.Equal(t, "main", doc1["b"])
		var doc2 bson.M
		require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: 2}}).Decode(&doc2))
		assert.EqualValues(t, 12, doc2["age"])
	})

	t.Run("ValidatorConflictDefersDocumentConflict", func(t *testing.T) {
		dbName, db := newDB("metadeferconflict")
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge)))
		_, err := db.Collection("items").InsertOne(ctx,
			bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(25)}, {Key: "tag", Value: "base"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "create validated items + doc", "alice <alice@acme.com>")

		vmBranch(t, env, dbName, "feature")
		featureDB := env.Client.Database(dbName + "@feature")
		require.NoError(t, featureDB.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(10)},
		}).Err())
		_, err = featureDB.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "tag", Value: "feature"}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature: validator + document", "bob <bob@widgets.io>")

		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(3)},
		}).Err())
		_, err = db.Collection("items").UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "tag", Value: "main"}}}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main: validator + document", "alice <alice@acme.com>")

		mainDB := env.Client.Database(dbName + "@main")
		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"], "divergent validator must conflict: %v", raw)
		require.Len(t, conflictsByType(t, mainDB, "metadata"), 1)
		require.Empty(t, conflictsByType(t, mainDB, "document"),
			"document conflict detection must wait for validator resolution")

		var before bson.M
		require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&before))
		assert.Equal(t, "main", before["tag"], "ours must remain visible while configuration is unresolved")

		mc := conflictsByType(t, mainDB, "metadata")[0]
		require.NoError(t, resolveConflict(t, mainDB, "items", mc["conflictId"].(string), "theirs", nil))
		cont := runCommandRaw(t, mainDB, bson.D{{Key: "doltMerge", Value: 1}, {Key: "continue", Value: 1}})
		require.EqualValues(t, 0, cont["ok"], "deferred document merge must re-pause: %v", cont)
		docs := conflictsByType(t, mainDB, "document")
		require.Len(t, docs, 1)
		assert.EqualValues(t, 1, docs[0]["ours"].(bson.M)["_id"])
	})

	t.Run("ValidatorConflictDefersUniqueIndexConflict", func(t *testing.T) {
		dbName, db := newDB("metadeferunique")
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge)))
		_, err := db.Collection("items").Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys:    bson.D{{Key: "sku", Value: int32(1)}},
			Options: options.Index().SetName("by_sku").SetUnique(true),
		})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "create validated items and index", "alice <alice@acme.com>")

		vmBranch(t, env, dbName, "feature")
		featureDB := env.Client.Database(dbName + "@feature")
		require.NoError(t, featureDB.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(10)},
		}).Err())
		_, err = featureDB.Collection("items").InsertOne(ctx,
			bson.D{{Key: "_id", Value: 2}, {Key: "age", Value: int32(12)}, {Key: "sku", Value: "S-1"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName+"@feature", "feature validator and indexed document", "bob <bob@widgets.io>")

		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"}, {Key: "validator", Value: ageGte(3)},
		}).Err())
		_, err = db.Collection("items").InsertOne(ctx,
			bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(5)}, {Key: "sku", Value: "S-1"}})
		require.NoError(t, err)
		dumboDBCommit(t, env, dbName, "main validator and indexed document", "alice <alice@acme.com>")

		mainDB := env.Client.Database(dbName + "@main")
		raw := vmMerge(t, env, dbName, "feature")
		require.EqualValues(t, 0, raw["ok"])
		require.Len(t, conflictsByType(t, mainDB, "metadata"), 1)
		require.Empty(t, conflictsByType(t, mainDB, "document"),
			"unique-index reconciliation must wait for validator resolution")
		require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Err())
		assert.ErrorIs(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: 2}}).Err(), mongo.ErrNoDocuments)

		mc := conflictsByType(t, mainDB, "metadata")[0]
		require.NoError(t, resolveConflict(t, mainDB, "items", mc["conflictId"].(string), "theirs", nil))
		cont := runCommandRaw(t, mainDB, bson.D{{Key: "doltMerge", Value: 1}, {Key: "continue", Value: 1}})
		require.EqualValues(t, 0, cont["ok"], "deferred unique collision must re-pause: %v", cont)
		docs := conflictsByType(t, mainDB, "document")
		require.Len(t, docs, 1)
		reason := docs[0]["reason"].(bson.M)
		assert.Equal(t, "uniqueKeyCollision", reason["code"])
		assert.Equal(t, "by_sku", reason["index"])

		require.NoError(t, resolveConflict(t, mainDB, "items", docs[0]["conflictId"].(string), "theirs", nil))
		require.Empty(t, conflictsByType(t, mainDB, "document"))
		vals := conflictsByType(t, mainDB, "validation")
		require.Len(t, vals, 1)
		assert.EqualValues(t, 1, vals[0]["documentId"])
		require.NoError(t, resolveConflict(t, mainDB, "items", vals[0]["conflictId"].(string), "drop", nil))
		continueMerge(t, mainDB)
		assert.ErrorIs(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Err(), mongo.ErrNoDocuments)
		require.NoError(t, mainDB.Collection("items").FindOne(ctx, bson.D{{Key: "_id", Value: 2}}).Err())
	})

	_ = options.CreateCollection
}
