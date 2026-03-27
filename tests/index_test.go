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

// Tests for index parity between MongoDB and Dongo.
//
// Coverage:
//   - createIndexes / dropIndexes / listIndexes for all index types
//   - Single-field, compound, multikey, text, 2dsphere, 2d, hashed, wildcard
//   - Partial, sparse, unique, TTL, hidden, clustered
//   - Index usage via hint() and explain()
//   - Error cases for invalid specs
//
// DongoXFail tests document correct MongoDB behaviour that Dongo has not yet
// implemented. Remove the dongoXFail() call when Dongo gains support.

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

// ─── helpers ─────────────────────────────────────────────────────────────────

// indexNames returns the set of index names present in the collection.
func indexNames(t *testing.T, coll *mongo.Collection) map[string]bool {
	t.Helper()
	ctx := context.Background()
	cursor, err := coll.Indexes().List(ctx)
	require.NoError(t, err)
	var docs []bson.M
	require.NoError(t, cursor.All(ctx, &docs))
	names := make(map[string]bool, len(docs))
	for _, doc := range docs {
		if name, ok := doc["name"].(string); ok {
			names[name] = true
		}
	}
	return names
}

// createIndex is a convenience wrapper around Indexes().CreateOne.
func createIndex(t *testing.T, coll *mongo.Collection, model mongo.IndexModel) string {
	t.Helper()
	name, err := coll.Indexes().CreateOne(context.Background(), model)
	require.NoError(t, err)
	return name
}

// ─── Single-field indexes ─────────────────────────────────────────────────────

// TestIndex_SingleFieldAscending verifies that a basic ascending single-field index
// can be created and appears in listIndexes. (DongoFull)
func TestIndex_SingleFieldAscending(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"score", 1}},
		Options: options.Index().SetName("score_1"),
	})
	assert.Equal(t, "score_1", name)
	assert.True(t, indexNames(t, coll)["score_1"], "score_1 should appear in listIndexes")
}

// TestIndex_SingleFieldDescending verifies that a descending single-field index
// can be created and listed. (DongoFull)
func TestIndex_SingleFieldDescending(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"ts", -1}},
		Options: options.Index().SetName("ts_neg1"),
	})
	assert.Equal(t, "ts_neg1", name)
	assert.True(t, indexNames(t, coll)["ts_neg1"])
}

// TestIndex_SingleFieldAutoName verifies that MongoDB generates a name like
// "field_1" when no explicit name is given. (DongoFull)
func TestIndex_SingleFieldAutoName(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys: bson.D{{"email", 1}},
	})
	assert.Equal(t, "email_1", name)
	assert.True(t, indexNames(t, coll)["email_1"])
}

// TestIndex_ListIndexesAlwaysHasId verifies that _id_ is always present. (DongoFull)
func TestIndex_ListIndexesAlwaysHasId(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	// Insert a doc to materialise the collection.
	_, err := coll.InsertOne(context.Background(), bson.D{{"x", 1}})
	require.NoError(t, err)

	assert.True(t, indexNames(t, coll)["_id_"], "_id_ index must always be present")
}

// ─── Compound indexes ─────────────────────────────────────────────────────────

// TestIndex_CompoundTwoFields verifies that a two-field compound index can be
// created and listed. (DongoFull)
func TestIndex_CompoundTwoFields(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"last", 1}, {"first", 1}},
		Options: options.Index().SetName("last_first"),
	})
	assert.Equal(t, "last_first", name)
	assert.True(t, indexNames(t, coll)["last_first"])
}

// TestIndex_CompoundThreeFields verifies that a three-field compound index
// is created correctly. (DongoFull)
func TestIndex_CompoundThreeFields(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"a", 1}, {"b", -1}, {"c", 1}},
		Options: options.Index().SetName("abc_compound"),
	})
	assert.Equal(t, "abc_compound", name)
	assert.True(t, indexNames(t, coll)["abc_compound"])
}

// TestIndex_CompoundAutoName verifies that MongoDB generates a composite name
// like "a_1_b_-1" when no explicit name is given. (DongoFull)
func TestIndex_CompoundAutoName(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys: bson.D{{"x", 1}, {"y", -1}},
	})
	assert.Equal(t, "x_1_y_-1", name)
}

// ─── Multikey indexes (on array fields) ──────────────────────────────────────

