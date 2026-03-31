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
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestAdvancedQuery_JsonSchema_ExclusiveMinimum_Maximum verifies $jsonSchema with
// exclusiveMinimum and exclusiveMaximum numeric constraints. (DongoFull)
func TestAdvancedQuery_JsonSchema_ExclusiveMinimum_Maximum(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("score", float64(5))),
		d(e("_id", int32(2)), e("score", float64(10))),
		d(e("_id", int32(3)), e("score", float64(15))),
		d(e("_id", int32(4)), e("score", float64(20))),
	)

	ctx := context.Background()
	// Match docs where score > 5 AND score < 20 (exclusive on both ends).
	cursor, err := coll.Find(ctx,
		d(e("$jsonSchema", d(
			e("properties", d(
				e("score", d(
					e("exclusiveMinimum", float64(5)),
					e("exclusiveMaximum", float64(20)),
				)),
			)),
		))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	ids := collectIDs(results)
	assert.Equal(t, []int32{2, 3}, ids)
}

// TestAdvancedQuery_JsonSchema_MultipleOf verifies $jsonSchema multipleOf constraint. (DongoFull)
func TestAdvancedQuery_JsonSchema_MultipleOf(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("n", int32(3))),
		d(e("_id", int32(2)), e("n", int32(6))),
		d(e("_id", int32(3)), e("n", int32(7))),
		d(e("_id", int32(4)), e("n", int32(9))),
	)

	ctx := context.Background()
	// Match docs where n is a multiple of 3.
	cursor, err := coll.Find(ctx,
		d(e("$jsonSchema", d(
			e("properties", d(
				e("n", d(e("multipleOf", float64(3)))),
			)),
		))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	ids := collectIDs(results)
	assert.Equal(t, []int32{1, 2, 4}, ids)
}

// TestAdvancedQuery_JsonSchema_NoMatch verifies that $jsonSchema returns no documents when
// the schema doesn't match any document. (DongoFull)
func TestAdvancedQuery_JsonSchema_NoMatch(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("x", int32(1))),
		d(e("_id", int32(2)), e("x", int32(2))),
	)

	ctx := context.Background()
	// Schema requires field "missing" to be present — no doc has it.
	cursor, err := coll.Find(ctx,
		d(e("$jsonSchema", d(
			e("required", bson.A{"missing"}),
		))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Empty(t, results)
}

// TestAdvancedQuery_JsonSchema_DuplicateRequired verifies that $jsonSchema with duplicate
// 'required' keys is rejected with FailedToParse (code 9). (DongoFull)
func TestAdvancedQuery_JsonSchema_DuplicateRequired(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("x", int32(1))),
		d(e("_id", int32(2)), e("x", int32(2))),
	)

	ctx := context.Background()
	// bson.D allows duplicate keys, but MongoDB rejects $jsonSchema with duplicate
	// keyword names with FailedToParse: "Duplicate $jsonSchema keyword: required".
	_, err := coll.Find(ctx,
		bson.D{{Key: "$jsonSchema", Value: bson.D{
			{Key: "required", Value: bson.A{"x"}},
			{Key: "required", Value: bson.A{"nonexistent_field_xyz"}},
		}}},
	)
	require.Error(t, err)
	cmdErr, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 9, cmdErr.Code, "expected FailedToParse (9), got code %d: %s", cmdErr.Code, cmdErr.Message)
	assert.Contains(t, cmdErr.Message, "Duplicate $jsonSchema keyword: required")
}

// TestAdvancedQuery_JsonSchema_OneOf verifies $jsonSchema oneOf constraint. (DongoFull)
func TestAdvancedQuery_JsonSchema_OneOf(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		// Matches only the int schema (bsonType int only) → oneOf passes (exactly 1 match).
		d(e("_id", int32(1)), e("val", int32(42))),
		// Matches only the string schema → oneOf passes.
		d(e("_id", int32(2)), e("val", "hello")),
		// float64 matches neither int nor string → oneOf fails.
		d(e("_id", int32(3)), e("val", float64(3.14))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("$jsonSchema", d(
			e("properties", d(
				e("val", d(
					e("oneOf", bson.A{
						d(e("bsonType", "int")),
						d(e("bsonType", "string")),
					}),
				)),
			)),
		))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	ids := collectIDs(results)
	assert.Equal(t, []int32{1, 2}, ids)
}

// TestAdvancedQuery_Regex_ExtendedWhitespace_x_Flag verifies that the regex x flag
// strips whitespace and comments from the pattern. (DongoFull)
func TestAdvancedQuery_Regex_ExtendedWhitespace_x_Flag(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("name", "foobar")),
		d(e("_id", int32(2)), e("name", "baz")),
	)

	ctx := context.Background()
	// Pattern "foo  bar" with x flag should match "foobar" (whitespace stripped).
	cursor, err := coll.Find(ctx,
		d(e("name", d(e("$regex", "foo  bar"), e("$options", "x")))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var gotID int32
	for _, el := range results[0] {
		if el.Key == "_id" {
			gotID = el.Value.(int32)
		}
	}
	assert.Equal(t, int32(1), gotID)
}

// TestAdvancedQuery_Regex_LookaheadSupported verifies that a regex with a PCRE lookahead
// assertion is evaluated correctly using the PCRE-compatible engine. (DongoFull)
func TestAdvancedQuery_Regex_LookaheadSupported(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("name", "foobar")),
		d(e("_id", int32(2)), e("name", "bazqux")),
	)

	ctx := context.Background()
	// "(?=foo)" is a PCRE lookahead — matches strings where "foo" follows at the current position.
	// "foobar" matches (lookahead succeeds at position 0); "bazqux" does not.
	cursor, err := coll.Find(ctx,
		d(e("name", d(e("$regex", "(?=foo)")))),
	)
	require.NoError(t, err, "lookahead regex should be supported via PCRE engine")

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, int32(1), results[0].Map()["_id"])
}

