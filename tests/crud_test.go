// Copyright 2021 FerretDB Inc.
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

// crud_test.go covers CRUD commands for parity testing between MongoDB and Dongo.
//
// Test naming convention:
//   - DongoFull: normal test, expected to pass on Dongo (no marker needed).
//   - DongoXFail: test expected to fail on Dongo (known limitation); uses dongoXFail() to skip.
//
// Minimum: 80 tests in this file, targeting 100+.

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// dongoXFail marks a test as an expected failure on Dongo (known limitation).
// The test body documents the correct MongoDB behaviour. When Dongo adds support,
// remove the dongoXFail() call and the test becomes a passing DongoFull test.
func dongoXFail(t *testing.T, reason string) {
	t.Helper()
	t.Skip("DongoXFail: " + reason)
}

// ─── InsertOne ───────────────────────────────────────────────────────────────

// TestCRUD_InsertOne tests basic InsertOne behaviour. (DongoFull)
func TestCRUD_InsertOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("BasicInsert", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		res, err := coll.InsertOne(ctx, d(e("_id", "a"), e("v", int32(1))))
		require.NoError(t, err)
		assert.Equal(t, "a", res.InsertedID)
	})

	t.Run("InsertWithGeneratedID", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		res, err := coll.InsertOne(ctx, d(e("v", int32(42))))
		require.NoError(t, err)
		_, ok := res.InsertedID.(primitive.ObjectID)
		assert.True(t, ok, "generated _id should be an ObjectID")
	})

	t.Run("DuplicateKeyError", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		_, err := coll.InsertOne(ctx, d(e("_id", "dup")))
		require.NoError(t, err)
		_, err = coll.InsertOne(ctx, d(e("_id", "dup")))
		require.Error(t, err)
		assert.True(t, mongo.IsDuplicateKeyError(err), "expected duplicate key error")
	})

	t.Run("NestedDocument", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		doc := d(e("_id", "nested"), e("addr", d(e("city", "NYC"), e("zip", "10001"))))
		_, err := coll.InsertOne(ctx, doc)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "nested"))).Decode(&result))
		m := result.Map()
		addr, ok := m["addr"].(bson.D)
		require.True(t, ok)
		assert.Equal(t, "NYC", addr.Map()["city"])
	})

	t.Run("ArrayValue", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		_, err := coll.InsertOne(ctx, d(e("_id", "arr"), e("tags", bson.A{"go", "db"})))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "arr"))).Decode(&result))
		assert.Equal(t, bson.A{"go", "db"}, result.Map()["tags"])
	})
}

// ─── InsertMany ──────────────────────────────────────────────────────────────

// TestCRUD_InsertMany tests InsertMany with ordered and unordered semantics. (DongoFull)
func TestCRUD_InsertMany(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("BasicInsertMany", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		docs := []interface{}{
			d(e("_id", "a"), e("v", int32(1))),
			d(e("_id", "b"), e("v", int32(2))),
			d(e("_id", "c"), e("v", int32(3))),
		}
		res, err := coll.InsertMany(ctx, docs)
		require.NoError(t, err)
		assert.Len(t, res.InsertedIDs, 3)
	})

	t.Run("OrderedStopOnDuplicate", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		_, err := coll.InsertOne(ctx, d(e("_id", "dup")))
		require.NoError(t, err)

		docs := []interface{}{
			d(e("_id", "x")),
			d(e("_id", "dup")), // duplicate — ordered mode should stop here
			d(e("_id", "y")),
		}
		res, err := coll.InsertMany(ctx, docs, options.InsertMany().SetOrdered(true))
		require.Error(t, err, "ordered insert should error on dup")
		bwe, ok := err.(mongo.BulkWriteException)
		require.True(t, ok)
		assert.Len(t, bwe.WriteErrors, 1)
		// "x" inserted, "dup" failed, "y" NOT inserted
		assert.Len(t, res.InsertedIDs, 1)

		count, err := coll.CountDocuments(ctx, d(e("_id", "y")))
		require.NoError(t, err)
		assert.Equal(t, int64(0), count)
	})

	t.Run("UnorderedContinueOnDuplicate", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		_, err := coll.InsertOne(ctx, d(e("_id", "dup")))
		require.NoError(t, err)

		docs := []interface{}{
			d(e("_id", "x")),
			d(e("_id", "dup")), // duplicate — unordered mode continues
			d(e("_id", "y")),
		}
		_, err = coll.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false))
		require.Error(t, err, "unordered insert should report error")
		bwe, ok := err.(mongo.BulkWriteException)
		require.True(t, ok)
		assert.Len(t, bwe.WriteErrors, 1)

		// Both "x" and "y" should be inserted despite dup failure.
		count, err := coll.CountDocuments(ctx, d(e("_id", d(e("$in", bson.A{"x", "y"})))))
		require.NoError(t, err)
		assert.Equal(t, int64(2), count)
	})
}

