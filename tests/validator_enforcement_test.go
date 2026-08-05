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

// DumboDB-side enforcement coverage for document validators on every write path
// (insert already enforced; update / findAndModify / bulkWrite are the paths
// closed by workspace-pui.1). Parity against real MongoDB lives in the harness
// (workspace-pui.2); this asserts DumboDB rejects with DocumentValidationFailure
// (121) and honors moderate/warn semantics.

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

const docValidationFailure = 121

func writeErrCode(err error) int32 {
	if c := mongoCommandCode(err); c != 0 {
		return c
	}
	var bwe mongo.BulkWriteException
	if errors.As(err, &bwe) {
		if bwe.WriteConcernError != nil {
			return int32(bwe.WriteConcernError.Code)
		}
		for _, we := range bwe.WriteErrors {
			return int32(we.Code)
		}
	}
	return 0
}

var nonNegAge = bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: int32(0)}}}}

func newValidatedColl(t *testing.T, env *dumboDBTestEnv, level, action string) *mongo.Collection {
	t.Helper()
	ctx := context.Background()
	db := env.Client.Database(fmt.Sprintf("valdb%d", rand.Int64N(1_000_000)))
	name := "items"
	opts := options.CreateCollection().SetValidator(nonNegAge)
	if level != "" {
		opts.SetValidationLevel(level)
	}
	if action != "" {
		opts.SetValidationAction(action)
	}
	require.NoError(t, db.CreateCollection(ctx, name, opts))
	return db.Collection(name)
}

func TestValidator_Update_RejectsInvalidResult(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := newValidatedColl(t, env, "strict", "error")

	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(5)}})
	require.NoError(t, err, "valid insert must succeed")

	_, err = coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(-5)}}}})
	require.Error(t, err, "update to an invalid document must be rejected")
	assert.EqualValues(t, docValidationFailure, mongoCommandCode(err))

	_, err = coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(9)}}}})
	require.NoError(t, err)
	var got bson.M
	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&got))
	assert.EqualValues(t, 9, got["age"], "rejected update must not have applied")
}

func TestValidator_UpdateUpsert_RejectsInvalid(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := newValidatedColl(t, env, "strict", "error")

	_, err := coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: 7}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(-1)}}}},
		options.UpdateOne().SetUpsert(true))
	require.Error(t, err, "upsert producing an invalid document must be rejected")
	assert.EqualValues(t, docValidationFailure, mongoCommandCode(err))

	n, err := coll.CountDocuments(ctx, bson.D{{Key: "_id", Value: 7}})
	require.NoError(t, err)
	assert.EqualValues(t, 0, n, "rejected upsert must not have inserted")
}

func TestValidator_FindAndModify_RejectsInvalidResult(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := newValidatedColl(t, env, "strict", "error")

	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(5)}})
	require.NoError(t, err)

	err = coll.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: 1}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(-3)}}}}).Err()
	require.Error(t, err, "findAndModify to an invalid document must be rejected")
	assert.EqualValues(t, docValidationFailure, mongoCommandCode(err))
}

func TestValidator_BulkWrite_RejectsInvalid(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := newValidatedColl(t, env, "strict", "error")

	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(5)}})
	require.NoError(t, err)

	_, err = coll.BulkWrite(ctx, []mongo.WriteModel{
		mongo.NewUpdateOneModel().
			SetFilter(bson.D{{Key: "_id", Value: 1}}).
			SetUpdate(bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(-1)}}}}),
	})
	require.Error(t, err, "bulkWrite update to invalid must be rejected")
	assert.EqualValues(t, docValidationFailure, writeErrCode(err))

	_, err = coll.BulkWrite(ctx, []mongo.WriteModel{
		mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "_id", Value: 2}, {Key: "age", Value: int32(-9)}}),
	})
	require.Error(t, err, "bulkWrite insert of invalid must be rejected")
	assert.EqualValues(t, docValidationFailure, writeErrCode(err))
}