// TestIndex_MultikeyCreation verifies that an index on an array field is
// created successfully and functions as a multikey index. (DongoFull)
func TestIndex_MultikeyCreation(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	// Insert documents with array fields.
	insertDocs(t, coll,
		d(e("_id", "a"), e("tags", bson.A{"go", "db"})),
		d(e("_id", "b"), e("tags", bson.A{"db", "sql"})),
		d(e("_id", "c"), e("tags", bson.A{"go"})),
	)

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"tags", 1}},
		Options: options.Index().SetName("tags_1"),
	})

	assert.True(t, indexNames(t, coll)["tags_1"])

	// Verify the index is usable for querying.
	cursor, err := coll.Find(ctx, bson.D{{"tags", "go"}})
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 2)
}

// TestIndex_MultikeyCompound verifies a compound index where one field is an
// array (multikey compound). (DongoFull)
func TestIndex_MultikeyCompound(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"category", 1}, {"tags", 1}},
		Options: options.Index().SetName("cat_tags"),
	})

	assert.True(t, indexNames(t, coll)["cat_tags"])
}

// ─── Unique indexes ───────────────────────────────────────────────────────────

// TestIndex_UniqueBasic verifies that a unique index is created and enforces
// uniqueness on insertion. (DongoXFail)
func TestIndex_UniqueBasic(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "unique index constraint enforcement not yet implemented for non-_id fields")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"email", 1}},
		Options: options.Index().SetName("email_unique").SetUnique(true),
	})

	_, err := coll.InsertOne(ctx, bson.D{{"email", "a@b.com"}})
	require.NoError(t, err)

	_, err = coll.InsertOne(ctx, bson.D{{"email", "a@b.com"}})
	require.Error(t, err)
	assert.True(t, mongo.IsDuplicateKeyError(err), "expected duplicate key error")
}

// TestIndex_UniqueCompound verifies that a compound unique index is created
// and enforces joint-field uniqueness. (DongoXFail)
func TestIndex_UniqueCompound(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "unique index constraint enforcement not yet implemented for non-_id fields")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"first", 1}, {"last", 1}},
		Options: options.Index().SetName("name_unique").SetUnique(true),
	})

	_, err := coll.InsertOne(ctx, bson.D{{"first", "John"}, {"last", "Doe"}})
	require.NoError(t, err)

	// Same combination should fail.
	_, err = coll.InsertOne(ctx, bson.D{{"first", "John"}, {"last", "Doe"}})
	require.Error(t, err)
	assert.True(t, mongo.IsDuplicateKeyError(err))

	// Different combination should succeed.
	_, err = coll.InsertOne(ctx, bson.D{{"first", "Jane"}, {"last", "Doe"}})
	require.NoError(t, err)
}

// TestIndex_UniqueListIndexesFlag verifies that listIndexes returns the
// unique flag for a unique index. (DongoFull)
func TestIndex_UniqueListIndexesFlag(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"uid", 1}},
		Options: options.Index().SetName("uid_unique").SetUnique(true),
	})

	cursor, err := coll.Indexes().List(ctx)
	require.NoError(t, err)
	var indexes []bson.M
	require.NoError(t, cursor.All(ctx, &indexes))

	for _, idx := range indexes {
		if idx["name"] == "uid_unique" {
			assert.Equal(t, true, idx["unique"], "unique flag should be true")
			return
		}
	}
	t.Fatal("uid_unique index not found in listIndexes")
}

// ─── Sparse indexes ───────────────────────────────────────────────────────────

// TestIndex_SparseCreation verifies that a sparse index can be created.
// Dongo accepts the sparse option but does not currently enforce it. (DongoFull)
func TestIndex_SparseCreation(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	// Sparse option is accepted but silently ignored per current implementation.
	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"optfield", 1}},
		Options: options.Index().SetName("optfield_sparse").SetSparse(true),
	})
	assert.Equal(t, "optfield_sparse", name)
	assert.True(t, indexNames(t, coll)["optfield_sparse"])
}

// TestIndex_SparseUnique verifies that a sparse unique index can be created.
// MongoDB allows multiple documents missing the indexed field with sparse+unique. (DongoFull)
func TestIndex_SparseUnique(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"phone", 1}},
		Options: options.Index().SetName("phone_sparse_unique").SetSparse(true).SetUnique(true),
	})
	assert.Equal(t, "phone_sparse_unique", name)
}