// ─── FindOne / Find ──────────────────────────────────────────────────────────

// TestCRUD_FindOne tests FindOne. (DongoFull)
func TestCRUD_FindOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("MatchingDoc", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1))))

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(1), result.Map()["v"])
	})

	t.Run("NotFound", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		err := coll.FindOne(ctx, d(e("_id", "missing"))).Decode(&bson.D{})
		assert.ErrorIs(t, err, mongo.ErrNoDocuments)
	})

	t.Run("NestedFieldFilter", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("addr", d(e("city", "NYC")))),
			d(e("_id", "b"), e("addr", d(e("city", "LA")))),
		)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("addr.city", "NYC"))).Decode(&result))
		assert.Equal(t, "a", result.Map()["_id"])
	})
}

// TestCRUD_Find tests Find with various options. (DongoFull)
func TestCRUD_Find(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("FindAll", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a")), d(e("_id", "b")), d(e("_id", "c")),
		)

		cursor, err := coll.Find(ctx, bson.D{})
		require.NoError(t, err)
		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 3)
	})

	t.Run("FindWithFilter", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("v", int32(1))),
			d(e("_id", "b"), e("v", int32(2))),
			d(e("_id", "c"), e("v", int32(3))),
		)

		cursor, err := coll.Find(ctx, d(e("v", d(e("$gt", int32(1))))))
		require.NoError(t, err)
		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 2)
	})

	t.Run("FindWithSortAndLimit", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("v", int32(3))),
			d(e("_id", "b"), e("v", int32(1))),
			d(e("_id", "c"), e("v", int32(2))),
		)

		cursor, err := coll.Find(ctx, bson.D{},
			options.Find().SetSort(d(e("v", 1))).SetLimit(2))
		require.NoError(t, err)
		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		assert.Equal(t, int32(1), results[0].Map()["v"])
		assert.Equal(t, int32(2), results[1].Map()["v"])
	})

	t.Run("FindWithSortDescending", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("v", int32(1))),
			d(e("_id", "b"), e("v", int32(2))),
			d(e("_id", "c"), e("v", int32(3))),
		)

		cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(d(e("v", -1))))
		require.NoError(t, err)
		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 3)
		assert.Equal(t, int32(3), results[0].Map()["v"])
	})

	t.Run("FindWithProjection", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1)), e("x", int32(2))))

		cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetProjection(d(e("v", 1))))
		require.NoError(t, err)
		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		_, hasX := results[0].Map()["x"]
		assert.False(t, hasX, "excluded field x should not be present")
	})

	t.Run("FindWithSkipAndLimit", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("v", int32(1))),
			d(e("_id", "b"), e("v", int32(2))),
			d(e("_id", "c"), e("v", int32(3))),
		)

		cursor, err := coll.Find(ctx, bson.D{},
			options.Find().SetSort(d(e("v", 1))).SetSkip(1).SetLimit(1))
		require.NoError(t, err)
		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.Equal(t, int32(2), results[0].Map()["v"])
	})

	t.Run("FindWithInOperator", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("v", int32(1))),
			d(e("_id", "b"), e("v", int32(2))),
			d(e("_id", "c"), e("v", int32(3))),
		)

		cursor, err := coll.Find(ctx, d(e("v", d(e("$in", bson.A{int32(1), int32(3)})))))
		require.NoError(t, err)
		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 2)
	})
}

// ─── UpdateOne ───────────────────────────────────────────────────────────────

// TestCRUD_UpdateOne tests UpdateOne with various operators. (DongoFull)
func TestCRUD_UpdateOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("SetOperator", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$set", d(e("v", int32(99))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(99), result.Map()["v"])
	})

	t.Run("IncOperator", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(5))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$inc", d(e("v", int32(3))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(8), result.Map()["v"])
	})

	t.Run("UnsetOperator", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1)), e("x", int32(2))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$unset", d(e("x", "")))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		_, hasX := result.Map()["x"]
		assert.False(t, hasX)
	})

	t.Run("Upsert", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		res, err := coll.UpdateOne(ctx, d(e("_id", "new")),
			d(e("$set", d(e("v", int32(1))))),
			options.Update().SetUpsert(true))
		require.NoError(t, err)
		assert.Equal(t, int64(1), res.UpsertedCount)
	})

	t.Run("UpsertNoMatch", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1))))

		res, err := coll.UpdateOne(ctx, d(e("_id", "missing")), d(e("$set", d(e("v", int32(9))))))
		require.NoError(t, err)
		assert.Equal(t, int64(0), res.ModifiedCount)
	})

	t.Run("MultipleOperators", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1)), e("x", int32(2))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$set", d(e("v", int32(10)))), e("$inc", d(e("x", int32(1))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(10), result.Map()["v"])
		assert.Equal(t, int32(3), result.Map()["x"])
	})

	t.Run("NonExistentField", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a")))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$inc", d(e("counter", int32(1))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(1), result.Map()["counter"])
	})
}