// TestAdvancedQuery_TextSearch_MetaTextScore_Projection verifies that $text search
// combined with {$meta: "textScore"} projection returns the score field (0.0). (DongoFull)
func TestAdvancedQuery_TextSearch_MetaTextScore_Projection(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("body", "the quick brown fox")),
		d(e("_id", int32(2)), e("body", "lorem ipsum dolor")),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("$text", d(e("$search", "fox")))),
		options.Find().SetProjection(d(
			e("_id", int32(1)),
			e("score", d(e("$meta", "textScore"))),
		)),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	// score field must be present and numeric (Dongo returns 0.0).
	var scoreFound bool
	for _, el := range results[0] {
		if el.Key == "score" {
			_, ok := el.Value.(float64)
			assert.True(t, ok, "score must be float64")
			scoreFound = true
		}
	}
	assert.True(t, scoreFound, "score field must be present in projection")
}

// TestAdvancedQuery_TextSearch_MetaTextScore_Sort verifies that $text search results
// can be sorted by {$meta: "textScore"} without error. (DongoFull)
func TestAdvancedQuery_TextSearch_MetaTextScore_Sort(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("body", "quick fox")),
		d(e("_id", int32(2)), e("body", "lazy dog")),
		d(e("_id", int32(3)), e("body", "brown fox")),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("$text", d(e("$search", "fox")))),
		options.Find().SetSort(d(e("score", d(e("$meta", "textScore"))))).
			SetProjection(d(e("_id", int32(1)), e("score", d(e("$meta", "textScore"))))),
	)
	require.NoError(t, err, "sort by textScore must not return an error")
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// Both fox docs match; dog doc does not.
	require.Len(t, results, 2)
}