// ─── Text indexes ─────────────────────────────────────────────────────────────

// TestIndex_TextCreate verifies that a text index can be created on a single field
// and appears in listIndexes. (DongoFull)
func TestIndex_TextCreate(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	name, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"title", "text"}},
		Options: options.Index().SetName("title_text"),
	})
	require.NoError(t, err)
	assert.Equal(t, "title_text", name)

	cursor, err := coll.Indexes().List(ctx)
	require.NoError(t, err)
	var indexes []bson.D
	require.NoError(t, cursor.All(ctx, &indexes))

	found := false
	for _, idx := range indexes {
		for _, elem := range idx {
			if elem.Key == "name" && elem.Value == "title_text" {
				found = true
			}
		}
	}
	assert.True(t, found, "title_text index should appear in listIndexes")
}

// TestIndex_TextCreateMultiField verifies that a text index can span multiple fields. (DongoFull)
func TestIndex_TextCreateMultiField(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"title", "text"}, {"body", "text"}},
		Options: options.Index().SetName("title_body_text"),
	})
	assert.Equal(t, "title_body_text", name)
}

// TestIndex_TextWildcard verifies that a wildcard text index ($**) can be created. (DongoFull)
func TestIndex_TextWildcard(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"$**", "text"}},
		Options: options.Index().SetName("wildcard_text"),
	})
	assert.Equal(t, "wildcard_text", name)
}

// TestIndex_TextOnlyOnePerCollection verifies that only one text index is
// permitted per collection. (DongoFull)
func TestIndex_TextOnlyOnePerCollection(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"title", "text"}},
		Options: options.Index().SetName("title_text"),
	})
	require.NoError(t, err)

	_, err = coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"body", "text"}},
		Options: options.Index().SetName("body_text"),
	})
	require.Error(t, err, "expected error when creating a second text index")
}

// TestIndex_TextCreateWithOptions verifies that text index options (default_language,
// weights) are accepted. (DongoFull)
func TestIndex_TextCreateWithOptions(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys: bson.D{{"content", "text"}},
		Options: options.Index().
			SetName("content_text").
			SetDefaultLanguage("english").
			SetWeights(bson.D{{"content", 1}}),
	})
	assert.Equal(t, "content_text", name)
}

// TestIndex_TextListIndexesShowsTextKey verifies that listIndexes shows the
// "text" key type in the key document. (DongoFull)
func TestIndex_TextListIndexesShowsTextKey(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"summary", "text"}},
		Options: options.Index().SetName("summary_text"),
	})
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

// ─── Hashed indexes ───────────────────────────────────────────────────────────

// TestIndex_HashedCreation verifies that a hashed index can be created. (DongoXFail)
func TestIndex_HashedCreation(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "hashed index key type not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"shardKey", "hashed"}},
		Options: options.Index().SetName("shardKey_hashed"),
	})
	assert.Equal(t, "shardKey_hashed", name)
	assert.True(t, indexNames(t, coll)["shardKey_hashed"])
}

// ─── 2dsphere indexes ────────────────────────────────────────────────────────

// TestIndex_2dsphereCreation verifies that a 2dsphere geospatial index can be
// created on a location field. (DongoXFail)
func TestIndex_2dsphereCreation(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index type not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"location", "2dsphere"}},
		Options: options.Index().SetName("location_2dsphere"),
	})
	assert.Equal(t, "location_2dsphere", name)
	assert.True(t, indexNames(t, coll)["location_2dsphere"])
}

// TestIndex_2dsphereCompound verifies a compound index with a 2dsphere component. (DongoXFail)
func TestIndex_2dsphereCompound(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index type not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"category", 1}, {"location", "2dsphere"}},
		Options: options.Index().SetName("cat_loc_2dsphere"),
	})
	assert.Equal(t, "cat_loc_2dsphere", name)
}

