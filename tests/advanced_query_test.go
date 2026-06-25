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
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestAdvancedQuery_JsonSchema_ExclusiveMinimum_Maximum verifies $jsonSchema with
// exclusiveMinimum and exclusiveMaximum numeric constraints. (DumboDBFull)
func TestAdvancedQuery_JsonSchema_ExclusiveMinimum_Maximum(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

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

// TestAdvancedQuery_JsonSchema_MultipleOf verifies $jsonSchema multipleOf constraint. (DumboDBFull)
func TestAdvancedQuery_JsonSchema_MultipleOf(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

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
// the schema doesn't match any document. (DumboDBFull)
func TestAdvancedQuery_JsonSchema_NoMatch(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("x", int32(1))),
		d(e("_id", int32(2)), e("x", int32(2))),
	)

	ctx := context.Background()
	// Schema requires field "missing" to be present  -- no doc has it.
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
// 'required' keys is rejected with FailedToParse (code 9). (DumboDBFull)
func TestAdvancedQuery_JsonSchema_DuplicateRequired(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

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

// TestAdvancedQuery_JsonSchema_OneOf verifies $jsonSchema oneOf constraint. (DumboDBFull)
func TestAdvancedQuery_JsonSchema_OneOf(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

	insertDocs(t, coll,
		// Matches only the int schema (bsonType int only) -> oneOf passes (exactly 1 match).
		d(e("_id", int32(1)), e("val", int32(42))),
		// Matches only the string schema -> oneOf passes.
		d(e("_id", int32(2)), e("val", "hello")),
		// float64 matches neither int nor string -> oneOf fails.
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
// strips whitespace and comments from the pattern. (DumboDBFull)
func TestAdvancedQuery_Regex_ExtendedWhitespace_x_Flag(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

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
// assertion is evaluated correctly using the PCRE-compatible engine. (DumboDBFull)
func TestAdvancedQuery_Regex_LookaheadSupported(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("name", "foobar")),
		d(e("_id", int32(2)), e("name", "bazqux")),
	)

	ctx := context.Background()
	// "(?=foo)" is a PCRE lookahead  -- matches strings where "foo" follows at the current position.
	// "foobar" matches (lookahead succeeds at position 0); "bazqux" does not.
	cursor, err := coll.Find(ctx,
		d(e("name", d(e("$regex", "(?=foo)")))),
	)
	require.NoError(t, err, "lookahead regex should be supported via PCRE engine")

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, int32(1), dmap(results[0])["_id"])
}

// TestAdvancedQuery_TextSearch_MetaTextScore_Projection verifies that $text search
// combined with {$meta: "textScore"} projection returns the score field (0.0). (DumboDBFull)
func TestAdvancedQuery_TextSearch_MetaTextScore_Projection(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

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

	// score field must be present and numeric (DumboDB returns 0.0).
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
// can be sorted by {$meta: "textScore"} without error. (DumboDBFull)
func TestAdvancedQuery_TextSearch_MetaTextScore_Sort(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

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
// terms matches documents containing any of the terms. (DumboDBFull)
func TestAdvancedQuery_TextSearch_MultipleTerms(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("body", "the quick brown fox")),
		d(e("_id", int32(2)), e("body", "jumped over the lazy dog")),
		d(e("_id", int32(3)), e("body", "lorem ipsum dolor")),
	)

	ctx := context.Background()
	// Search for "fox" OR "dog"  -- should match docs 1 and 2.
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

// TestAdvancedQuery_TextSearch_WithAdditionalFilter verifies that $text search
// combined with an additional equality filter returns only documents that satisfy
// both conditions. (DumboDBFull)
func TestAdvancedQuery_TextSearch_WithAdditionalFilter(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("body", "apple pie"), e("category", "food")),
		d(e("_id", int32(2)), e("body", "apple juice"), e("category", "drink")),
		d(e("_id", int32(3)), e("body", "banana split"), e("category", "food")),
	)

	ctx := context.Background()
	// Text search for "apple" AND category == "food"  -- only doc 1 qualifies.
	cursor, err := coll.Find(ctx,
		d(e("$text", d(e("$search", "apple"))), e("category", "food")),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1, "combined text+field filter must return only matching docs")

	ids := collectIDs(results)
	assert.Equal(t, []int32{1}, ids)
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
