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
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestProjection_Slice_AllElements verifies that {$slice: N} where N >= array length
// returns all elements. (DongoFull)
func TestProjection_Slice_AllElements(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
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

// TestProjection_Slice_FirstN verifies that {$slice: N} (N>0) returns the first N elements. (DongoFull)
func TestProjection_Slice_FirstN(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
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

// TestProjection_Slice_LastN verifies that {$slice: -N} returns the last N elements. (DongoFull)
func TestProjection_Slice_LastN(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
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
// counts from the end and then returns at most M elements. (DongoFull)
func TestProjection_Slice_NegativeSkip(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
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
// starting at skip. (DongoFull)
func TestProjection_Slice_SkipLimit(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
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

// TestSort_MetaTextScore verifies that sorting by {$meta: "textScore"} in a $text query
// does not return an error and returns the correct matching documents. (DongoFull)
func TestSort_MetaTextScore(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
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
