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
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestCappedCollection_Create tests that capped collections can be created with
// the capped, size, and max options.
func TestCappedCollection_Create(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	db := env.client.Database("testdb")
	ctx := context.Background()

	collName := fmt.Sprintf("capped_%d", randID())

	err := db.CreateCollection(ctx, collName, options.CreateCollection().
		SetCapped(true).
		SetSizeInBytes(1024*1024))
	require.NoError(t, err)

	// Verify collection appears in listCollections with capped=true.
	cursor, err := db.ListCollections(ctx, bson.D{{"name", collName}})
	require.NoError(t, err)
	var colls []bson.D
	require.NoError(t, cursor.All(ctx, &colls))
	require.Len(t, colls, 1)

	var cappedField bool
	for _, elem := range colls[0] {
		if elem.Key == "options" {
			if opts, ok := elem.Value.(bson.D); ok {
				for _, opt := range opts {
					if opt.Key == "capped" {
						cappedField, _ = opt.Value.(bool)
					}
				}
			}
		}
	}
	assert.True(t, cappedField, "capped collection should report capped=true in listCollections")
}

// TestCappedCollection_MaxDocuments tests that a capped collection with a max
// document count enforces FIFO eviction when the limit is exceeded.
func TestCappedCollection_MaxDocuments(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	db := env.client.Database("testdb")
	ctx := context.Background()

	collName := fmt.Sprintf("capped_max_%d", randID())
	const maxDocs = 3

	err := db.CreateCollection(ctx, collName, options.CreateCollection().
		SetCapped(true).
		SetSizeInBytes(1024*1024).
		SetMaxDocuments(maxDocs))
	require.NoError(t, err)

	coll := db.Collection(collName)

	// Insert more documents than the max.
	for i := 1; i <= 5; i++ {
		_, err := coll.InsertOne(ctx, bson.D{{"_id", fmt.Sprintf("doc%d", i)}, {"seq", int32(i)}})
		require.NoError(t, err)
	}

	// Only the last maxDocs documents should remain (FIFO eviction).
	count, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.Equal(t, int64(maxDocs), count, "capped collection should retain at most %d documents", maxDocs)

	// The oldest documents should have been evicted.
	var results []bson.D
	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{"seq", 1}}))
	require.NoError(t, err)
	require.NoError(t, cursor.All(ctx, &results))

	require.Len(t, results, maxDocs)
	// Documents 1 and 2 should have been evicted; documents 3, 4, 5 remain.
	seqs := make([]int32, 0, maxDocs)
	for _, r := range results {
		for _, elem := range r {
			if elem.Key == "seq" {
				if v, ok := elem.Value.(int32); ok {
					seqs = append(seqs, v)
				}
			}
		}
	}
	assert.Equal(t, []int32{3, 4, 5}, seqs, "oldest documents should have been evicted")
}

// TestCappedCollection_SizeLimit tests that a capped collection with only a size
// limit enforces eviction when the estimated size is exceeded.
func TestCappedCollection_SizeLimit(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	db := env.client.Database("testdb")
	ctx := context.Background()

	collName := fmt.Sprintf("capped_size_%d", randID())
	// Use a very small size to trigger eviction: 512 bytes with avgDocSize=512 means 1 doc max.
	const cappedSize = 512

	err := db.CreateCollection(ctx, collName, options.CreateCollection().
		SetCapped(true).
		SetSizeInBytes(cappedSize))
	require.NoError(t, err)

	coll := db.Collection(collName)

	// Insert 3 documents; size limit should evict old ones.
	for i := 1; i <= 3; i++ {
		_, err := coll.InsertOne(ctx, bson.D{{"_id", fmt.Sprintf("doc%d", i)}, {"seq", int32(i)}})
		require.NoError(t, err)
	}

	// Should have evicted old docs due to size limit.
	count, err := coll.CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	assert.LessOrEqual(t, count, int64(2), "size-capped collection should evict old docs")
}

