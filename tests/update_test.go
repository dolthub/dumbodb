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

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// TestUpdate_Pipeline tests pipeline-style updates ([]bson.D) using $set, $unset, and $addFields.
func TestUpdate_Pipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("SetLiteralField", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "a"), e("v", int32(1))),
		)

		// Pipeline-style update: $set a new field.
		_, err := coll.UpdateOne(ctx,
			d(e("_id", "a")),
			bson.A{d(e("$set", d(e("v", int32(42)))))},
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))
		assert.Equal(t, int32(42), docField(result, "v"))
	})

	t.Run("SetMultipleFields", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "b"), e("x", int32(1)), e("y", int32(2))),
		)

		_, err := coll.UpdateOne(ctx,
			d(e("_id", "b")),
			bson.A{d(e("$set", d(e("x", int32(10)), e("z", int32(30)))))},
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "b"))).Decode(&result))
		assert.Equal(t, int32(10), docField(result, "x"))
		assert.Equal(t, int32(2), docField(result, "y"))
		assert.Equal(t, int32(30), docField(result, "z"))
	})

	t.Run("UnsetField", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "c"), e("keep", int32(1)), e("drop", int32(2))),
		)

		_, err := coll.UpdateOne(ctx,
			d(e("_id", "c")),
			bson.A{d(e("$unset", "drop"))},
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "c"))).Decode(&result))
		assert.Equal(t, int32(1), docField(result, "keep"))
		assert.Nil(t, docFieldOrNil(result, "drop"), "drop field should be removed")
	})

	t.Run("UnsetArrayOfFields", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "d"), e("a", int32(1)), e("b", int32(2)), e("c", int32(3))),
		)

		_, err := coll.UpdateOne(ctx,
			d(e("_id", "d")),
			bson.A{d(e("$unset", bson.A{"a", "b"}))},
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "d"))).Decode(&result))
		assert.Nil(t, docFieldOrNil(result, "a"), "a should be removed")
		assert.Nil(t, docFieldOrNil(result, "b"), "b should be removed")
		assert.Equal(t, int32(3), docField(result, "c"))
	})

	t.Run("AddFields", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "e"), e("score", int32(5))),
		)

		_, err := coll.UpdateOne(ctx,
			d(e("_id", "e")),
			bson.A{d(e("$addFields", d(e("bonus", int32(10)))))},
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "e"))).Decode(&result))
		assert.Equal(t, int32(5), docField(result, "score"))
		assert.Equal(t, int32(10), docField(result, "bonus"))
	})

	t.Run("MultipleStages", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "f"), e("a", int32(1)), e("b", int32(2))),
		)

		// Two-stage pipeline: first $set, then $unset.
		_, err := coll.UpdateOne(ctx,
			d(e("_id", "f")),
			bson.A{
				d(e("$set", d(e("c", int32(3))))),
				d(e("$unset", "b")),
			},
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "f"))).Decode(&result))
		assert.Equal(t, int32(1), docField(result, "a"))
		assert.Equal(t, int32(3), docField(result, "c"))
		assert.Nil(t, docFieldOrNil(result, "b"), "b should be removed")
	})

	t.Run("UpdateMany", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "g1"), e("type", "x"), e("v", int32(1))),
			d(e("_id", "g2"), e("type", "x"), e("v", int32(2))),
			d(e("_id", "g3"), e("type", "y"), e("v", int32(3))),
		)

		res, err := coll.UpdateMany(ctx,
			d(e("type", "x")),
			bson.A{d(e("$set", d(e("updated", true))))},
		)
		require.NoError(t, err)
		assert.Equal(t, int64(2), res.ModifiedCount)

		var r1, r2, r3 bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "g1"))).Decode(&r1))
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "g2"))).Decode(&r2))
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "g3"))).Decode(&r3))
		assert.Equal(t, true, docField(r1, "updated"))
		assert.Equal(t, true, docField(r2, "updated"))
		assert.Nil(t, docFieldOrNil(r3, "updated"), "g3 should not be updated")
	})
}