// ─── UpdateMany ──────────────────────────────────────────────────────────────

// TestCRUD_UpdateMany tests UpdateMany. (DongoFull)
func TestCRUD_UpdateMany(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("UpdatesMultipleDocs", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("status", "pending")),
			d(e("_id", "b"), e("status", "pending")),
			d(e("_id", "c"), e("status", "done")),
		)

		res, err := coll.UpdateMany(ctx,
			d(e("status", "pending")),
			d(e("$set", d(e("status", "processed")))))
		require.NoError(t, err)
		assert.Equal(t, int64(2), res.ModifiedCount)
	})

	t.Run("UpsertMulti", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		res, err := coll.UpdateMany(ctx,
			d(e("_id", "new")),
			d(e("$set", d(e("v", int32(1))))),
			options.Update().SetUpsert(true))
		require.NoError(t, err)
		assert.Equal(t, int64(1), res.UpsertedCount)
	})
}

// ─── DeleteOne / DeleteMany ───────────────────────────────────────────────────

// TestCRUD_Delete tests DeleteOne and DeleteMany. (DongoFull)
func TestCRUD_Delete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("DeleteOne", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a")), d(e("_id", "b")),
		)

		res, err := coll.DeleteOne(ctx, d(e("_id", "a")))
		require.NoError(t, err)
		assert.Equal(t, int64(1), res.DeletedCount)

		count, err := coll.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), count)
	})

	t.Run("DeleteOneNoMatch", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a")))

		res, err := coll.DeleteOne(ctx, d(e("_id", "missing")))
		require.NoError(t, err)
		assert.Equal(t, int64(0), res.DeletedCount)
	})

	t.Run("DeleteMany", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("v", int32(1))),
			d(e("_id", "b"), e("v", int32(1))),
			d(e("_id", "c"), e("v", int32(2))),
		)

		res, err := coll.DeleteMany(ctx, d(e("v", int32(1))))
		require.NoError(t, err)
		assert.Equal(t, int64(2), res.DeletedCount)
	})

	t.Run("DeleteManyEmptyFilter", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a")), d(e("_id", "b")), d(e("_id", "c")),
		)

		res, err := coll.DeleteMany(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), res.DeletedCount)
	})
}

// ─── ReplaceOne ──────────────────────────────────────────────────────────────

// TestCRUD_ReplaceOne tests ReplaceOne. (DongoFull)
func TestCRUD_ReplaceOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("BasicReplace", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1)), e("extra", "old")))

		res, err := coll.ReplaceOne(ctx, d(e("_id", "a")), d(e("_id", "a"), e("v", int32(99))))
		require.NoError(t, err)
		assert.Equal(t, int64(1), res.ModifiedCount)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		_, hasExtra := result.Map()["extra"]
		assert.False(t, hasExtra, "replaced doc should not have old fields")
		assert.Equal(t, int32(99), result.Map()["v"])
	})

	t.Run("ReplaceUpsert", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		res, err := coll.ReplaceOne(ctx, d(e("_id", "new")),
			d(e("_id", "new"), e("v", int32(1))),
			options.Replace().SetUpsert(true))
		require.NoError(t, err)
		assert.Equal(t, int64(1), res.UpsertedCount)
	})
}

// ─── Update operator: $push ──────────────────────────────────────────────────

// TestCRUD_UpdatePush tests $push with modifiers. (DongoFull)
func TestCRUD_UpdatePush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("PushBasic", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{int32(1), int32(2)})))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$push", d(e("arr", int32(3))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, bson.A{int32(1), int32(2), int32(3)}, result.Map()["arr"])
	})

	t.Run("PushWithEach", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{int32(1)})))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$push", d(e("arr", d(e("$each", bson.A{int32(2), int32(3)})))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, bson.A{int32(1), int32(2), int32(3)}, result.Map()["arr"])
	})

	t.Run("PushWithEachAndSlice", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{int32(1), int32(2), int32(3)})))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$push", d(e("arr", d(
				e("$each", bson.A{int32(4), int32(5)}),
				e("$slice", int32(3)),
			))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		arr := result.Map()["arr"].(bson.A)
		assert.Len(t, arr, 3, "$slice should keep only 3 elements")
	})

	t.Run("PushWithEachAndSort", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{int32(3), int32(1)})))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$push", d(e("arr", d(
				e("$each", bson.A{int32(2)}),
				e("$sort", int32(1)),
			))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, bson.A{int32(1), int32(2), int32(3)}, result.Map()["arr"])
	})

	t.Run("PushWithEachAndPosition", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{int32(1), int32(2), int32(3)})))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$push", d(e("arr", d(
				e("$each", bson.A{int32(10)}),
				e("$position", int32(1)),
			))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, bson.A{int32(1), int32(10), int32(2), int32(3)}, result.Map()["arr"])
	})
}

