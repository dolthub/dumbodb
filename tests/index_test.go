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
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestIndex_TextCreate verifies that a text index can be created on a single field.
func TestIndex_TextCreate(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()

	model := mongo.IndexModel{
		Keys: bson.D{{"title", "text"}},
		Options: options.Index().SetName("title_text"),
	}

	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	assert.Equal(t, "title_text", name)

	// Verify the index appears in listIndexes.
	cursor, err := coll.Indexes().List(ctx)
	require.NoError(t, err)

	var indexes []bson.D
	require.NoError(t, cursor.All(ctx, &indexes))

	found := false
	for _, idx := range indexes {
		for _, e := range idx {
			if e.Key == "name" && e.Value == "title_text" {
				found = true
			}
		}
	}
	assert.True(t, found, "expected title_text index in listIndexes result")
}

// TestIndex_TextCreateMultiField verifies that a text index can span multiple fields.
func TestIndex_TextCreateMultiField(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()

	model := mongo.IndexModel{
		Keys: bson.D{{"title", "text"}, {"body", "text"}},
		Options: options.Index().SetName("title_body_text"),
	}

	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	assert.Equal(t, "title_body_text", name)
}

// TestIndex_TextCreateWithOptions verifies that text index options are accepted.
func TestIndex_TextCreateWithOptions(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()

	model := mongo.IndexModel{
		Keys: bson.D{{"content", "text"}},
		Options: options.Index().
			SetName("content_text").
			SetDefaultLanguage("english").
			SetWeights(bson.D{{"content", 1}}),
	}

	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	assert.Equal(t, "content_text", name)
}

// TestIndex_TextOnlyOnePerCollection verifies that only one text index is allowed per collection.
func TestIndex_TextOnlyOnePerCollection(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()

	// Create the first text index.
	first := mongo.IndexModel{
		Keys:    bson.D{{"title", "text"}},
		Options: options.Index().SetName("title_text"),
	}
	_, err := coll.Indexes().CreateOne(ctx, first)
	require.NoError(t, err)

	// Attempting to create a second text index should fail.
	second := mongo.IndexModel{
		Keys:    bson.D{{"body", "text"}},
		Options: options.Index().SetName("body_text"),
	}
	_, err = coll.Indexes().CreateOne(ctx, second)
	require.Error(t, err, "expected error when creating a second text index")
}

// TestIndex_TextWildcard verifies that a wildcard text index ($**) can be created.
func TestIndex_TextWildcard(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()

	model := mongo.IndexModel{
		Keys:    bson.D{{"$**", "text"}},
		Options: options.Index().SetName("wildcard_text"),
	}

	name, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)
	assert.Equal(t, "wildcard_text", name)
}

// TestIndex_TextListIndexesShowsTextKey verifies that listIndexes shows the "text" key type.
func TestIndex_TextListIndexesShowsTextKey(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()

	model := mongo.IndexModel{
		Keys:    bson.D{{"summary", "text"}},
		Options: options.Index().SetName("summary_text"),
	}

	_, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	cursor, err := coll.Indexes().List(ctx)
	require.NoError(t, err)

	var indexes []bson.M
	require.NoError(t, cursor.All(ctx, &indexes))

	var textKeyFound bool
	for _, idx := range indexes {
		name, _ := idx["name"].(string)
		if name != "summary_text" {
			continue
		}

		key, ok := idx["key"].(bson.M)
		require.True(t, ok, "expected key to be a document")

		val, exists := key["summary"]
		require.True(t, exists, "expected summary field in text index key")
		assert.Equal(t, "text", val, "expected text index key value to be \"text\"")
		textKeyFound = true
	}

	assert.True(t, textKeyFound, "expected to find summary_text index in listIndexes")
}
