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

// Package tests contains integration tests for dongo operators.
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

// TestAggPipeline_sample tests the $sample aggregation stage.
func TestAggPipeline_sample(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a")),
		d(e("_id", "b")),
		d(e("_id", "c")),
		d(e("_id", "d")),
		d(e("_id", "e")),
	)

	t.Run("SizeZero", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sample", d(e("size", int32(0))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 0)
	})

	t.Run("SizeOne", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sample", d(e("size", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 1)
	})

	t.Run("SizeThree", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sample", d(e("size", int32(3))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 3)

		// All returned docs must be from the original collection.
		validIDs := map[interface{}]bool{"a": true, "b": true, "c": true, "d": true, "e": true}
		for _, r := range results {
			for _, kv := range r {
				if kv.Key == "_id" {
					assert.True(t, validIDs[kv.Value], "unexpected _id: %v", kv.Value)
				}
			}
		}
	})

	t.Run("SizeExceedsCollection", func(t *testing.T) {
		t.Parallel()

		// When size > number of docs, return all docs.
		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sample", d(e("size", int32(100))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 5)
	})

	t.Run("SizeEqualCollection", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sample", d(e("size", int32(5))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 5)
	})

	t.Run("NoDuplicates", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sample", d(e("size", int32(5))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 5)

		// No duplicates.
		seen := map[interface{}]bool{}
		for _, r := range results {
			for _, kv := range r {
				if kv.Key == "_id" {
					assert.False(t, seen[kv.Value], "duplicate _id: %v", kv.Value)
					seen[kv.Value] = true
				}
			}
		}
	})

	t.Run("EmptyCollection", func(t *testing.T) {
		t.Parallel()

		emptyColl := env.collection(t)

		ctx := context.Background()
		cursor, err := emptyColl.Aggregate(ctx, bson.A{
			d(e("$sample", d(e("size", int32(3))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 0)
	})
}

// TestAggPipeline_sampleErrors tests $sample stage error cases.
func TestAggPipeline_sampleErrors(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	t.Run("NegativeSize", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$sample", d(e("size", int32(-1))))),
		})
		require.Error(t, err)

		var cmdErr mongo.CommandError
		require.ErrorAs(t, err, &cmdErr)
		assert.EqualValues(t, 28747, cmdErr.Code)
	})

	t.Run("MissingSize", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$sample", d())),
		})
		require.Error(t, err)

		var cmdErr mongo.CommandError
		require.ErrorAs(t, err, &cmdErr)
		assert.EqualValues(t, 28745, cmdErr.Code)
	})

	t.Run("NonDocumentSpec", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$sample", "not-a-doc")),
		})
		require.Error(t, err)

		var cmdErr mongo.CommandError
		require.ErrorAs(t, err, &cmdErr)
		assert.EqualValues(t, 28745, cmdErr.Code)
	})

	t.Run("StringSize", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$sample", d(e("size", "not-a-number")))),
		})
		require.Error(t, err)

		var cmdErr mongo.CommandError
		require.ErrorAs(t, err, &cmdErr)
		assert.EqualValues(t, 28745, cmdErr.Code)
	})
}

// TestAggPipeline_allowDiskUse tests that allowDiskUse option is accepted without error.
func TestAggPipeline_allowDiskUse(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "x"), e("v", int32(1))),
		d(e("_id", "y"), e("v", int32(2))),
		d(e("_id", "z"), e("v", int32(3))),
	)

	ctx := context.Background()
	opts := options.Aggregate().SetAllowDiskUse(true)

	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("v", int32(-1))))),
	}, opts)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 3)
}

// TestAggPipeline_comment tests that comment option is accepted without error.
func TestAggPipeline_comment(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "x"), e("v", int32(1))),
	)

	ctx := context.Background()
	opts := options.Aggregate().SetComment("test comment for profiling")

	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$limit", int32(10))),
	}, opts)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 1)
}

// TestAggPipeline_hint tests that hint option is accepted without error and returns correct results.
func TestAggPipeline_hint(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "x"), e("v", int32(1))),
		d(e("_id", "y"), e("v", int32(2))),
	)

	ctx := context.Background()
	// Use the default _id index hint.
	opts := options.Aggregate().SetHint(d(e("_id", int32(1))))

	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$match", d(e("v", d(e("$gt", int32(0))))))),
	}, opts)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 2)
}