// ─── Update operator: $pull ──────────────────────────────────────────────────

// TestCRUD_UpdatePull tests $pull with equality and query conditions. (DongoFull)
func TestCRUD_UpdatePull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("PullBasicEquality", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{int32(1), int32(2), int32(3), int32(2)})))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$pull", d(e("arr", int32(2))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, bson.A{int32(1), int32(3)}, result.Map()["arr"])
	})

	t.Run("PullWithGtCondition", func(t *testing.T) {
		dongoXFail(t, "$pull with query conditions (e.g. $gt) not yet applied")
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{int32(1), int32(5), int32(2), int32(8)})))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$pull", d(e("arr", d(e("$gt", int32(3))))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, bson.A{int32(1), int32(2)}, result.Map()["arr"])
	})

	t.Run("PullRemovesAllMatches", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{int32(1), int32(1), int32(2)})))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$pull", d(e("arr", int32(1))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, bson.A{int32(2)}, result.Map()["arr"])
	})
}

// ─── Update operator: $addToSet ──────────────────────────────────────────────

// TestCRUD_UpdateAddToSet tests $addToSet. (DongoFull)
func TestCRUD_UpdateAddToSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("AddToSetNoDuplicate", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{"x", "y"})))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$addToSet", d(e("arr", "x")))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		// "x" already exists, should not be added again.
		assert.Equal(t, bson.A{"x", "y"}, result.Map()["arr"])
	})

	t.Run("AddToSetNewElement", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{"x"})))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$addToSet", d(e("arr", "z")))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, bson.A{"x", "z"}, result.Map()["arr"])
	})

	t.Run("AddToSetWithEach", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{"a"})))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$addToSet", d(e("arr", d(e("$each", bson.A{"a", "b", "c"})))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		arr := result.Map()["arr"].(bson.A)
		assert.Len(t, arr, 3)
	})
}

// ─── Update operator: $pop ───────────────────────────────────────────────────

// TestCRUD_UpdatePop tests $pop. (DongoFull)
func TestCRUD_UpdatePop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("PopLast", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{int32(1), int32(2), int32(3)})))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$pop", d(e("arr", int32(1))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, bson.A{int32(1), int32(2)}, result.Map()["arr"])
	})

	t.Run("PopFirst", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{int32(1), int32(2), int32(3)})))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$pop", d(e("arr", int32(-1))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, bson.A{int32(2), int32(3)}, result.Map()["arr"])
	})
}

// ─── Update operator: $bit ───────────────────────────────────────────────────

// TestCRUD_UpdateBit tests $bit AND/OR/XOR. (DongoFull)
func TestCRUD_UpdateBit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("BitAnd", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(0b1100)))) // 12

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$bit", d(e("v", d(e("and", int32(0b1010))))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(0b1000), result.Map()["v"]) // 12 & 10 = 8
	})

	t.Run("BitOr", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(0b1100))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$bit", d(e("v", d(e("or", int32(0b0011))))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(0b1111), result.Map()["v"]) // 12 | 3 = 15
	})

	t.Run("BitXor", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(0b1100))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$bit", d(e("v", d(e("xor", int32(0b1010))))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(0b0110), result.Map()["v"]) // 12 ^ 10 = 6
	})
}

// ─── Update operators: $min, $max ────────────────────────────────────────────

// TestCRUD_UpdateMinMax tests $min and $max. (DongoFull)
func TestCRUD_UpdateMinMax(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("MinUpdatesWhenSmaller", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(10))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$min", d(e("v", int32(5))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(5), result.Map()["v"])
	})

	t.Run("MinNoUpdateWhenLarger", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(10))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$min", d(e("v", int32(20))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(10), result.Map()["v"])
	})

	t.Run("MaxUpdatesWhenLarger", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(10))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$max", d(e("v", int32(20))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(20), result.Map()["v"])
	})

	t.Run("MaxNoUpdateWhenSmaller", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(10))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$max", d(e("v", int32(5))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(10), result.Map()["v"])
	})
}

// ─── Update operator: $currentDate ───────────────────────────────────────────

