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

	// Scenario8: divergent validators on both branches. This asserts the CURRENT
	// behavior -- the merge completes and keeps ours (main), silently dropping
	// theirs. docs/verify/validators.md flags this as a known gap; workspace-xhm
	// will flip it to surface a resolvable conflict, at which point this subtest
	// is replaced by a surfaces-and-resolves assertion.
	t.Run("Scenario8_DivergentValidators_TodayKeepsOurs", func(t *testing.T) {
		dbName := fmt.Sprintf("valconflict%d", suffix)
		db := env.Client.Database(dbName)
		require.NoError(t, db.Drop(ctx))
		require.NoError(t, db.CreateCollection(ctx, "items",
			options.CreateCollection().SetValidator(valNonNegAge)))
		dumboDBCommit(t, env, dbName, "create validated items", "alice <alice@acme.com>")
		vmBranch(t, env, dbName, "feature")

		// Feature: age >= 18.
		featDB := env.Client.Database(dbName + "@feature")
		require.NoError(t, featDB.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"},
			{Key: "validator", Value: bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: int32(18)}}}}},
		}).Err())
		dumboDBCommit(t, env, dbName+"@feature", "feature: age >= 18", "bob <bob@widgets.io>")

		// Main: age >= 21.
		require.NoError(t, db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: "items"},
			{Key: "validator", Value: bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: int32(21)}}}}},
		}).Err())
		dumboDBCommit(t, env, dbName, "main: age >= 21", "alice <alice@acme.com>")

		raw := vmMerge(t, env, dbName, "feature")
		// TODAY: silent keep-ours, no conflict. (TARGET, workspace-xhm: conflict.)
		require.EqualValues(t, 1, raw["ok"], "today the divergent-validator merge completes silently: %v", raw)

		mainDB := env.Client.Database(dbName + "@main")
		v := validatorOf(t, mainDB, "items")
		require.NotNil(t, v)
		age, _ := v["age"].(bson.M)
		assert.EqualValues(t, 21, age["$gte"], "today keeps ours (main's age >= 21); theirs is dropped")
	})
}
