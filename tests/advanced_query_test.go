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

// Advanced query operator parity tests.
//
// Test naming conventions:
//   - DongoFull: test expected to pass on both Dongo and MongoDB.
//   - DongoXFail: test expected to fail on Dongo (known limitation); uses dongoXFail() to skip.
//     Once the underlying issue is fixed, remove the dongoXFail() call and the test becomes a
//     passing DongoFull test.

package tests

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// createTextIndex is a helper that creates a text index on the given fields.
func createTextIndex(t *testing.T, coll *mongo.Collection, fields bson.D, name string) {
	t.Helper()

	model := mongo.IndexModel{
		Keys:    fields,
		Options: options.Index().SetName(name),
	}

	ctx := context.Background()
	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
}

// TestTextSearch_BasicMatch verifies that $text search returns documents containing the search term.
func TestTextSearch_BasicMatch(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	createTextIndex(t, coll, bson.D{{"title", "text"}}, "title_text")

	insertDocs(t, coll,
		d(e("_id", "a"), e("title", "MongoDB is a document database")),
		d(e("_id", "b"), e("title", "Redis is an in-memory store")),
		d(e("_id", "c"), e("title", "MongoDB supports rich queries")),
	)

	ctx := context.Background()
	filter := bson.D{{"$text", bson.D{{"$search", "MongoDB"}}}}
	cursor, err := coll.Find(ctx, filter, options.Find().SetSort(d(e("_id", 1))))
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	ids := make([]string, len(results))
	for i, r := range results {
		for _, kv := range r {
			if kv.Key == "_id" {
				ids[i] = kv.Value.(string)
			}
		}
	}

	sort.Strings(ids)
	assert.Equal(t, []string{"a", "c"}, ids)
}

// TestTextSearch_MultiTerm verifies that multiple space-separated terms are ANDed.
func TestTextSearch_MultiTerm(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	createTextIndex(t, coll, bson.D{{"body", "text"}}, "body_text")

	insertDocs(t, coll,
		d(e("_id", "x"), e("body", "quick brown fox")),
		d(e("_id", "y"), e("body", "lazy brown dog")),
		d(e("_id", "z"), e("body", "quick fox jumps")),
	)

	ctx := context.Background()
	// Both "quick" AND "brown" must be present.
	filter := bson.D{{"$text", bson.D{{"$search", "quick brown"}}}}
	cursor, err := coll.Find(ctx, filter, options.Find().SetSort(d(e("_id", 1))))
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	ids := make([]string, len(results))
	for i, r := range results {
		for _, kv := range r {
			if kv.Key == "_id" {
				ids[i] = kv.Value.(string)
			}
		}
	}

	assert.Equal(t, []string{"x"}, ids)
}

// TestTextSearch_NegatedTerm verifies that prefixing a term with '-' excludes matching documents.
func TestTextSearch_NegatedTerm(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	createTextIndex(t, coll, bson.D{{"title", "text"}}, "title_text")

	insertDocs(t, coll,
		d(e("_id", 1), e("title", "apple pie recipe")),
		d(e("_id", 2), e("title", "apple cider vinegar")),
		d(e("_id", 3), e("title", "banana bread recipe")),
	)

	ctx := context.Background()
	// "apple" but NOT "cider".
	filter := bson.D{{"$text", bson.D{{"$search", "apple -cider"}}}}
	cursor, err := coll.Find(ctx, filter, options.Find().SetSort(d(e("_id", 1))))
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Equal(t, 1, len(results))
	for _, kv := range results[0] {
		if kv.Key == "_id" {
			assert.Equal(t, int32(1), kv.Value)
		}
	}
}