// TestAdvancedQuery_TextSearch_MultipleTerms verifies that $text search with multiple
// terms matches documents containing any of the terms. (DongoFull)
func TestAdvancedQuery_TextSearch_MultipleTerms(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("body", "the quick brown fox")),
		d(e("_id", int32(2)), e("body", "jumped over the lazy dog")),
		d(e("_id", int32(3)), e("body", "lorem ipsum dolor")),
	)

	ctx := context.Background()
	// Search for "fox" OR "dog" — should match docs 1 and 2.
	cursor, err := coll.Find(ctx,
		d(e("$text", d(e("$search", "fox dog")))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	ids := collectIDs(results)
	assert.Equal(t, []int32{1, 2}, ids)
}

// --- $elemMatch tests ---

// TestAdvancedQuery_ElemMatch_BasicGt verifies {field: {$elemMatch: {$gt: N}}} matches
// documents where at least one array element satisfies the condition. (DongoFull)
func TestAdvancedQuery_ElemMatch_BasicGt(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("scores", bson.A{int32(10), int32(20)})),
		d(e("_id", int32(2)), e("scores", bson.A{int32(1), int32(2)})),
		d(e("_id", int32(3)), e("scores", bson.A{int32(100)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("scores", d(e("$elemMatch", d(e("$gt", int32(15))))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1, 3}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_MultiConditionSameElement verifies that {$elemMatch:
// {$gt: N, $lt: M}} requires BOTH conditions to be satisfied by the SAME element. (DongoFull)
func TestAdvancedQuery_ElemMatch_MultiConditionSameElement(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		// doc 1: [5, 95] — 5 < 10 but not > 0 in same bound, 95 > 0 but not < 10;
		//   wait actually 5 IS in (0,10): 5>0 AND 5<10, so doc1 SHOULD match.
		// Let's use ranges that make it clear:
		// Filter: {$gt: 50, $lt: 60} — only elements in (50, 60) match.
		d(e("_id", int32(1)), e("v", bson.A{int32(5), int32(90)})),   // no element in (50,60)
		d(e("_id", int32(2)), e("v", bson.A{int32(55), int32(90)})),  // 55 in (50,60)
		d(e("_id", int32(3)), e("v", bson.A{int32(51), int32(59)})),  // 51 and 59 both in range
		d(e("_id", int32(4)), e("v", bson.A{int32(10), int32(100)})), // 10 < 50, 100 > 60
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$elemMatch", d(e("$gt", int32(50)), e("$lt", int32(60))))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// Only docs 2 and 3 have elements in (50,60). Doc 4 has no single element in range.
	assert.Equal(t, []int32{2, 3}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_NoMatch verifies $elemMatch returns no results when
// no element satisfies the condition. (DongoFull)
func TestAdvancedQuery_ElemMatch_NoMatch(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("scores", bson.A{int32(1), int32(2), int32(3)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("scores", d(e("$elemMatch", d(e("$gt", int32(100))))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Empty(t, results)
}

// TestAdvancedQuery_ElemMatch_NonArray verifies $elemMatch on a non-array field
// returns no results. (DongoFull)
func TestAdvancedQuery_ElemMatch_NonArray(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("score", int32(42))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("score", d(e("$elemMatch", d(e("$gt", int32(0))))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Empty(t, results)
}

// TestAdvancedQuery_ElemMatch_MissingField verifies $elemMatch on a missing field
// returns no results. (DongoFull)
func TestAdvancedQuery_ElemMatch_MissingField(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("other", int32(42))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("scores", d(e("$elemMatch", d(e("$gt", int32(0))))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Empty(t, results)
}

// TestAdvancedQuery_ElemMatch_EmptyArray verifies $elemMatch on an empty array
// returns no results. (DongoFull)
func TestAdvancedQuery_ElemMatch_EmptyArray(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("scores", bson.A{})),
		d(e("_id", int32(2)), e("scores", bson.A{int32(5)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("scores", d(e("$elemMatch", d(e("$gt", int32(0))))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{2}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_Ne verifies {$elemMatch: {$ne: N}} works correctly.
// (DongoFull)
func TestAdvancedQuery_ElemMatch_Ne(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{int32(5), int32(5)})),    // all 5, no element != 5
		d(e("_id", int32(2)), e("v", bson.A{int32(5), int32(10)})),   // 10 != 5
		d(e("_id", int32(3)), e("v", bson.A{int32(1), int32(2)})),    // neither is 5
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$elemMatch", d(e("$ne", int32(5))))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// Docs 2 and 3 have at least one element != 5.
	assert.Equal(t, []int32{2, 3}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_Not verifies {$elemMatch: {$not: {$eq: N}}} works. (DongoFull)
func TestAdvancedQuery_ElemMatch_Not(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{int32(5), int32(5)})),
		d(e("_id", int32(2)), e("v", bson.A{int32(5), int32(9)})),
		d(e("_id", int32(3)), e("v", bson.A{int32(1), int32(2)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$elemMatch", d(e("$not", d(e("$eq", int32(5))))))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{2, 3}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_In verifies {$elemMatch: {$in: [...]}} works. (DongoFull)
func TestAdvancedQuery_ElemMatch_In(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{int32(1), int32(2)})),
		d(e("_id", int32(2)), e("v", bson.A{int32(3), int32(4)})),
		d(e("_id", int32(3)), e("v", bson.A{int32(5), int32(6)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$elemMatch", d(e("$in", bson.A{int32(2), int32(4)})))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1, 2}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_Nin verifies {$elemMatch: {$nin: [...]}} works. (DongoFull)
func TestAdvancedQuery_ElemMatch_Nin(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{int32(1), int32(2)})),  // 1 not in [3,4,5]
		d(e("_id", int32(2)), e("v", bson.A{int32(3), int32(4)})),  // all in [3,4,5]
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$elemMatch", d(e("$nin", bson.A{int32(3), int32(4), int32(5)})))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_ExistsTrue verifies {$elemMatch: {field: {$exists: true}}}
// on document arrays. (DongoFull)
func TestAdvancedQuery_ElemMatch_ExistsTrue(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("items", bson.A{
			d(e("name", "a")),
		})),
		d(e("_id", int32(2)), e("items", bson.A{
			d(e("other", "b")),
		})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("items", d(e("$elemMatch", d(e("name", d(e("$exists", true)))))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_TypeCheck verifies {$elemMatch: {$type: "string"}}
// on a scalar array. (DongoFull)
func TestAdvancedQuery_ElemMatch_TypeCheck(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{int32(1), "hello"})),
		d(e("_id", int32(2)), e("v", bson.A{int32(1), int32(2)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$elemMatch", d(e("$type", "string")))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_DocConditions verifies $elemMatch with document field
// conditions matches embedded document array elements. (DongoFull)
func TestAdvancedQuery_ElemMatch_DocConditions(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("results", bson.A{
			d(e("score", int32(80)), e("subject", "math")),
			d(e("score", int32(60)), e("subject", "english")),
		})),
		d(e("_id", int32(2)), e("results", bson.A{
			d(e("score", int32(55)), e("subject", "math")),
		})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("results", d(e("$elemMatch", d(e("score", d(e("$gte", int32(70))))))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_DocMultiFieldConditions verifies $elemMatch with multiple
// field conditions on embedded documents (all conditions must match same element). (DongoFull)
func TestAdvancedQuery_ElemMatch_DocMultiFieldConditions(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("results", bson.A{
			d(e("score", int32(80)), e("subject", "math")),
			d(e("score", int32(60)), e("subject", "english")),
		})),
		d(e("_id", int32(2)), e("results", bson.A{
			d(e("score", int32(80)), e("subject", "english")),
		})),
		d(e("_id", int32(3)), e("results", bson.A{
			d(e("score", int32(60)), e("subject", "math")),
		})),
	)

	ctx := context.Background()
	// Match docs where a SINGLE element has score >= 70 AND subject == "math"
	cursor, err := coll.Find(ctx,
		d(e("results", d(e("$elemMatch", d(
			e("score", d(e("$gte", int32(70)))),
			e("subject", "math"),
		))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// Only doc 1 has an element with score >= 70 AND subject == "math".
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_AndInside verifies {$elemMatch: {$and: [...]}} works
// for document array elements. (DongoFull)
func TestAdvancedQuery_ElemMatch_AndInside(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("results", bson.A{
			d(e("score", int32(85)), e("pass", true)),
			d(e("score", int32(40)), e("pass", false)),
		})),
		d(e("_id", int32(2)), e("results", bson.A{
			d(e("score", int32(40)), e("pass", false)),
		})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("results", d(e("$elemMatch", d(
			e("$and", bson.A{
				d(e("score", d(e("$gt", int32(80))))),
				d(e("pass", true)),
			}),
		))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_OrInside verifies {$elemMatch: {$or: [...]}} works
// for document array elements. (DongoFull)
func TestAdvancedQuery_ElemMatch_OrInside(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("results", bson.A{
			d(e("score", int32(95))),
		})),
		d(e("_id", int32(2)), e("results", bson.A{
			d(e("score", int32(15))),
		})),
		d(e("_id", int32(3)), e("results", bson.A{
			d(e("score", int32(50))),
		})),
	)

	ctx := context.Background()
	// Match docs where an element has score > 90 OR score < 20.
	cursor, err := coll.Find(ctx,
		d(e("results", d(e("$elemMatch", d(
			e("$or", bson.A{
				d(e("score", d(e("$gt", int32(90))))),
				d(e("score", d(e("$lt", int32(20))))),
			}),
		))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1, 2}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_NorInside verifies {$elemMatch: {$nor: [...]}} works
// for document array elements. (DongoFull)
func TestAdvancedQuery_ElemMatch_NorInside(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("results", bson.A{
			d(e("score", int32(50))),
		})),
		d(e("_id", int32(2)), e("results", bson.A{
			d(e("score", int32(95))),
		})),
		d(e("_id", int32(3)), e("results", bson.A{
			d(e("score", int32(5))),
		})),
	)

	ctx := context.Background()
	// $nor: score > 90, score < 10 — match elements that are neither high nor low.
	cursor, err := coll.Find(ctx,
		d(e("results", d(e("$elemMatch", d(
			e("$nor", bson.A{
				d(e("score", d(e("$gt", int32(90))))),
				d(e("score", d(e("$lt", int32(10))))),
			}),
		))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// Only doc 1 (score=50) satisfies nor (not high, not low).
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_GteEq verifies $elemMatch with $gte and $lte
// boundary conditions. (DongoFull)
func TestAdvancedQuery_ElemMatch_GteEq(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{int32(10), int32(20)})),
		d(e("_id", int32(2)), e("v", bson.A{int32(15)})),
		d(e("_id", int32(3)), e("v", bson.A{int32(5), int32(25)})),
	)

	ctx := context.Background()
	// Match where some element is in [10, 20] inclusive.
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$elemMatch", d(e("$gte", int32(10)), e("$lte", int32(20))))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// Doc1: 10 and 20 both in range. Doc2: 15 in range. Doc3: neither 5 nor 25 in [10,20].
	assert.Equal(t, []int32{1, 2}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_Regex verifies {$elemMatch: {$regex: ...}} works. (DongoFull)
func TestAdvancedQuery_ElemMatch_Regex(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("tags", bson.A{"alpha", "beta"})),
		d(e("_id", int32(2)), e("tags", bson.A{"gamma", "delta"})),
		d(e("_id", int32(3)), e("tags", bson.A{"foo"})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("tags", d(e("$elemMatch", d(e("$regex", "^a")))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// --- $all tests ---

// TestAdvancedQuery_All_Basic verifies {field: {$all: [...]}} matches documents
// whose array contains all the specified values. (DongoFull)
func TestAdvancedQuery_All_Basic(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("tags", bson.A{"a", "b", "c"})),
		d(e("_id", int32(2)), e("tags", bson.A{"a", "b"})),
		d(e("_id", int32(3)), e("tags", bson.A{"x", "y"})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("tags", d(e("$all", bson.A{"a", "b"})))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1, 2}, collectIDs(results))
}

// TestAdvancedQuery_All_EmptyQueryArray verifies {$all: []} matches nothing. (DongoFull)
func TestAdvancedQuery_All_EmptyQueryArray(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("tags", bson.A{"a"})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("tags", d(e("$all", bson.A{})))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Empty(t, results)
}

// TestAdvancedQuery_All_ScalarField verifies $all on a scalar field matches
// if all elements in the query equal the scalar. (DongoFull)
func TestAdvancedQuery_All_ScalarField(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", int32(42))),
		d(e("_id", int32(2)), e("v", int32(7))),
	)

	ctx := context.Background()
	// $all with [42, 42] should match the scalar 42.
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$all", bson.A{int32(42), int32(42)})))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_All_ExtraElementsOK verifies that $all succeeds when the
// array has extra elements beyond those specified. (DongoFull)
func TestAdvancedQuery_All_ExtraElementsOK(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("tags", bson.A{"a", "b", "c", "d"})),
		d(e("_id", int32(2)), e("tags", bson.A{"a"})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("tags", d(e("$all", bson.A{"a", "b"})))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_All_MissingField verifies $all on a missing field
// returns no results. (DongoFull)
func TestAdvancedQuery_All_MissingField(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("other", "x")),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("tags", d(e("$all", bson.A{"a"})))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Empty(t, results)
}

// TestAdvancedQuery_All_ElemMatch verifies $all with $elemMatch inside works. (DongoFull)
func TestAdvancedQuery_All_ElemMatch(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("results", bson.A{
			d(e("score", int32(85)), e("pass", true)),
			d(e("score", int32(90)), e("pass", true)),
		})),
		d(e("_id", int32(2)), e("results", bson.A{
			d(e("score", int32(85)), e("pass", true)),
		})),
	)

	ctx := context.Background()
	// Require at least one element with score >= 90 (using $all with $elemMatch).
	cursor, err := coll.Find(ctx,
		d(e("results", d(e("$all", bson.A{
			d(e("$elemMatch", d(e("score", d(e("$gte", int32(90))))))),
		})))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// --- $size tests ---

// TestAdvancedQuery_Size_Basic verifies {field: {$size: N}} matches arrays of exactly N elements. (DongoFull)
func TestAdvancedQuery_Size_Basic(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{int32(1), int32(2)})),
		d(e("_id", int32(2)), e("v", bson.A{int32(1), int32(2), int32(3)})),
		d(e("_id", int32(3)), e("v", bson.A{int32(1)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$size", int32(2))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Size_Zero verifies {$size: 0} matches empty arrays. (DongoFull)
func TestAdvancedQuery_Size_Zero(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{})),
		d(e("_id", int32(2)), e("v", bson.A{int32(1)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$size", int32(0))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Size_NegativeError verifies {$size: -1} returns an error. (DongoFull)
func TestAdvancedQuery_Size_NegativeError(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{int32(1)})),
	)

	ctx := context.Background()
	_, err := coll.Find(ctx,
		d(e("v", d(e("$size", int32(-1))))),
	)
	require.Error(t, err)
	var cmdErr mongo.CommandError
	require.ErrorAs(t, err, &cmdErr)
	assert.Equal(t, int32(2), cmdErr.Code) // BadValue
}

// TestAdvancedQuery_Size_NonIntError verifies {$size: 1.5} returns an error. (DongoFull)
func TestAdvancedQuery_Size_NonIntError(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{int32(1)})),
	)

	ctx := context.Background()
	_, err := coll.Find(ctx,
		d(e("v", d(e("$size", float64(1.5))))),
	)
	require.Error(t, err)
	var cmdErr mongo.CommandError
	require.ErrorAs(t, err, &cmdErr)
	assert.Equal(t, int32(2), cmdErr.Code) // BadValue
}

// TestAdvancedQuery_Size_NonArray verifies $size on a non-array field returns no results. (DongoFull)
func TestAdvancedQuery_Size_NonArray(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", int32(2))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$size", int32(2))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Empty(t, results)
}

// TestAdvancedQuery_Size_MissingField verifies $size on a missing field returns no results. (DongoFull)
func TestAdvancedQuery_Size_MissingField(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("other", "x")),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$size", int32(0))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Empty(t, results)
}

// --- $exists tests ---

// TestAdvancedQuery_Exists_TruePresent verifies {field: {$exists: true}} matches
// documents where the field exists. (DongoFull)
func TestAdvancedQuery_Exists_TruePresent(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("x", int32(1))),
		d(e("_id", int32(2)), e("y", int32(2))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("x", d(e("$exists", true)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Exists_FalseMissing verifies {field: {$exists: false}} matches
// documents where the field does not exist. (DongoFull)
func TestAdvancedQuery_Exists_FalseMissing(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("x", int32(1))),
		d(e("_id", int32(2)), e("y", int32(2))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("x", d(e("$exists", false)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{2}, collectIDs(results))
}

// TestAdvancedQuery_Exists_TrueNullValue verifies {$exists: true} matches fields
// that are present even with null value. (DongoFull)
func TestAdvancedQuery_Exists_TrueNullValue(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("x", nil)),
		d(e("_id", int32(2)), e("y", int32(1))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("x", d(e("$exists", true)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// Doc1 has x=null (field exists), Doc2 has no x.
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Exists_TrueDoesNotMatch verifies {$exists: true} does NOT match
// documents where the field is missing. (DongoFull)
func TestAdvancedQuery_Exists_TrueDoesNotMatch(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("other", int32(1))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("x", d(e("$exists", true)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Empty(t, results)
}

// TestAdvancedQuery_Exists_FalseDoesNotMatchPresent verifies {$exists: false} does NOT match
// documents where the field is present. (DongoFull)
func TestAdvancedQuery_Exists_FalseDoesNotMatchPresent(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("x", int32(5))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("x", d(e("$exists", false)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Empty(t, results)
}

// TestAdvancedQuery_Exists_NestedField verifies $exists works with dot-notation
// for nested fields. (DongoFull)
func TestAdvancedQuery_Exists_NestedField(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("a", d(e("b", int32(1))))),
		d(e("_id", int32(2)), e("a", d(e("c", int32(1))))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("a.b", d(e("$exists", true)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Exists_ExistsAndTypeCombo verifies combining $exists with $type. (DongoFull)
func TestAdvancedQuery_Exists_ExistsAndTypeCombo(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", "hello")),
		d(e("_id", int32(2)), e("v", int32(42))),
		d(e("_id", int32(3))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$exists", true), e("$type", "string")))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// --- $type tests ---

// TestAdvancedQuery_Type_StringName verifies {$type: "string"} matches string fields. (DongoFull)
func TestAdvancedQuery_Type_StringName(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", "hello")),
		d(e("_id", int32(2)), e("v", int32(42))),
		d(e("_id", int32(3)), e("v", true)),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$type", "string")))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Type_NumericCode verifies {$type: 2} (string type code) works. (DongoFull)
func TestAdvancedQuery_Type_NumericCode(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", "hello")),
		d(e("_id", int32(2)), e("v", int32(42))),
	)

	ctx := context.Background()
	// Type code 2 = string in BSON.
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$type", int32(2))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Type_ArrayOfTypes verifies {$type: [...]} matches any of the listed types. (DongoFull)
func TestAdvancedQuery_Type_ArrayOfTypes(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", "hello")),
		d(e("_id", int32(2)), e("v", int32(42))),
		d(e("_id", int32(3)), e("v", true)),
		d(e("_id", int32(4)), e("v", float64(3.14))),
	)

	ctx := context.Background()
	// Match string or int32 (codes 2 and 16).
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$type", bson.A{"string", "int"})))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1, 2}, collectIDs(results))
}

// TestAdvancedQuery_Type_Null verifies {$type: "null"} matches null-valued fields. (DongoFull)
func TestAdvancedQuery_Type_Null(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", nil)),
		d(e("_id", int32(2)), e("v", int32(1))),
		d(e("_id", int32(3))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$type", "null")))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// Only doc1 has v=null explicitly. Doc3 has no v field — $type:"null" should NOT match
	// an absent field (it specifically matches the BSON null type).
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Type_Array verifies {$type: "array"} matches array-typed fields. (DongoFull)
func TestAdvancedQuery_Type_Array(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{int32(1), int32(2)})),
		d(e("_id", int32(2)), e("v", "not-an-array")),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$type", "array")))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Type_Object verifies {$type: "object"} matches document-typed fields. (DongoFull)
func TestAdvancedQuery_Type_Object(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", d(e("k", "v")))),
		d(e("_id", int32(2)), e("v", "not-an-object")),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$type", "object")))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Type_Bool verifies {$type: "bool"} matches boolean fields. (DongoFull)
func TestAdvancedQuery_Type_Bool(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", true)),
		d(e("_id", int32(2)), e("v", int32(1))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$type", "bool")))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Type_OnArrayField verifies $type checks array element types
// (e.g., "string" matches an array containing a string). (DongoFull)
func TestAdvancedQuery_Type_OnArrayField(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{int32(1), "hello"})),
		d(e("_id", int32(2)), e("v", bson.A{int32(1), int32(2)})),
	)

	ctx := context.Background()
	// $type:"string" should match doc1 because its array contains a string element.
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$type", "string")))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Type_NumberAlias verifies {$type: "number"} matches int32, int64,
// and double fields. (DongoFull)
func TestAdvancedQuery_Type_NumberAlias(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", int32(1))),
		d(e("_id", int32(2)), e("v", float64(1.5))),
		d(e("_id", int32(3)), e("v", "str")),
		d(e("_id", int32(4)), e("v", true)),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$type", "number")))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1, 2}, collectIDs(results))
}

// --- Bitwise query tests ---

// TestAdvancedQuery_Bitwise_AllSet verifies {$bitsAllSet: mask} matches when all
// specified bits are set. (DongoFull)
func TestAdvancedQuery_Bitwise_AllSet(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		// 0b0110 = 6 — bits 1 and 2 set
		d(e("_id", int32(1)), e("v", int32(6))),
		// 0b0111 = 7 — bits 0, 1, 2 set
		d(e("_id", int32(2)), e("v", int32(7))),
		// 0b0100 = 4 — only bit 2 set
		d(e("_id", int32(3)), e("v", int32(4))),
	)

	ctx := context.Background()
	// Mask 6 (0b0110): require bits 1 and 2 both set.
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$bitsAllSet", int32(6))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1, 2}, collectIDs(results))
}

// TestAdvancedQuery_Bitwise_AllClear verifies {$bitsAllClear: mask} matches when all
// specified bits are clear. (DongoFull)
func TestAdvancedQuery_Bitwise_AllClear(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", int32(0b1000))), // bit 3 set, bits 0-2 clear
		d(e("_id", int32(2)), e("v", int32(0b0001))), // bit 0 set
		d(e("_id", int32(3)), e("v", int32(0))),       // all bits clear
	)

	ctx := context.Background()
	// Mask 0b0110 = 6: require bits 1 and 2 clear.
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$bitsAllClear", int32(6))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// Doc1 (8): bits 1,2 are clear → match. Doc2 (1): bits 1,2 are clear → match. Doc3 (0): bits 1,2 clear → match.
	assert.Equal(t, []int32{1, 2, 3}, collectIDs(results))
}

// TestAdvancedQuery_Bitwise_AnySet verifies {$bitsAnySet: mask} matches when at least
// one specified bit is set. (DongoFull)
func TestAdvancedQuery_Bitwise_AnySet(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", int32(0b0010))), // bit 1 set
		d(e("_id", int32(2)), e("v", int32(0b0100))), // bit 2 set
		d(e("_id", int32(3)), e("v", int32(0b0001))), // only bit 0 set
		d(e("_id", int32(4)), e("v", int32(0))),       // no bits set
	)

	ctx := context.Background()
	// Mask 6 (0b0110): match if bit 1 OR bit 2 is set.
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$bitsAnySet", int32(6))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1, 2}, collectIDs(results))
}

// TestAdvancedQuery_Bitwise_AnyClear verifies {$bitsAnyClear: mask} matches when at least
// one specified bit is clear. (DongoFull)
func TestAdvancedQuery_Bitwise_AnyClear(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", int32(0b1111))), // all bits set
		d(e("_id", int32(2)), e("v", int32(0b1101))), // bit 1 clear
		d(e("_id", int32(3)), e("v", int32(0b0110))), // bits 0,3 clear
	)

	ctx := context.Background()
	// Mask 0b0110 = 6: match if bit 1 OR bit 2 is clear.
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$bitsAnyClear", int32(6))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// Doc1 (0b1111): bits 1,2 both set → no match. Doc2 (0b1101): bit 1 clear → match. Doc3 (0b0110): bit 0,3 clear → bits 1,2 set → no match.
	assert.Equal(t, []int32{2}, collectIDs(results))
}

// TestAdvancedQuery_Bitwise_BitPositionsAllSet verifies $bitsAllSet with bit position array. (DongoFull)
func TestAdvancedQuery_Bitwise_BitPositionsAllSet(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", int32(0b1110))), // bits 1,2,3 set
		d(e("_id", int32(2)), e("v", int32(0b0110))), // bits 1,2 set (not 3)
		d(e("_id", int32(3)), e("v", int32(0b0000))),
	)

	ctx := context.Background()
	// Check that bits at positions 1 and 2 are both set.
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$bitsAllSet", bson.A{int32(1), int32(2)})))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1, 2}, collectIDs(results))
}

// TestAdvancedQuery_Bitwise_Float verifies bitwise operators work on float64 integer values. (DongoFull)
func TestAdvancedQuery_Bitwise_Float(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", float64(7))), // 0b0111
		d(e("_id", int32(2)), e("v", float64(4))), // 0b0100
	)

	ctx := context.Background()
	// Require bit 0 is set (mask=1).
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$bitsAllSet", int32(1))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Bitwise_NullNoMatch verifies bitwise operators return no match
// for null field values. (DongoFull)
func TestAdvancedQuery_Bitwise_NullNoMatch(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", nil)),
		d(e("_id", int32(2)), e("v", int32(7))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$bitsAllSet", int32(1))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{2}, collectIDs(results))
}

// TestAdvancedQuery_Bitwise_Int64 verifies bitwise operators work on int64 values. (DongoFull)
func TestAdvancedQuery_Bitwise_Int64(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", int64(0b1100))), // bits 2,3 set
		d(e("_id", int32(2)), e("v", int64(0b0011))), // bits 0,1 set
	)

	ctx := context.Background()
	// Mask 12 (0b1100): require bits 2 and 3 set.
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$bitsAllSet", int32(12))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// --- $where tests ---

// TestAdvancedQuery_Where_ReturnsError verifies that $where operator returns an error
// since JavaScript execution is not supported. (DongoFull)
func TestAdvancedQuery_Where_ReturnsError(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", int32(5))),
	)

	ctx := context.Background()
	_, err := coll.Find(ctx,
		d(e("$where", "this.v > 3")),
	)
	// $where requires JS execution, which is not supported.
	// Dongo should return an error (either NotImplemented or BadValue).
	require.Error(t, err)
}

// --- Combination tests ---

// TestAdvancedQuery_Combo_AndWithElemMatch verifies combining $and with $elemMatch. (DongoFull)
func TestAdvancedQuery_Combo_AndWithElemMatch(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("scores", bson.A{int32(80), int32(90)}), e("active", true)),
		d(e("_id", int32(2)), e("scores", bson.A{int32(80), int32(90)}), e("active", false)),
		d(e("_id", int32(3)), e("scores", bson.A{int32(50)}), e("active", true)),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("$and", bson.A{
			d(e("scores", d(e("$elemMatch", d(e("$gte", int32(85))))))),
			d(e("active", true)),
		})),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Combo_OrWithElemMatch verifies combining $or with $elemMatch. (DongoFull)
func TestAdvancedQuery_Combo_OrWithElemMatch(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("scores", bson.A{int32(95)})),
		d(e("_id", int32(2)), e("scores", bson.A{int32(50)})),
		d(e("_id", int32(3)), e("scores", bson.A{int32(5)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("$or", bson.A{
			d(e("scores", d(e("$elemMatch", d(e("$gte", int32(90))))))),
			d(e("scores", d(e("$elemMatch", d(e("$lte", int32(10))))))),
		})),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1, 3}, collectIDs(results))
}

// TestAdvancedQuery_Combo_AllAndSize verifies combining $all with $size. (DongoFull)
func TestAdvancedQuery_Combo_AllAndSize(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{"a", "b"})),       // size=2, has a,b
		d(e("_id", int32(2)), e("v", bson.A{"a", "b", "c"})),  // size=3, has a,b
		d(e("_id", int32(3)), e("v", bson.A{"a"})),             // size=1, no b
	)

	ctx := context.Background()
	// Require size=2 AND contains both a and b.
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$all", bson.A{"a", "b"}), e("$size", int32(2))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Combo_ExistsAndType verifies combining $exists and $type. (DongoFull)
func TestAdvancedQuery_Combo_ExistsAndType(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", int32(42))),
		d(e("_id", int32(2)), e("v", "hello")),
		d(e("_id", int32(3))),
	)

	ctx := context.Background()
	// v exists AND v is an integer.
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$exists", true), e("$type", "int")))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Combo_ElemMatchNested verifies nested $elemMatch on arrays of arrays. (DongoFull)
func TestAdvancedQuery_Combo_ElemMatchNested(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("groups", bson.A{
			d(e("members", bson.A{
				d(e("name", "alice"), e("age", int32(30))),
				d(e("name", "bob"), e("age", int32(25))),
			})),
		})),
		d(e("_id", int32(2)), e("groups", bson.A{
			d(e("members", bson.A{
				d(e("name", "charlie"), e("age", int32(20))),
			})),
		})),
	)

	ctx := context.Background()
	// Find docs where some group has a member over 28.
	cursor, err := coll.Find(ctx,
		d(e("groups", d(e("$elemMatch", d(
			e("members", d(e("$elemMatch", d(e("age", d(e("$gt", int32(28)))))))),
		))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_Combo_TypeOnMixedArray verifies $type checks correctly on arrays
// containing multiple types. (DongoFull)
func TestAdvancedQuery_Combo_TypeOnMixedArray(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{int32(1), "hello", true})),
		d(e("_id", int32(2)), e("v", bson.A{int32(1), int32(2)})),
	)

	ctx := context.Background()
	// $type "bool" should match doc1 because it has a boolean element.
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$type", "bool")))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_ScalarEq verifies {$elemMatch: {$eq: N}} works on scalar arrays. (DongoFull)
func TestAdvancedQuery_ElemMatch_ScalarEq(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{int32(1), int32(5), int32(9)})),
		d(e("_id", int32(2)), e("v", bson.A{int32(2), int32(3)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$elemMatch", d(e("$eq", int32(5))))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_GteLteRange verifies that a specific range check requires
// a SINGLE element to be in-range (not one element for each bound). (DongoFull)
func TestAdvancedQuery_ElemMatch_GteLteRange(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		// [1, 100]: 1 is < 10, 100 is > 90. No element is in [10, 90].
		d(e("_id", int32(1)), e("v", bson.A{int32(1), int32(100)})),
		// [50]: in [10, 90].
		d(e("_id", int32(2)), e("v", bson.A{int32(50)})),
		// [10, 90]: boundary values — both satisfy {$gte:10, $lte:90}.
		d(e("_id", int32(3)), e("v", bson.A{int32(10), int32(90)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$elemMatch", d(e("$gte", int32(10)), e("$lte", int32(90))))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	// Doc1: no element in [10,90]. Docs 2,3: have elements in range.
	assert.Equal(t, []int32{2, 3}, collectIDs(results))
}

// TestAdvancedQuery_All_MultipleValues verifies $all with more than 2 required values. (DongoFull)
func TestAdvancedQuery_All_MultipleValues(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", bson.A{"a", "b", "c", "d"})),
		d(e("_id", int32(2)), e("v", bson.A{"a", "b"})),
		d(e("_id", int32(3)), e("v", bson.A{"a", "b", "c"})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$all", bson.A{"a", "b", "c"})))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1, 3}, collectIDs(results))
}

// TestAdvancedQuery_Type_Double verifies {$type: "double"} matches float64 fields. (DongoFull)
func TestAdvancedQuery_Type_Double(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", float64(3.14))),
		d(e("_id", int32(2)), e("v", int32(3))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$type", "double")))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_GtWithSort verifies $elemMatch works correctly with sorting. (DongoFull)
func TestAdvancedQuery_ElemMatch_GtWithSort(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(3)), e("v", bson.A{int32(100)})),
		d(e("_id", int32(1)), e("v", bson.A{int32(5)})),
		d(e("_id", int32(2)), e("v", bson.A{int32(50)})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("v", d(e("$elemMatch", d(e("$gt", int32(10))))))),
		options.Find().SetSort(d(e("_id", int32(1)))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{2, 3}, collectIDs(results))
}

// TestAdvancedQuery_ElemMatch_DocEq verifies $elemMatch with exact value equality
// on a document field. (DongoFull)
func TestAdvancedQuery_ElemMatch_DocEq(t *testing.T) {
	t.Parallel()
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("items", bson.A{
			d(e("name", "pencil"), e("qty", int32(5))),
			d(e("name", "pen"), e("qty", int32(10))),
		})),
		d(e("_id", int32(2)), e("items", bson.A{
			d(e("name", "eraser"), e("qty", int32(1))),
		})),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx,
		d(e("items", d(e("$elemMatch", d(e("name", "pen")))))),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, []int32{1}, collectIDs(results))
}

// collectIDs extracts int32 _id values from a slice of documents, in order.
func collectIDs(docs []bson.D) []int32 {
	ids := make([]int32, 0, len(docs))
	for _, doc := range docs {
		for _, el := range doc {
			if el.Key == "_id" {
				if id, ok := el.Value.(int32); ok {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}
