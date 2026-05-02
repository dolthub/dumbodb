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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestProjection_Slice_AllElements verifies that {$slice: N} where N >= array length
// returns all elements. (DumboDBFull)
func TestProjection_Slice_AllElements(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("nums", bson.A{int32(10), int32(20), int32(30)})),
	)

	ctx := context.Background()
	// $slice with value larger than array length returns all elements.
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetProjection(d(e("_id", int32(0)), e("nums", d(e("$slice", int32(100)))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	arr := extractArray(t, results[0], "nums")
	assert.Equal(t, bson.A{int32(10), int32(20), int32(30)}, arr)
}

// TestProjection_Slice_FirstN verifies that {$slice: N} (N>0) returns the first N elements. (DumboDBFull)
func TestProjection_Slice_FirstN(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("nums", bson.A{int32(10), int32(20), int32(30), int32(40), int32(50)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetProjection(d(e("_id", int32(0)), e("nums", d(e("$slice", int32(3)))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	arr := extractArray(t, results[0], "nums")
	assert.Equal(t, bson.A{int32(10), int32(20), int32(30)}, arr)
}

// TestProjection_Slice_LastN verifies that {$slice: -N} returns the last N elements. (DumboDBFull)
func TestProjection_Slice_LastN(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("nums", bson.A{int32(10), int32(20), int32(30), int32(40), int32(50)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetProjection(d(e("_id", int32(0)), e("nums", d(e("$slice", int32(-2)))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	arr := extractArray(t, results[0], "nums")
	assert.Equal(t, bson.A{int32(40), int32(50)}, arr)
}

// TestProjection_Slice_NegativeSkip verifies that {$slice: [-N, M]} with a negative skip
// counts from the end and then returns at most M elements. (DumboDBFull)
func TestProjection_Slice_NegativeSkip(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("nums", bson.A{int32(10), int32(20), int32(30), int32(40), int32(50)})),
	)

	ctx := context.Background()
	// Skip -3 (start from index 5-3=2 → value 30), limit 2 → [30, 40].
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetProjection(d(e("_id", int32(0)), e("nums", d(e("$slice", bson.A{int32(-3), int32(2)}))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	arr := extractArray(t, results[0], "nums")
	assert.Equal(t, bson.A{int32(30), int32(40)}, arr)
}

// TestProjection_Slice_SkipLimit verifies that {$slice: [skip, limit]} returns limit elements
// starting at skip. (DumboDBFull)
func TestProjection_Slice_SkipLimit(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("nums", bson.A{int32(10), int32(20), int32(30), int32(40), int32(50)})),
	)

	ctx := context.Background()
	// Skip 1, limit 3 → [20, 30, 40].
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetProjection(d(e("_id", int32(0)), e("nums", d(e("$slice", bson.A{int32(1), int32(3)}))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	arr := extractArray(t, results[0], "nums")
	assert.Equal(t, bson.A{int32(20), int32(30), int32(40)}, arr)
}

// TestProjection_ElemMatch_NoMatchInDoc verifies that when a $elemMatch projection
// condition matches no element in the array, the field is omitted from the result. (DumboDBFull)
func TestProjection_ElemMatch_NoMatchInDoc(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("nums", bson.A{int32(1), int32(2), int32(3)})),
	)

	ctx := context.Background()
	// $elemMatch with condition {$gt: 10}  -- no element qualifies.
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetProjection(d(
			e("nums", d(e("$elemMatch", d(e("$gt", int32(10)))))),
		)),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	// The "nums" field must be absent since no element matched.
	for _, el := range results[0] {
		assert.NotEqual(t, "nums", el.Key, "$elemMatch with no match must omit the field")
	}
}

// TestSort_FourFields verifies that sorting by four fields produces the correct
// stable ordering across multiple documents. (DumboDBFull)
func TestSort_FourFields(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("a", int32(1)), e("b", int32(2)), e("c", int32(1)), e("x", int32(10))),
		d(e("_id", int32(2)), e("a", int32(1)), e("b", int32(1)), e("c", int32(2)), e("x", int32(20))),
		d(e("_id", int32(3)), e("a", int32(2)), e("b", int32(1)), e("c", int32(1)), e("x", int32(30))),
		d(e("_id", int32(4)), e("a", int32(1)), e("b", int32(1)), e("c", int32(1)), e("x", int32(40))),
	)

	ctx := context.Background()
	// Sort by a asc, b asc, c asc, x asc.
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetSort(d(
			e("a", int32(1)),
			e("b", int32(1)),
			e("c", int32(1)),
			e("x", int32(1)),
		)),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 4)

	// Expected order: id=4 (a=1,b=1,c=1,x=40  -- wait, the only doc with a=1,b=1,c=1 is id=4 so it comes before id=2 (a=1,b=1,c=2).
	// Full expected: 4 (1,1,1,40) → 2 (1,1,2,20) → 1 (1,2,1,10) → 3 (2,1,1,30).
	expectedIDs := []int32{4, 2, 1, 3}
	for i, doc := range results {
		var id int32
		for _, el := range doc {
			if el.Key == "_id" {
				id = el.Value.(int32)
			}
		}
		assert.Equal(t, expectedIDs[i], id, "sort by four fields: position %d", i)
	}
}

// TestSort_Natural_Descending verifies that {$natural: -1} returns documents in
// reverse insertion order. (DumboDBFull)
func TestSort_Natural_Descending(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("seq", int32(1))),
		d(e("_id", int32(2)), e("seq", int32(2))),
		d(e("_id", int32(3)), e("seq", int32(3))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetSort(bson.D{{Key: "$natural", Value: -1}}),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// Reverse insertion order: 3, 2, 1.
	expectedIDs := []int32{3, 2, 1}
	for i, doc := range results {
		var id int32
		for _, el := range doc {
			if el.Key == "_id" {
				id = el.Value.(int32)
			}
		}
		assert.Equal(t, expectedIDs[i], id, "$natural:-1 order: position %d", i)
	}
}

// TestSort_MetaTextScore verifies that sorting by {$meta: "textScore"} in a $text query
// does not return an error and returns the correct matching documents. (DumboDBFull)
func TestSort_MetaTextScore(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("content", "apple banana")),
		d(e("_id", int32(2)), e("content", "cherry date")),
		d(e("_id", int32(3)), e("content", "apple cherry")),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("$text", d(e("$search", "apple")))),
		options.Find().SetSort(d(e("score", d(e("$meta", "textScore"))))).
			SetProjection(d(e("_id", int32(1)), e("score", d(e("$meta", "textScore"))))),
	)
	require.NoError(t, err, "sort by {$meta: textScore} must not error")
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// Docs 1 and 3 contain "apple".
	require.Len(t, results, 2)
}

// extractArray extracts a bson.A from a document field, failing the test if not found.
func extractArray(t *testing.T, doc bson.D, key string) bson.A {
	t.Helper()
	for _, el := range doc {
		if el.Key == key {
			arr, ok := el.Value.(bson.A)
			require.True(t, ok, "field %q must be an array, got %T", key, el.Value)
			return arr
		}
	}
	t.Fatalf("field %q not found in document", key)
	return nil
}