// TestIndex_2dsphereQuery verifies that a $near query works with a 2dsphere index. (DongoXFail)
func TestIndex_2dsphereQuery(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2dsphere index type and $near query not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"loc", "2dsphere"}},
		Options: options.Index().SetName("loc_2dsphere"),
	})

	insertDocs(t, coll,
		d(e("_id", "nyc"), e("loc", bson.D{{"type", "Point"}, {"coordinates", bson.A{-74.0060, 40.7128}}})),
		d(e("_id", "la"), e("loc", bson.D{{"type", "Point"}, {"coordinates", bson.A{-118.2437, 34.0522}}})),
	)

	cursor, err := coll.Find(ctx, bson.D{{
		"loc", bson.D{{"$near", bson.D{
			{"$geometry", bson.D{{"type", "Point"}, {"coordinates", bson.A{-74.0, 40.7}}}},
			{"$maxDistance", 10000},
		}}},
	}})
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "nyc", results[0].Map()["_id"])
}

// ─── 2d (legacy) indexes ──────────────────────────────────────────────────────

// TestIndex_2dCreation verifies that a legacy 2d index can be created. (DongoXFail)
func TestIndex_2dCreation(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "2d (legacy) index type not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"coords", "2d"}},
		Options: options.Index().SetName("coords_2d"),
	})
	assert.Equal(t, "coords_2d", name)
	assert.True(t, indexNames(t, coll)["coords_2d"])
}

// ─── Wildcard indexes ─────────────────────────────────────────────────────────

// TestIndex_WildcardAllFields verifies that a wildcard index on all fields
// can be created. (DongoXFail)
func TestIndex_WildcardAllFields(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "wildcard index ($**) not yet implemented as a distinct index type")

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"$**", 1}},
		Options: options.Index().SetName("wildcard_all"),
	})
	assert.Equal(t, "wildcard_all", name)
	assert.True(t, indexNames(t, coll)["wildcard_all"])
}

// TestIndex_WildcardSubfield verifies that a wildcard index on a sub-path
// can be created. (DongoXFail)
func TestIndex_WildcardSubfield(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "wildcard index on sub-path not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"metadata.$**", 1}},
		Options: options.Index().SetName("meta_wildcard"),
	})
	assert.Equal(t, "meta_wildcard", name)
}

// ─── Partial indexes ──────────────────────────────────────────────────────────

// TestIndex_PartialFilter verifies that a partial index with a filter
// expression can be created. (DongoXFail)
func TestIndex_PartialFilter(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "partialFilterExpression not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys: bson.D{{"status", 1}},
		Options: options.Index().
			SetName("active_status").
			SetPartialFilterExpression(bson.D{{"status", "active"}}),
	})
	assert.Equal(t, "active_status", name)
	assert.True(t, indexNames(t, coll)["active_status"])
}

// TestIndex_PartialFilterWithRange verifies a partial index using a range
// filter expression. (DongoXFail)
func TestIndex_PartialFilterWithRange(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "partialFilterExpression not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys: bson.D{{"score", 1}},
		Options: options.Index().
			SetName("high_scores").
			SetPartialFilterExpression(bson.D{{"score", bson.D{{"$gt", 90}}}}),
	})
	assert.Equal(t, "high_scores", name)
}

// ─── TTL indexes ──────────────────────────────────────────────────────────────

// TestIndex_TTLCreation verifies that a TTL index can be created with
// expireAfterSeconds. (DongoXFail)
func TestIndex_TTLCreation(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "expireAfterSeconds (TTL indexes) not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"createdAt", 1}},
		Options: options.Index().SetName("ttl_createdAt").SetExpireAfterSeconds(3600),
	})
	assert.Equal(t, "ttl_createdAt", name)
	assert.True(t, indexNames(t, coll)["ttl_createdAt"])
}

// TestIndex_TTLListIndexesShowsExpiry verifies that listIndexes exposes the
// expireAfterSeconds field for TTL indexes. (DongoXFail)
func TestIndex_TTLListIndexesShowsExpiry(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "expireAfterSeconds (TTL indexes) not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"expiresAt", 1}},
		Options: options.Index().SetName("ttl_idx").SetExpireAfterSeconds(7200),
	})

	cursor, err := coll.Indexes().List(ctx)
	require.NoError(t, err)
	var indexes []bson.M
	require.NoError(t, cursor.All(ctx, &indexes))

	for _, idx := range indexes {
		if idx["name"] == "ttl_idx" {
			assert.EqualValues(t, 7200, idx["expireAfterSeconds"],
				"expireAfterSeconds should be present in listIndexes for TTL index")
			return
		}
	}
	t.Fatal("ttl_idx not found in listIndexes")
}

// ─── Hidden indexes ───────────────────────────────────────────────────────────

