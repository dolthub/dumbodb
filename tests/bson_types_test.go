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

// TestBSON_minkey_maxkey_insert verifies that documents containing MinKey and MaxKey
// BSON types can be inserted and retrieved correctly. (DumboDBFull)
func TestBSON_minkey_maxkey_insert(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()

	// Insert documents with MinKey and MaxKey special BSON types.
	_, err := coll.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(1)},
		{Key: "k", Value: bson.MinKey{}},
	})
	require.NoError(t, err, "inserting a document with MinKey must succeed")

	_, err = coll.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "k", Value: bson.MaxKey{}},
	})
	require.NoError(t, err, "inserting a document with MaxKey must succeed")

	// Retrieve and verify MinKey document.
	var minDoc bson.D
	err = coll.FindOne(ctx, bson.D{{Key: "_id", Value: int32(1)}}).Decode(&minDoc)
	require.NoError(t, err)

	var foundMinKey bool
	for _, el := range minDoc {
		if el.Key == "k" {
			_, foundMinKey = el.Value.(bson.MinKey)
			break
		}
	}
	assert.True(t, foundMinKey, "retrieved document must contain MinKey value")

	// Retrieve and verify MaxKey document.
	var maxDoc bson.D
	err = coll.FindOne(ctx, bson.D{{Key: "_id", Value: int32(2)}}).Decode(&maxDoc)
	require.NoError(t, err)

	var foundMaxKey bool
	for _, el := range maxDoc {
		if el.Key == "k" {
			_, foundMaxKey = el.Value.(bson.MaxKey)
			break
		}
	}
	assert.True(t, foundMaxKey, "retrieved document must contain MaxKey value")
}

// TestBSON_minkey_sort_order verifies that MinKey sorts before all other values
// and MaxKey sorts after all other values. (DumboDBFull)
func TestBSON_minkey_sort_order(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", int32(42))),
		d(e("_id", int32(2)), e("v", "hello")),
	)

	ctx := context.Background()

	// Insert MinKey and MaxKey docs.
	_, err := coll.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(3)},
		{Key: "v", Value: bson.MinKey{}},
	})
	require.NoError(t, err)

	_, err = coll.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(4)},
		{Key: "v", Value: bson.MaxKey{}},
	})
	require.NoError(t, err)

	// Sort ascending by "v": MinKey < numbers < strings < MaxKey.
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetSort(bson.D{{Key: "v", Value: 1}}),
	)
	require.NoError(t, err)
	defer cursor.Close(ctx)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 4)

	// First doc must be the MinKey doc (id=3).
	var firstID int32
	for _, el := range results[0] {
		if el.Key == "_id" {
			firstID = el.Value.(int32)
		}
	}
	assert.Equal(t, int32(3), firstID, "MinKey must sort first in ascending order")

	// Last doc must be the MaxKey doc (id=4).
	var lastID int32
	for _, el := range results[3] {
		if el.Key == "_id" {
			lastID = el.Value.(int32)
		}
	}
	assert.Equal(t, int32(4), lastID, "MaxKey must sort last in ascending order")
}

// TestBSON_minkey_type_filter verifies that $type queries with "minKey" and "maxKey"
// string aliases correctly match documents containing those BSON types. (DumboDBFull)
func TestBSON_minkey_type_filter(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("val", int32(42))),
	)

	ctx := context.Background()

	_, err := coll.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(2)},
		{Key: "val", Value: bson.MinKey{}},
	})
	require.NoError(t, err)

	_, err = coll.InsertOne(ctx, bson.D{
		{Key: "_id", Value: int32(3)},
		{Key: "val", Value: bson.MaxKey{}},
	})
	require.NoError(t, err)

	// Query for minKey type.
	cur, err := coll.Find(ctx, bson.D{{Key: "val", Value: bson.D{{Key: "$type", Value: "minKey"}}}})
	require.NoError(t, err)
	defer cur.Close(ctx)

	var minResults []bson.D
	require.NoError(t, cur.All(ctx, &minResults))
	require.Len(t, minResults, 1, "$type:'minKey' must match exactly one document")
	for _, el := range minResults[0] {
		if el.Key == "_id" {
			assert.Equal(t, int32(2), el.Value)
		}
	}

	// Query for maxKey type.
	cur2, err := coll.Find(ctx, bson.D{{Key: "val", Value: bson.D{{Key: "$type", Value: "maxKey"}}}})
	require.NoError(t, err)
	defer cur2.Close(ctx)

	var maxResults []bson.D
	require.NoError(t, cur2.All(ctx, &maxResults))
	require.Len(t, maxResults, 1, "$type:'maxKey' must match exactly one document")
	for _, el := range maxResults[0] {
		if el.Key == "_id" {
			assert.Equal(t, int32(3), el.Value)
		}
	}
}