// TestCRUD_UpdateCurrentDate tests $currentDate with date and timestamp types. (DongoFull)
func TestCRUD_UpdateCurrentDate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("CurrentDateAsDate", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a")))

		before := time.Now().Add(-time.Second)
		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$currentDate", d(e("ts", d(e("$type", "date")))))))
		require.NoError(t, err)
		after := time.Now().Add(time.Second)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		ts, ok := result.Map()["ts"].(primitive.DateTime)
		require.True(t, ok, "expected primitive.DateTime")
		tsTime := ts.Time()
		assert.True(t, tsTime.After(before) && tsTime.Before(after))
	})

	t.Run("CurrentDateAsTimestamp", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a")))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$currentDate", d(e("ts", d(e("$type", "timestamp")))))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		_, ok := result.Map()["ts"].(primitive.Timestamp)
		assert.True(t, ok, "expected primitive.Timestamp")
	})

	t.Run("CurrentDateBooleanTrue", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a")))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$currentDate", d(e("ts", true)))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		_, ok := result.Map()["ts"].(primitive.DateTime)
		assert.True(t, ok, "true should set date type")
	})
}

// ─── Update operator: $rename ────────────────────────────────────────────────

// TestCRUD_UpdateRename tests $rename. (DongoFull)
func TestCRUD_UpdateRename(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("RenameField", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("old", int32(42))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$rename", d(e("old", "new")))))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		m := result.Map()
		_, hasOld := m["old"]
		_, hasNew := m["new"]
		assert.False(t, hasOld, "old field should be gone")
		assert.True(t, hasNew, "new field should exist")
		assert.Equal(t, int32(42), m["new"])
	})

	t.Run("RenameNonExistentField", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1))))

		// Renaming a non-existent field is a no-op in MongoDB.
		_, err := coll.UpdateOne(ctx, d(e("_id", "a")), d(e("$rename", d(e("missing", "x")))))
		require.NoError(t, err)
	})
}

// ─── Update operator: $setOnInsert ───────────────────────────────────────────

// TestCRUD_UpdateSetOnInsert tests $setOnInsert. (DongoFull)
func TestCRUD_UpdateSetOnInsert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("SetOnInsertOnUpsert", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		_, err := coll.UpdateOne(ctx, d(e("_id", "new")),
			d(e("$set", d(e("v", int32(1)))), e("$setOnInsert", d(e("created", "yes")))),
			options.Update().SetUpsert(true))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "new"))).Decode(&result))
		assert.Equal(t, "yes", result.Map()["created"])
	})

	t.Run("SetOnInsertNotAppliedOnUpdate", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$set", d(e("v", int32(2)))), e("$setOnInsert", d(e("created", "yes")))),
			options.Update().SetUpsert(true))
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		_, hasCreated := result.Map()["created"]
		assert.False(t, hasCreated, "$setOnInsert should not apply on regular update")
	})
}

// ─── Update: pipeline-style ───────────────────────────────────────────────────

// TestCRUD_UpdatePipeline tests pipeline-style updates ([]bson.D). (DongoFull)
func TestCRUD_UpdatePipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("PipelineSet", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(5))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			bson.A{d(e("$set", d(e("doubled", d(e("$multiply", bson.A{"$v", int32(2)}))))))})
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(10), result.Map()["doubled"])
	})

	t.Run("PipelineUnset", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1)), e("x", int32(2))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			bson.A{d(e("$unset", bson.A{"x"}))})
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		_, hasX := result.Map()["x"]
		assert.False(t, hasX)
	})

	t.Run("PipelineAddFields", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("price", float64(9.99)), e("qty", int32(3))))

		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			bson.A{d(e("$addFields", d(e("total", d(e("$multiply", bson.A{"$price", "$qty"}))))))})
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		total, ok := result.Map()["total"].(float64)
		require.True(t, ok)
		assert.InDelta(t, 29.97, total, 0.001)
	})
}

// ─── Update: arrayFilters (DongoXFail) ───────────────────────────────────────

// TestCRUD_ArrayFilters tests arrayFilters with positional $[identifier]. (DongoXFail)
func TestCRUD_ArrayFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("ArrayFilterPositionalIdentifier", func(t *testing.T) {
		dongoXFail(t, "arrayFilters with $[identifier] not yet implemented")

		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"),
			e("grades", bson.A{
				d(e("grade", "B"), e("score", int32(80))),
				d(e("grade", "C"), e("score", int32(70))),
				d(e("grade", "B"), e("score", int32(85))),
			}),
		))

		// Update all "B" grade subdocuments' scores with arrayFilters.
		_, err := coll.UpdateOne(ctx, d(e("_id", "a")),
			d(e("$set", d(e("grades.$[elem].score", int32(90))))),
			options.Update().SetArrayFilters(options.ArrayFilters{
				Filters: []interface{}{d(e("elem.grade", "B"))},
			}),
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		grades := result.Map()["grades"].(bson.A)
		for _, g := range grades {
			gd := g.(bson.D).Map()
			if gd["grade"] == "B" {
				assert.Equal(t, int32(90), gd["score"])
			}
		}
	})

	t.Run("ArrayFilterMultipleConditions", func(t *testing.T) {
		dongoXFail(t, "arrayFilters with $[identifier] not yet implemented")

		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "b"),
			e("scores", bson.A{int32(80), int32(60), int32(90), int32(40)}),
		))

		_, err := coll.UpdateOne(ctx, d(e("_id", "b")),
			d(e("$set", d(e("scores.$[x]", int32(50))))),
			options.Update().SetArrayFilters(options.ArrayFilters{
				Filters: []interface{}{d(e("x", d(e("$lt", int32(65)))))},
			}),
		)
		require.NoError(t, err)
	})
}