// TestIndex_HiddenCreation verifies that a hidden index can be created. (DongoXFail)
func TestIndex_HiddenCreation(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "hidden index option not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)

	name := createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"field", 1}},
		Options: options.Index().SetName("hidden_idx").SetHidden(true),
	})
	assert.Equal(t, "hidden_idx", name)
}

// TestIndex_HiddenListIndexesFlag verifies that listIndexes marks the index
// as hidden. (DongoXFail)
func TestIndex_HiddenListIndexesFlag(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "hidden index option not yet implemented")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"val", 1}},
		Options: options.Index().SetName("val_hidden").SetHidden(true),
	})

	cursor, err := coll.Indexes().List(ctx)
	require.NoError(t, err)
	var indexes []bson.M
	require.NoError(t, cursor.All(ctx, &indexes))

	for _, idx := range indexes {
		if idx["name"] == "val_hidden" {
			assert.Equal(t, true, idx["hidden"], "hidden flag should be true in listIndexes")
			return
		}
	}
	t.Fatal("val_hidden not found in listIndexes")
}

// ─── Clustered indexes ────────────────────────────────────────────────────────

// TestIndex_ClusteredCollection verifies that a clustered collection (MongoDB 5.3+)
// can be created with a clustered index. (DongoXFail)
func TestIndex_ClusteredCollection(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "clustered indexes / clustered collections not yet implemented")

	env := startDongo(t)
	ctx := context.Background()

	// Clustered collections are created via createCollection with clusteredIndex option.
	err := env.client.Database("testdb").CreateCollection(ctx, "clustered_col_idx_test",
		options.CreateCollection().SetClusteredIndex(bson.D{
			{"key", bson.D{{"_id", 1}}},
			{"unique", true},
		}),
	)
	require.NoError(t, err)
}

// ─── dropIndexes ─────────────────────────────────────────────────────────────

// TestIndex_DropByName verifies that a named index can be dropped. (DongoXFail)
func TestIndex_DropByName(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "dropIndexes backend not yet implemented (stub returns ok without removing index)")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"x", 1}},
		Options: options.Index().SetName("x_1"),
	})
	require.True(t, indexNames(t, coll)["x_1"])

	_, err := coll.Indexes().DropOne(ctx, "x_1")
	require.NoError(t, err)

	assert.False(t, indexNames(t, coll)["x_1"], "x_1 should be absent after drop")
}

// TestIndex_DropIdIndexFails verifies that attempting to drop the _id index
// returns an error. (DongoFull)
func TestIndex_DropIdIndexFails(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	_, err := coll.InsertOne(ctx, bson.D{{"x", 1}})
	require.NoError(t, err)

	_, err = coll.Indexes().DropOne(ctx, "_id_")
	require.Error(t, err, "dropping the _id_ index should return an error")
}

// TestIndex_DropAll verifies that DropAll removes all non-_id indexes. (DongoXFail)
func TestIndex_DropAll(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "dropIndexes backend not yet implemented (stub returns ok without removing index)")

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"a", 1}},
		Options: options.Index().SetName("a_1"),
	})
	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"b", 1}},
		Options: options.Index().SetName("b_1"),
	})
	require.True(t, indexNames(t, coll)["a_1"])
	require.True(t, indexNames(t, coll)["b_1"])

	_, err := coll.Indexes().DropAll(ctx)
	require.NoError(t, err)

	names := indexNames(t, coll)
	assert.False(t, names["a_1"], "a_1 should be dropped")
	assert.False(t, names["b_1"], "b_1 should be dropped")
	// _id_ should survive DropAll.
	assert.True(t, names["_id_"], "_id_ should survive DropAll")
}

// TestIndex_DropNonExistentFails verifies that dropping a nonexistent index
// name returns an error. (DongoFull)
func TestIndex_DropNonExistentFails(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	_, err := coll.InsertOne(ctx, bson.D{{"x", 1}})
	require.NoError(t, err)

	_, err = coll.Indexes().DropOne(ctx, "no_such_index")
	require.Error(t, err, "dropping a nonexistent index should fail")
}

// ─── listIndexes ─────────────────────────────────────────────────────────────