// TestCollectionView_Create tests that a collection view can be created with
// viewOn and pipeline options.
func TestCollectionView_Create(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	db := env.client.Database("testdb")
	ctx := context.Background()

	// Create source collection with some documents.
	srcName := fmt.Sprintf("src_%d", randID())
	viewName := fmt.Sprintf("view_%d", randID())

	err := db.CreateCollection(ctx, srcName)
	require.NoError(t, err)

	srcColl := db.Collection(srcName)
	insertDocs(t, srcColl,
		d(e("_id", "a"), e("val", int32(1))),
		d(e("_id", "b"), e("val", int32(2))),
		d(e("_id", "c"), e("val", int32(3))),
	)

	// Create a view on the source collection.
	err = db.CreateView(ctx, viewName, srcName, mongo.Pipeline{})
	require.NoError(t, err)

	// Verify view appears in listCollections with type="view".
	cursor, err := db.ListCollections(ctx, bson.D{{"name", viewName}})
	require.NoError(t, err)
	var colls []bson.D
	require.NoError(t, cursor.All(ctx, &colls))
	require.Len(t, colls, 1, "view should appear in listCollections")

	var collType string
	for _, elem := range colls[0] {
		if elem.Key == "type" {
			collType, _ = elem.Value.(string)
		}
	}
	assert.Equal(t, "view", collType, "collection view should have type='view'")
}

// TestCollectionView_Read tests that a view supports read operations and
// returns documents from the underlying collection.
func TestCollectionView_Read(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	db := env.client.Database("testdb")
	ctx := context.Background()

	srcName := fmt.Sprintf("src_%d", randID())
	viewName := fmt.Sprintf("view_%d", randID())

	err := db.CreateCollection(ctx, srcName)
	require.NoError(t, err)

	srcColl := db.Collection(srcName)
	insertDocs(t, srcColl,
		d(e("_id", "a"), e("val", int32(1))),
		d(e("_id", "b"), e("val", int32(2))),
		d(e("_id", "c"), e("val", int32(3))),
	)

	err = db.CreateView(ctx, viewName, srcName, mongo.Pipeline{})
	require.NoError(t, err)

	// Find from the view should return the source collection's documents.
	viewColl := db.Collection(viewName)
	cursor, err := viewColl.Find(ctx, bson.D{})
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Len(t, results, 3, "view should return all documents from source collection")
}

// TestCollectionView_WriteRejected tests that write operations on a view are rejected.
func TestCollectionView_WriteRejected(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	db := env.client.Database("testdb")
	ctx := context.Background()

	srcName := fmt.Sprintf("src_%d", randID())
	viewName := fmt.Sprintf("view_%d", randID())

	err := db.CreateCollection(ctx, srcName)
	require.NoError(t, err)

	err = db.CreateView(ctx, viewName, srcName, mongo.Pipeline{})
	require.NoError(t, err)

	viewColl := db.Collection(viewName)

	// Insert should fail.
	_, err = viewColl.InsertOne(ctx, bson.D{{"_id", "x"}, {"val", int32(1)}})
	assert.Error(t, err, "insert into a view should be rejected")

	// Verify the error is about the view not being writable.
	if cmdErr, ok := err.(mongo.CommandError); ok {
		assert.Equal(t, int32(166), cmdErr.Code, "expected error code 166 (CommandNotSupportedOnView)")
	}
}

// TestCollectionView_Validate tests that validate() works on a view and returns
// a response indicating it is valid.
func TestCollectionView_Validate(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	db := env.client.Database("testdb")
	ctx := context.Background()

	srcName := fmt.Sprintf("src_%d", randID())
	viewName := fmt.Sprintf("view_%d", randID())

	err := db.CreateCollection(ctx, srcName)
	require.NoError(t, err)

	err = db.CreateView(ctx, viewName, srcName, mongo.Pipeline{})
	require.NoError(t, err)

	// Validate should succeed for a view.
	var result bson.D
	err = db.RunCommand(ctx, bson.D{{"validate", viewName}}).Decode(&result)
	require.NoError(t, err)

	var ok float64
	for _, elem := range result {
		if elem.Key == "ok" {
			ok, _ = elem.Value.(float64)
		}
	}
	assert.Equal(t, float64(1), ok, "validate should return ok=1 for a view")
}

// randID returns a random int64 to generate unique collection names in parallel tests.
func randID() int64 {
	return rand.Int64()
}
