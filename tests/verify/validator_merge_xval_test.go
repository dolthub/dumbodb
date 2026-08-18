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

// Automated twin of docs/verify/validators.md Scenario 5 (merge
// cross-validation, epic workspace-h0w). A merge may never make data quality
// more non-conformant than it was: a document the merge cleanly inserts or
// modifies into a state that violates the resulting validator -- and that was
// not already violating at the base -- surfaces a type:"validation" conflict,
// resolved by replace-to-conform ("custom") or "drop". A document already in a
// data conflict is validated at resolution time instead (trigger 2).

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

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

	_ = options.CreateCollection
}