func TestValidator_Warn_AllowsInvalidWrite(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := newValidatedColl(t, env, "strict", "warn")

	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(5)}})
	require.NoError(t, err)

	_, err = coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(-5)}}}})
	require.NoError(t, err, "validationAction:warn must allow the write")

	var got bson.M
	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "_id", Value: 1}}).Decode(&got))
	assert.EqualValues(t, -5, got["age"], "warn write must have applied")
}

// A validator is durable AND still enforces after restart -- enforcement
// re-hydrates from the durable catalog (workspace-pui.3).
func TestValidator_SurvivesRestart(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("valrestart%d", rand.Int64N(1_000_000))
	db := env.Client.Database(dbName)
	opts := options.CreateCollection().SetValidator(nonNegAge).
		SetValidationLevel("strict").SetValidationAction("error")
	require.NoError(t, db.CreateCollection(ctx, "items", opts))

	_, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(-1)}})
	require.Error(t, err)
	assert.EqualValues(t, docValidationFailure, mongoCommandCode(err))

	env.Restart(t)
	db = env.Client.Database(dbName)
	coll := db.Collection("items")

	var lc bson.M
	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "listCollections", Value: 1},
		{Key: "filter", Value: bson.D{{Key: "name", Value: "items"}}},
	}).Decode(&lc))
	batch := lc["cursor"].(bson.M)["firstBatch"].(bson.A)
	require.Len(t, batch, 1, "collection must still exist after restart")
	collOpts, _ := batch[0].(bson.M)["options"].(bson.M)
	assert.NotNil(t, collOpts["validator"], "validator must survive restart")

	_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 2}, {Key: "age", Value: int32(-5)}})
	require.Error(t, err, "validator must still reject after restart")
	assert.EqualValues(t, docValidationFailure, mongoCommandCode(err))

	_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 3}, {Key: "age", Value: int32(5)}})
	require.NoError(t, err, "valid insert must succeed after restart")
}

func TestValidator_Bypass_AllowsInvalid(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	coll := newValidatedColl(t, env, "strict", "error")

	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 2}, {Key: "age", Value: int32(-9)}},
		options.InsertOne().SetBypassDocumentValidation(true))
	require.NoError(t, err, "bypassDocumentValidation must allow an invalid insert")

	_, err = coll.InsertOne(ctx, bson.D{{Key: "_id", Value: 3}, {Key: "age", Value: int32(5)}})
	require.NoError(t, err)
	_, err = coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: 3}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(-1)}}}},
		options.UpdateOne().SetBypassDocumentValidation(true))
	require.NoError(t, err, "bypassDocumentValidation must allow an invalid update")

	var got bson.M
	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "_id", Value: 3}}).Decode(&got))
	assert.EqualValues(t, -1, got["age"], "bypassed update must have applied")
}

// moderate distinction: updating an already-invalid document is allowed, but
// turning a valid document invalid is rejected.
func TestValidator_Moderate_GrandfathersInvalidPreImage(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	db := env.Client.Database(fmt.Sprintf("valmod%d", rand.Int64N(1_000_000)))
	coll := db.Collection("items")

	_, err := coll.InsertMany(ctx, []interface{}{
		bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: int32(-5)}},
		bson.D{{Key: "_id", Value: 2}, {Key: "age", Value: int32(10)}},
	})
	require.NoError(t, err)

	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "items"},
		{Key: "validator", Value: nonNegAge},
		{Key: "validationLevel", Value: "moderate"},
		{Key: "validationAction", Value: "error"},
	}).Err())

	_, err = coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: 1}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "note", Value: "x"}}}})
	require.NoError(t, err, "moderate must allow updates to an already-invalid document")

	_, err = coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: 2}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: int32(-1)}}}})
	require.Error(t, err, "moderate must reject turning a valid document invalid")
	assert.EqualValues(t, docValidationFailure, mongoCommandCode(err))
}