// ─── FindOneAndUpdate ─────────────────────────────────────────────────────────

// TestCRUD_FindOneAndUpdate tests FindOneAndUpdate with before/after semantics. (DongoFull)
func TestCRUD_FindOneAndUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("ReturnDocumentBefore", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1))))

		var result bson.D
		err := coll.FindOneAndUpdate(ctx,
			d(e("_id", "a")),
			d(e("$set", d(e("v", int32(99))))),
			options.FindOneAndUpdate().SetReturnDocument(options.Before),
		).Decode(&result)
		require.NoError(t, err)
		// Before: returns original value.
		assert.Equal(t, int32(1), result.Map()["v"])
	})

	t.Run("ReturnDocumentAfter", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1))))

		var result bson.D
		err := coll.FindOneAndUpdate(ctx,
			d(e("_id", "a")),
			d(e("$set", d(e("v", int32(99))))),
			options.FindOneAndUpdate().SetReturnDocument(options.After),
		).Decode(&result)
		require.NoError(t, err)
		// After: returns updated value.
		assert.Equal(t, int32(99), result.Map()["v"])
	})

	t.Run("WithProjection", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1)), e("x", int32(2))))

		var result bson.D
		err := coll.FindOneAndUpdate(ctx,
			d(e("_id", "a")),
			d(e("$inc", d(e("v", int32(1))))),
			options.FindOneAndUpdate().
				SetReturnDocument(options.After).
				SetProjection(d(e("v", 1))),
		).Decode(&result)
		require.NoError(t, err)
		_, hasX := result.Map()["x"]
		assert.False(t, hasX)
		assert.Equal(t, int32(2), result.Map()["v"])
	})

	t.Run("UpsertReturnsNewDoc", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		var result bson.D
		err := coll.FindOneAndUpdate(ctx,
			d(e("_id", "new")),
			d(e("$set", d(e("v", int32(1))))),
			options.FindOneAndUpdate().
				SetReturnDocument(options.After).
				SetUpsert(true),
		).Decode(&result)
		require.NoError(t, err)
		assert.Equal(t, int32(1), result.Map()["v"])
	})

	t.Run("NotFoundWithoutUpsert", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		err := coll.FindOneAndUpdate(ctx,
			d(e("_id", "missing")),
			d(e("$set", d(e("v", int32(1))))),
		).Decode(&bson.D{})
		assert.ErrorIs(t, err, mongo.ErrNoDocuments)
	})
}

// ─── FindOneAndReplace ────────────────────────────────────────────────────────

// TestCRUD_FindOneAndReplace tests FindOneAndReplace. (DongoFull)
func TestCRUD_FindOneAndReplace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("BasicReplace", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1)), e("old", "yes")))

		var result bson.D
		err := coll.FindOneAndReplace(ctx,
			d(e("_id", "a")),
			d(e("_id", "a"), e("v", int32(99))),
			options.FindOneAndReplace().SetReturnDocument(options.After),
		).Decode(&result)
		require.NoError(t, err)
		_, hasOld := result.Map()["old"]
		assert.False(t, hasOld, "old fields should be gone after replace")
		assert.Equal(t, int32(99), result.Map()["v"])
	})

	t.Run("ReturnDocumentBefore", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1))))

		var result bson.D
		err := coll.FindOneAndReplace(ctx,
			d(e("_id", "a")),
			d(e("_id", "a"), e("v", int32(99))),
			options.FindOneAndReplace().SetReturnDocument(options.Before),
		).Decode(&result)
		require.NoError(t, err)
		assert.Equal(t, int32(1), result.Map()["v"])
	})

	t.Run("WithProjection", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1)), e("x", int32(2))))

		var result bson.D
		err := coll.FindOneAndReplace(ctx,
			d(e("_id", "a")),
			d(e("_id", "a"), e("v", int32(99)), e("x", int32(2))),
			options.FindOneAndReplace().
				SetReturnDocument(options.After).
				SetProjection(d(e("v", 1))),
		).Decode(&result)
		require.NoError(t, err)
		_, hasX := result.Map()["x"]
		assert.False(t, hasX)
	})
}