// TestTextSearch_CaseSensitive verifies that $caseSensitive controls case matching.
func TestTextSearch_CaseSensitive(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	createTextIndex(t, coll, bson.D{{"name", "text"}}, "name_text")

	insertDocs(t, coll,
		d(e("_id", "u"), e("name", "Go programming language")),
		d(e("_id", "v"), e("name", "go is fast")),
	)

	ctx := context.Background()

	// Case-insensitive (default): both docs match "go".
	filter := bson.D{{"$text", bson.D{{"$search", "go"}, {"$caseSensitive", false}}}}
	cursor, err := coll.Find(ctx, filter, options.Find().SetSort(d(e("_id", 1))))
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, 2, len(results), "case-insensitive search should match both docs")

	// Case-sensitive: only "go is fast" matches lowercase "go".
	filter = bson.D{{"$text", bson.D{{"$search", "go"}, {"$caseSensitive", true}}}}
	cursor, err = coll.Find(ctx, filter, options.Find().SetSort(d(e("_id", 1))))
	require.NoError(t, err)

	results = nil
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, 1, len(results), "case-sensitive search should match only the lowercase doc")

	for _, kv := range results[0] {
		if kv.Key == "_id" {
			assert.Equal(t, "v", kv.Value)
		}
	}
}

// TestTextSearch_NoMatch verifies that $text search returns empty result when nothing matches.
func TestTextSearch_NoMatch(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	createTextIndex(t, coll, bson.D{{"desc", "text"}}, "desc_text")

	insertDocs(t, coll,
		d(e("_id", 1), e("desc", "foo bar baz")),
	)

	ctx := context.Background()
	filter := bson.D{{"$text", bson.D{{"$search", "qux"}}}}
	cursor, err := coll.Find(ctx, filter)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Equal(t, 0, len(results))
}

// TestTextSearch_MetaTextScore verifies that {$meta: "textScore"} projection works.
func TestTextSearch_MetaTextScore(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	createTextIndex(t, coll, bson.D{{"content", "text"}}, "content_text")

	insertDocs(t, coll,
		d(e("_id", 1), e("content", "hello world")),
		d(e("_id", 2), e("content", "goodbye world")),
	)

	ctx := context.Background()
	filter := bson.D{{"$text", bson.D{{"$search", "world"}}}}
	projection := bson.D{{"score", bson.D{{"$meta", "textScore"}}}}

	cursor, err := coll.Find(ctx, filter, options.Find().SetProjection(projection))
	require.NoError(t, err)

	var results []bson.M
	require.NoError(t, cursor.All(ctx, &results))

	require.Equal(t, 2, len(results))

	for _, r := range results {
		score, ok := r["score"]
		require.True(t, ok, "expected 'score' field in projection result")
		_, isFloat := score.(float64)
		assert.True(t, isFloat, "expected score to be a float64, got %T", score)
	}
}

// TestTextSearch_MultiField verifies $text searches across multiple text-indexed fields.
func TestTextSearch_MultiField(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	createTextIndex(t, coll, bson.D{{"title", "text"}, {"body", "text"}}, "title_body_text")

	insertDocs(t, coll,
		d(e("_id", "a"), e("title", "introduction"), e("body", "this is about databases")),
		d(e("_id", "b"), e("title", "advanced topics"), e("body", "indexes and queries")),
		d(e("_id", "c"), e("title", "no match here"), e("body", "unrelated content")),
	)

	// "introduction" appears only in title.
	filter := bson.D{{"$text", bson.D{{"$search", "introduction"}}}}
	ids := queryIDs(t, coll, filter)
	assert.Equal(t, []interface{}{"a"}, ids)

	// "databases" appears only in body.
	filter = bson.D{{"$text", bson.D{{"$search", "databases"}}}}
	ids = queryIDs(t, coll, filter)
	assert.Equal(t, []interface{}{"a"}, ids)

	// "indexes" appears only in body of doc "b".
	filter = bson.D{{"$text", bson.D{{"$search", "indexes"}}}}
	ids = queryIDs(t, coll, filter)
	assert.Equal(t, []interface{}{"b"}, ids)
}