// TestUpdate_PushModifiers tests $push with $each, $position, $slice, and $sort modifiers.
func TestUpdate_PushModifiers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("PushEach", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "p1"), e("arr", bson.A{int32(1), int32(2)})),
		)

		_, err := coll.UpdateOne(ctx,
			d(e("_id", "p1")),
			d(e("$push", d(e("arr", d(e("$each", bson.A{int32(3), int32(4)})))))),
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "p1"))).Decode(&result))
		assert.Equal(t, bson.A{int32(1), int32(2), int32(3), int32(4)}, docField(result, "arr"))
	})

	t.Run("PushPosition", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "p2"), e("arr", bson.A{int32(1), int32(2), int32(3)})),
		)

		// Insert at position 1.
		_, err := coll.UpdateOne(ctx,
			d(e("_id", "p2")),
			d(e("$push", d(
				e("arr", d(
					e("$each", bson.A{int32(10), int32(20)}),
					e("$position", int32(1)),
				)),
			))),
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "p2"))).Decode(&result))
		assert.Equal(t, bson.A{int32(1), int32(10), int32(20), int32(2), int32(3)}, docField(result, "arr"))
	})

	t.Run("PushSlicePositive", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "p3"), e("arr", bson.A{int32(1), int32(2), int32(3)})),
		)

		// Push two elements then slice to keep only first 3.
		_, err := coll.UpdateOne(ctx,
			d(e("_id", "p3")),
			d(e("$push", d(
				e("arr", d(
					e("$each", bson.A{int32(4), int32(5)}),
					e("$slice", int32(3)),
				)),
			))),
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "p3"))).Decode(&result))
		assert.Equal(t, bson.A{int32(1), int32(2), int32(3)}, docField(result, "arr"))
	})

	t.Run("PushSliceNegative", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "p4"), e("arr", bson.A{int32(1), int32(2), int32(3)})),
		)

		// Push two elements then keep only last 3.
		_, err := coll.UpdateOne(ctx,
			d(e("_id", "p4")),
			d(e("$push", d(
				e("arr", d(
					e("$each", bson.A{int32(4), int32(5)}),
					e("$slice", int32(-3)),
				)),
			))),
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "p4"))).Decode(&result))
		assert.Equal(t, bson.A{int32(3), int32(4), int32(5)}, docField(result, "arr"))
	})

	t.Run("PushSortAscending", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "p5"), e("arr", bson.A{int32(3), int32(1), int32(4)})),
		)

		_, err := coll.UpdateOne(ctx,
			d(e("_id", "p5")),
			d(e("$push", d(
				e("arr", d(
					e("$each", bson.A{int32(2)}),
					e("$sort", int32(1)),
				)),
			))),
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "p5"))).Decode(&result))
		assert.Equal(t, bson.A{int32(1), int32(2), int32(3), int32(4)}, docField(result, "arr"))
	})

	t.Run("PushSortDescending", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "p6"), e("arr", bson.A{int32(3), int32(1), int32(4)})),
		)

		_, err := coll.UpdateOne(ctx,
			d(e("_id", "p6")),
			d(e("$push", d(
				e("arr", d(
					e("$each", bson.A{int32(2)}),
					e("$sort", int32(-1)),
				)),
			))),
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "p6"))).Decode(&result))
		assert.Equal(t, bson.A{int32(4), int32(3), int32(2), int32(1)}, docField(result, "arr"))
	})

	t.Run("PushSortByDocumentField", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "p7"), e("scores", bson.A{
				d(e("name", "alice"), e("score", int32(3))),
				d(e("name", "bob"), e("score", int32(1))),
			})),
		)

		_, err := coll.UpdateOne(ctx,
			d(e("_id", "p7")),
			d(e("$push", d(
				e("scores", d(
					e("$each", bson.A{d(e("name", "carol"), e("score", int32(2)))}),
					e("$sort", d(e("score", int32(1)))),
				)),
			))),
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "p7"))).Decode(&result))

		scoresRaw := docField(result, "scores")
		scores, ok := scoresRaw.(bson.A)
		require.True(t, ok)
		require.Len(t, scores, 3)

		// Should be sorted by score ascending: bob(1), carol(2), alice(3).
		names := []string{
			docField(scores[0].(bson.D), "name").(string),
			docField(scores[1].(bson.D), "name").(string),
			docField(scores[2].(bson.D), "name").(string),
		}
		assert.Equal(t, []string{"bob", "carol", "alice"}, names)
	})

	t.Run("PushSliceZeroClearsArray", func(t *testing.T) {
		t.Parallel()

		env := startDongo(t)
		coll := env.collection(t)

		insertDocs(t, coll,
			d(e("_id", "p8"), e("arr", bson.A{int32(1), int32(2), int32(3)})),
		)

		_, err := coll.UpdateOne(ctx,
			d(e("_id", "p8")),
			d(e("$push", d(
				e("arr", d(
					e("$each", bson.A{}),
					e("$slice", int32(0)),
				)),
			))),
		)
		require.NoError(t, err)

		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "p8"))).Decode(&result))
		assert.Equal(t, bson.A{}, docField(result, "arr"))
	})
}

// docField returns the value of the given key from a bson.D, panics if not found.
func docField(doc bson.D, key string) interface{} {
	for _, e := range doc {
		if e.Key == key {
			return e.Value
		}
	}

	panic("key not found: " + key)
}

// docFieldOrNil returns the value of the given key from a bson.D, or nil if not found.
func docFieldOrNil(doc bson.D, key string) interface{} {
	for _, e := range doc {
		if e.Key == key {
			return e.Value
		}
	}

	return nil
}