// ─── FindOneAndDelete ─────────────────────────────────────────────────────────

// TestCRUD_FindOneAndDelete tests FindOneAndDelete. (DongoFull)
func TestCRUD_FindOneAndDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("BasicDelete", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1))))

		var result bson.D
		err := coll.FindOneAndDelete(ctx, d(e("_id", "a"))).Decode(&result)
		require.NoError(t, err)
		assert.Equal(t, int32(1), result.Map()["v"])

		count, err := coll.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(0), count)
	})

	t.Run("NotFound", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		err := coll.FindOneAndDelete(ctx, d(e("_id", "missing"))).Decode(&bson.D{})
		assert.ErrorIs(t, err, mongo.ErrNoDocuments)
	})

	t.Run("WithProjection", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1)), e("x", int32(2))))

		var result bson.D
		err := coll.FindOneAndDelete(ctx,
			d(e("_id", "a")),
			options.FindOneAndDelete().SetProjection(d(e("v", 1))),
		).Decode(&result)
		require.NoError(t, err)
		_, hasX := result.Map()["x"]
		assert.False(t, hasX)
		assert.Equal(t, int32(1), result.Map()["v"])
	})
}

// ─── CountDocuments / EstimatedDocumentCount ──────────────────────────────────

// TestCRUD_Count tests CountDocuments and EstimatedDocumentCount. (DongoFull)
func TestCRUD_Count(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("CountAll", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a")), d(e("_id", "b")), d(e("_id", "c")),
		)

		count, err := coll.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(3), count)
	})

	t.Run("CountWithFilter", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("v", int32(1))),
			d(e("_id", "b"), e("v", int32(2))),
			d(e("_id", "c"), e("v", int32(1))),
		)

		count, err := coll.CountDocuments(ctx, d(e("v", int32(1))))
		require.NoError(t, err)
		assert.Equal(t, int64(2), count)
	})

	t.Run("CountEmpty", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		count, err := coll.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.Equal(t, int64(0), count)
	})

	t.Run("EstimatedDocumentCountBasic", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a")), d(e("_id", "b")),
		)

		count, err := coll.EstimatedDocumentCount(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(2), count)
	})

	t.Run("EstimatedDocumentCountEmpty", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		count, err := coll.EstimatedDocumentCount(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(0), count)
	})
}

// ─── Distinct ─────────────────────────────────────────────────────────────────

// TestCRUD_Distinct tests Distinct on various field types. (DongoFull)
func TestCRUD_Distinct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("StringField", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("color", "red")),
			d(e("_id", "b"), e("color", "blue")),
			d(e("_id", "c"), e("color", "red")),
		)

		result, err := coll.Distinct(ctx, "color", bson.D{})
		require.NoError(t, err)
		sort.Slice(result, func(i, j int) bool { return result[i].(string) < result[j].(string) })
		assert.Equal(t, []interface{}{"blue", "red"}, result)
	})

	t.Run("NestedField", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("addr", d(e("city", "NYC")))),
			d(e("_id", "b"), e("addr", d(e("city", "LA")))),
			d(e("_id", "c"), e("addr", d(e("city", "NYC")))),
		)

		result, err := coll.Distinct(ctx, "addr.city", bson.D{})
		require.NoError(t, err)
		sort.Slice(result, func(i, j int) bool { return result[i].(string) < result[j].(string) })
		assert.Equal(t, []interface{}{"LA", "NYC"}, result)
	})

	t.Run("ArrayField", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("tags", bson.A{"go", "db"})),
			d(e("_id", "b"), e("tags", bson.A{"db", "sql"})),
		)

		result, err := coll.Distinct(ctx, "tags", bson.D{})
		require.NoError(t, err)
		strs := make([]string, len(result))
		for i, v := range result {
			strs[i] = v.(string)
		}
		sort.Strings(strs)
		assert.Equal(t, []string{"db", "go", "sql"}, strs)
	})

	t.Run("WithFilter", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("cat", "A"), e("v", int32(1))),
			d(e("_id", "b"), e("cat", "B"), e("v", int32(2))),
			d(e("_id", "c"), e("cat", "A"), e("v", int32(3))),
		)

		result, err := coll.Distinct(ctx, "v", d(e("cat", "A")))
		require.NoError(t, err)
		assert.Len(t, result, 2)
	})

	t.Run("NonExistentField", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1))))

		result, err := coll.Distinct(ctx, "nonexistent", bson.D{})
		require.NoError(t, err)
		assert.Empty(t, result)
	})
}

// ─── BulkWrite ────────────────────────────────────────────────────────────────