// TestIndex_ListIndexesKeyDocument verifies that listIndexes returns the key
// document in the correct field. (DongoFull)
func TestIndex_ListIndexesKeyDocument(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"score", -1}},
		Options: options.Index().SetName("score_neg1"),
	})

	cursor, err := coll.Indexes().List(ctx)
	require.NoError(t, err)
	var indexes []bson.M
	require.NoError(t, cursor.All(ctx, &indexes))

	for _, idx := range indexes {
		if idx["name"] == "score_neg1" {
			key, ok := idx["key"].(bson.M)
			require.True(t, ok, "key field should be a document")
			val, exists := key["score"]
			require.True(t, exists, "score field should be present in key")
			assert.EqualValues(t, -1, val)
			return
		}
	}
	t.Fatal("score_neg1 not found in listIndexes")
}

// TestIndex_ListIndexesOnNonExistentCollection verifies that listIndexes on a
// collection that doesn't exist returns an empty cursor without error.
// The Go MongoDB driver converts NamespaceNotFound into an empty batch cursor. (DongoFull)
func TestIndex_ListIndexesOnNonExistentCollection(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	ctx := context.Background()

	coll := env.client.Database("testdb").Collection("no_such_col_xyz")
	cursor, err := coll.Indexes().List(ctx)
	// The Go driver converts NamespaceNotFound to an empty cursor (no error).
	require.NoError(t, err)

	var indexes []bson.M
	require.NoError(t, cursor.All(ctx, &indexes))
	assert.Empty(t, indexes, "non-existent collection should yield empty index list")
}

// TestIndex_ListIndexesMultiple verifies that creating multiple indexes results
// in all of them appearing in listIndexes. (DongoFull)
func TestIndex_ListIndexesMultiple(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	for _, model := range []mongo.IndexModel{
		{Keys: bson.D{{"a", 1}}, Options: options.Index().SetName("a_1")},
		{Keys: bson.D{{"b", 1}}, Options: options.Index().SetName("b_1")},
		{Keys: bson.D{{"c", -1}}, Options: options.Index().SetName("c_neg1")},
	} {
		createIndex(t, coll, model)
	}

	names := indexNames(t, coll)
	assert.True(t, names["a_1"])
	assert.True(t, names["b_1"])
	assert.True(t, names["c_neg1"])
	assert.True(t, names["_id_"])
}

// ─── createIndexes auto-creates collection ────────────────────────────────────

// TestIndex_CreateOnNonExistentCollectionAutoCreates verifies that createIndexes
// on a collection that doesn't exist automatically creates it. (DongoFull)
func TestIndex_CreateOnNonExistentCollectionAutoCreates(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	ctx := context.Background()

	coll := env.client.Database("testdb").Collection("autocreate_col_xyz")
	t.Cleanup(func() { coll.Drop(context.Background()) }) //nolint:errcheck

	name, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"v", 1}},
		Options: options.Index().SetName("v_1"),
	})
	require.NoError(t, err)
	assert.Equal(t, "v_1", name)
}

// ─── Hint / query planning ────────────────────────────────────────────────────

// TestIndex_HintByName verifies that Find accepts a hint by index name and
// returns correct results. Dongo accepts the hint but does not use it for
// query planning (hint is currently ignored). (DongoFull)
func TestIndex_HintByName(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
		d(e("_id", "c"), e("v", int32(3))),
	)

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"v", 1}},
		Options: options.Index().SetName("v_1"),
	})

	cursor, err := coll.Find(ctx, bson.D{{"v", bson.D{{"$gt", int32(1)}}}},
		options.Find().SetHint("v_1"),
	)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 2)
}

// TestIndex_HintByKeyDoc verifies that Find accepts a hint specified as a
// key document and returns correct results. (DongoFull)
func TestIndex_HintByKeyDoc(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(10))),
		d(e("_id", "b"), e("v", int32(20))),
	)

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"v", 1}},
		Options: options.Index().SetName("v_1"),
	})

	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetHint(bson.D{{"v", 1}}),
	)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 2)
}

// TestIndex_HintNaturalOrder verifies that {$natural: 1} hint is accepted. (DongoFull)
func TestIndex_HintNaturalOrder(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	insertDocs(t, coll,
		d(e("_id", "x"), e("v", int32(1))),
		d(e("_id", "y"), e("v", int32(2))),
	)

	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetHint(bson.D{{"$natural", 1}}),
	)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 2)
}

// TestIndex_HintIdIndex verifies that hinting the _id index is accepted. (DongoFull)
func TestIndex_HintIdIndex(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
	)

	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetHint(bson.D{{"_id", 1}}),
	)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 1)
}