// TestCRUD_BulkWrite tests BulkWrite with various scenarios. (DongoFull)
func TestCRUD_BulkWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("OrderedMixedModels", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "upd"), e("v", int32(1))),
			d(e("_id", "del"), e("v", int32(2))),
		)

		models := []mongo.WriteModel{
			mongo.NewInsertOneModel().SetDocument(d(e("_id", "new"), e("v", int32(3)))),
			mongo.NewUpdateOneModel().
				SetFilter(d(e("_id", "upd"))).
				SetUpdate(d(e("$set", d(e("v", int32(10)))))),
			mongo.NewDeleteOneModel().SetFilter(d(e("_id", "del"))),
		}
		res, err := coll.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(true))
		require.NoError(t, err)
		assert.Equal(t, int64(1), res.InsertedCount)
		assert.Equal(t, int64(1), res.ModifiedCount)
		assert.Equal(t, int64(1), res.DeletedCount)
	})

	t.Run("OrderedStopOnError", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "dup")))

		models := []mongo.WriteModel{
			mongo.NewInsertOneModel().SetDocument(d(e("_id", "ok1"))),
			mongo.NewInsertOneModel().SetDocument(d(e("_id", "dup"))), // duplicate
			mongo.NewInsertOneModel().SetDocument(d(e("_id", "ok2"))),
		}
		_, err := coll.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(true))
		require.Error(t, err)

		// ok2 should NOT be inserted (ordered stops on first error).
		count, err := coll.CountDocuments(ctx, d(e("_id", "ok2")))
		require.NoError(t, err)
		assert.Equal(t, int64(0), count)
	})

	t.Run("UnorderedContinuesOnError", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "dup")))

		models := []mongo.WriteModel{
			mongo.NewInsertOneModel().SetDocument(d(e("_id", "ok1"))),
			mongo.NewInsertOneModel().SetDocument(d(e("_id", "dup"))), // duplicate
			mongo.NewInsertOneModel().SetDocument(d(e("_id", "ok2"))),
		}
		_, err := coll.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
		require.Error(t, err)

		// Both ok1 and ok2 should be inserted.
		count, err := coll.CountDocuments(ctx,
			d(e("_id", d(e("$in", bson.A{"ok1", "ok2"})))))
		require.NoError(t, err)
		assert.Equal(t, int64(2), count)
	})

	t.Run("UpdateManyModel", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll,
			d(e("_id", "a"), e("v", int32(1))),
			d(e("_id", "b"), e("v", int32(1))),
			d(e("_id", "c"), e("v", int32(2))),
		)

		models := []mongo.WriteModel{
			mongo.NewUpdateManyModel().
				SetFilter(d(e("v", int32(1)))).
				SetUpdate(d(e("$set", d(e("updated", true))))),
		}
		res, err := coll.BulkWrite(ctx, models)
		require.NoError(t, err)
		assert.Equal(t, int64(2), res.ModifiedCount)
	})

	t.Run("ReplaceOneModel", func(t *testing.T) {
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"), e("old", int32(1))))

		models := []mongo.WriteModel{
			mongo.NewReplaceOneModel().
				SetFilter(d(e("_id", "a"))).
				SetReplacement(d(e("_id", "a"), e("new", int32(99)))),
		}
		res, err := coll.BulkWrite(ctx, models)
		require.NoError(t, err)
		assert.Equal(t, int64(1), res.ModifiedCount)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		_, hasOld := result.Map()["old"]
		assert.False(t, hasOld)
	})

	t.Run("EmptyWritesResult", func(t *testing.T) {
		dongoXFail(t, "BulkWrite rejects empty slice instead of returning zero-count result")
		t.Parallel()
		env := startDongo(t)
		coll := env.collection(t)

		res, err := coll.BulkWrite(ctx, []mongo.WriteModel{})
		require.NoError(t, err)
		assert.NotNil(t, res)
		assert.Equal(t, int64(0), res.InsertedCount)
	})
}

// TestCRUD_BulkWriteArrayFilters tests BulkWrite with arrayFilters. (DongoXFail)
func TestCRUD_BulkWriteArrayFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("UpdateOneWithArrayFilters", func(t *testing.T) {
		dongoXFail(t, "arrayFilters with $[identifier] not yet implemented")

		env := startDongo(t)
		coll := env.collection(t)
		insertDocs(t, coll, d(e("_id", "a"),
			e("items", bson.A{
				d(e("name", "x"), e("qty", int32(5))),
				d(e("name", "y"), e("qty", int32(2))),
			}),
		))

		models := []mongo.WriteModel{
			mongo.NewUpdateOneModel().
				SetFilter(d(e("_id", "a"))).
				SetUpdate(d(e("$set", d(e("items.$[elem].qty", int32(10)))))).
				SetArrayFilters(options.ArrayFilters{
					Filters: []interface{}{d(e("elem.name", "x"))},
				}),
		}
		_, err := coll.BulkWrite(ctx, models)
		require.NoError(t, err)
	})
}