// ─── explain() ───────────────────────────────────────────────────────────────

// TestIndex_ExplainFind verifies that explain() on a find query returns a
// document with an ok field. (DongoFull)
func TestIndex_ExplainFind(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
	)

	var result bson.M
	err := env.client.Database("testdb").RunCommand(ctx, bson.D{
		{"explain", bson.D{
			{"find", coll.Name()},
			{"filter", bson.D{{"v", 1}}},
		}},
	}).Decode(&result)
	require.NoError(t, err)
	assert.EqualValues(t, 1, result["ok"])
}

// TestIndex_ExplainWithHint verifies that explain() accepts a hint option. (DongoFull)
func TestIndex_ExplainWithHint(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	insertDocs(t, coll, d(e("_id", "a"), e("v", int32(1))))

	createIndex(t, coll, mongo.IndexModel{
		Keys:    bson.D{{"v", 1}},
		Options: options.Index().SetName("v_1"),
	})

	var result bson.M
	err := env.client.Database("testdb").RunCommand(ctx, bson.D{
		{"explain", bson.D{
			{"find", coll.Name()},
			{"filter", bson.D{{"v", 1}}},
			{"hint", "v_1"},
		}},
	}).Decode(&result)
	require.NoError(t, err)
	assert.EqualValues(t, 1, result["ok"])
}

// ─── Error cases for invalid specs ───────────────────────────────────────────

// TestIndex_ErrorEmptyKeyDocument verifies that an empty key document returns
// an appropriate error. (DongoFull)
func TestIndex_ErrorEmptyKeyDocument(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{},
		Options: options.Index().SetName("empty"),
	})
	require.Error(t, err, "empty key document should fail")
}

// TestIndex_ErrorDuplicateIndexNameDifferentKey verifies that creating an index
// with the same name as an existing index but a different key is rejected. (DongoFull)
func TestIndex_ErrorDuplicateIndexNameDifferentKey(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"a", 1}},
		Options: options.Index().SetName("conflict_name"),
	})
	require.NoError(t, err)

	// Same name, different key — should fail.
	_, err = coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"b", 1}},
		Options: options.Index().SetName("conflict_name"),
	})
	require.Error(t, err, "duplicate name with different key should return an error")
}

// TestIndex_ErrorSameKeyDifferentName verifies that an index with the same key
// but a different name is rejected. (DongoFull)
func TestIndex_ErrorSameKeyDifferentName(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"x", 1}},
		Options: options.Index().SetName("original_name"),
	})
	require.NoError(t, err)

	// Same key, different name — should fail.
	_, err = coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"x", 1}},
		Options: options.Index().SetName("different_name"),
	})
	require.Error(t, err, "same key with different name should return an error")
}

// TestIndex_ErrorIdIndexDescending verifies that {_id: -1} is rejected. (DongoFull)
func TestIndex_ErrorIdIndexDescending(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"_id", -1}},
		Options: options.Index().SetName("id_desc"),
	})
	require.Error(t, err, "{_id: -1} should be rejected")
}

// TestIndex_ErrorReservedNameForNonIdIndex verifies that using the reserved name
// _id_ for a non-_id index is rejected. (DongoFull)
func TestIndex_ErrorReservedNameForNonIdIndex(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"x", 1}},
		Options: options.Index().SetName("_id_"),
	})
	require.Error(t, err, "using _id_ name for non-_id index should be rejected")
}

// TestIndex_ErrorIdxKeyValueOutOfRange verifies that index key values other than
// 1 and -1 are rejected. (DongoFull)
func TestIndex_ErrorIdxKeyValueOutOfRange(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"x", 2}},
		Options: options.Index().SetName("x_2"),
	})
	require.Error(t, err, "index key value 2 should be rejected")
}

// TestIndex_IdempotentCreate verifies that creating the same index twice is
// idempotent (no error, returns the same index name). (DongoFull)
func TestIndex_IdempotentCreate(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)
	ctx := context.Background()

	model := mongo.IndexModel{
		Keys:    bson.D{{"z", 1}},
		Options: options.Index().SetName("z_1"),
	}

	name1, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	name2, err := coll.Indexes().CreateOne(ctx, model)
	require.NoError(t, err)

	assert.Equal(t, name1, name2, "creating the same index twice should return the same name")
}
