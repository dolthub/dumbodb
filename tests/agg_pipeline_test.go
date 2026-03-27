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

// ─── $sortByCount ─────────────────────────────────────────────────────────────

// TestAggPipeline_sortByCount tests the $sortByCount aggregation stage.
func TestAggPipeline_sortByCount(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("category", "A")),
		d(e("_id", int32(2)), e("category", "B")),
		d(e("_id", int32(3)), e("category", "A")),
		d(e("_id", int32(4)), e("category", "C")),
		d(e("_id", int32(5)), e("category", "A")),
		d(e("_id", int32(6)), e("category", "B")),
	)

	t.Run("BasicCount", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sortByCount", "$category")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 3)

		// Results should be sorted by count descending: A(3), B(2), C(1).
		assert.Equal(t, "A", results[0].Map()["_id"])
		assert.Equal(t, int32(3), results[0].Map()["count"])
		assert.Equal(t, "B", results[1].Map()["_id"])
		assert.Equal(t, int32(2), results[1].Map()["count"])
		assert.Equal(t, "C", results[2].Map()["_id"])
		assert.Equal(t, int32(1), results[2].Map()["count"])
	})

	t.Run("EmptyCollection", func(t *testing.T) {
		t.Parallel()

		emptyColl := env.collection(t)

		ctx := context.Background()
		cursor, err := emptyColl.Aggregate(ctx, bson.A{
			d(e("$sortByCount", "$category")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 0)
	})

	t.Run("AllSameValue", func(t *testing.T) {
		t.Parallel()

		sameColl := env.collection(t)
		insertDocs(t, sameColl,
			d(e("_id", int32(10)), e("x", "same")),
			d(e("_id", int32(11)), e("x", "same")),
			d(e("_id", int32(12)), e("x", "same")),
		)

		ctx := context.Background()
		cursor, err := sameColl.Aggregate(ctx, bson.A{
			d(e("$sortByCount", "$x")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.Equal(t, "same", results[0].Map()["_id"])
		assert.Equal(t, int32(3), results[0].Map()["count"])
	})

	t.Run("MissingField", func(t *testing.T) {
		t.Parallel()

		mixedColl := env.collection(t)
		insertDocs(t, mixedColl,
			d(e("_id", int32(20)), e("cat", "X")),
			d(e("_id", int32(21))), // missing cat field → null
			d(e("_id", int32(22))), // missing cat field → null
		)

		ctx := context.Background()
		cursor, err := mixedColl.Aggregate(ctx, bson.A{
			d(e("$sortByCount", "$cat")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		// null (count=2) and X (count=1).
		require.Len(t, results, 2)
		assert.Equal(t, int32(2), results[0].Map()["count"])
		assert.Equal(t, int32(1), results[1].Map()["count"])
	})
}

// TestAggPipeline_sortByCountErrors tests $sortByCount error cases.
func TestAggPipeline_sortByCountErrors(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	t.Run("NullExpression", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$sortByCount", nil)),
		})
		require.Error(t, err)
	})
}

// ─── $bucket ──────────────────────────────────────────────────────────────────

// TestAggPipeline_bucket tests the $bucket aggregation stage.
func TestAggPipeline_bucket(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("price", int32(5))),
		d(e("_id", int32(2)), e("price", int32(15))),
		d(e("_id", int32(3)), e("price", int32(25))),
		d(e("_id", int32(4)), e("price", int32(35))),
		d(e("_id", int32(5)), e("price", int32(45))),
	)

	t.Run("BasicBuckets", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucket", d(
				e("groupBy", "$price"),
				e("boundaries", bson.A{int32(0), int32(20), int32(40)}),
				e("default", "Other"),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		// Bucket [0,20): price 5, 15 → count 2.
		// Bucket [20,40): price 25, 35 → count 2.
		// Default "Other": price 45 → count 1.
		require.Len(t, results, 3)
		assert.Equal(t, int32(0), results[0].Map()["_id"])
		assert.Equal(t, int32(2), results[0].Map()["count"])
		assert.Equal(t, int32(20), results[1].Map()["_id"])
		assert.Equal(t, int32(2), results[1].Map()["count"])
		assert.Equal(t, "Other", results[2].Map()["_id"])
		assert.Equal(t, int32(1), results[2].Map()["count"])
	})

	t.Run("NoDefault_AllFit", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucket", d(
				e("groupBy", "$price"),
				e("boundaries", bson.A{int32(0), int32(50)}),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.Equal(t, int32(0), results[0].Map()["_id"])
		assert.Equal(t, int32(5), results[0].Map()["count"])
	})

	t.Run("EmptyBucketsSkipped", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucket", d(
				e("groupBy", "$price"),
				e("boundaries", bson.A{int32(0), int32(10), int32(100)}),
				e("default", "Other"),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		// [0,10): price 5 → count 1.
		// [10,100): price 15, 25, 35, 45 → count 4.
		require.Len(t, results, 2)
	})

	t.Run("WithOutputAccumulator", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucket", d(
				e("groupBy", "$price"),
				e("boundaries", bson.A{int32(0), int32(30), int32(60)}),
				e("output", d(
					e("total", d(e("$sum", "$price"))),
					e("cnt", d(e("$sum", int32(1)))),
				)),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		// [0,30): 5+15+25 = 45.
		assert.Equal(t, int32(0), results[0].Map()["_id"])
		// [30,60): 35+45 = 80.
		assert.Equal(t, int32(30), results[1].Map()["_id"])
	})
}

// TestAggPipeline_bucketErrors tests $bucket error cases.
func TestAggPipeline_bucketErrors(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	t.Run("MissingGroupBy", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucket", d(
				e("boundaries", bson.A{int32(0), int32(10)}),
			))),
		})
		require.Error(t, err)
	})

	t.Run("MissingBoundaries", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucket", d(
				e("groupBy", "$x"),
			))),
		})
		require.Error(t, err)
	})

	t.Run("TooFewBoundaries", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucket", d(
				e("groupBy", "$x"),
				e("boundaries", bson.A{int32(0)}),
			))),
		})
		require.Error(t, err)
	})

	t.Run("UnsortedBoundaries", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucket", d(
				e("groupBy", "$x"),
				e("boundaries", bson.A{int32(10), int32(5)}),
			))),
		})
		require.Error(t, err)
	})

	t.Run("NoDefaultValueOutOfRange", func(t *testing.T) {
		t.Parallel()

		outOfRangeColl := env.collection(t)
		insertDocs(t, outOfRangeColl,
			d(e("_id", int32(1)), e("x", int32(100))),
		)

		ctx := context.Background()
		cursor, err := outOfRangeColl.Aggregate(ctx, bson.A{
			d(e("$bucket", d(
				e("groupBy", "$x"),
				e("boundaries", bson.A{int32(0), int32(10)}),
			))),
		})
		// Error should be returned at iteration time (cursor.All).
		if err == nil {
			var results []bson.D
			err = cursor.All(ctx, &results)
		}
		require.Error(t, err)
	})
}

// ─── $bucketAuto ──────────────────────────────────────────────────────────────

// TestAggPipeline_bucketAuto tests the $bucketAuto aggregation stage.
func TestAggPipeline_bucketAuto(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("score", int32(10))),
		d(e("_id", int32(2)), e("score", int32(20))),
		d(e("_id", int32(3)), e("score", int32(30))),
		d(e("_id", int32(4)), e("score", int32(40))),
		d(e("_id", int32(5)), e("score", int32(50))),
		d(e("_id", int32(6)), e("score", int32(60))),
	)

	t.Run("TwoBuckets", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucketAuto", d(
				e("groupBy", "$score"),
				e("buckets", int32(2)),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 2)

		// Each result has _id: {min: X, max: Y} and count.
		for _, r := range results {
			_, hasID := r.Map()["_id"]
			assert.True(t, hasID, "result missing _id")
			_, hasCount := r.Map()["count"]
			assert.True(t, hasCount, "result missing count")
		}
	})

	t.Run("ThreeBuckets", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucketAuto", d(
				e("groupBy", "$score"),
				e("buckets", int32(3)),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 3)
	})

	t.Run("BucketsExceedDocs", func(t *testing.T) {
		t.Parallel()

		fewColl := env.collection(t)
		insertDocs(t, fewColl,
			d(e("_id", int32(10)), e("v", int32(1))),
			d(e("_id", int32(11)), e("v", int32(2))),
		)

		ctx := context.Background()
		cursor, err := fewColl.Aggregate(ctx, bson.A{
			d(e("$bucketAuto", d(
				e("groupBy", "$v"),
				e("buckets", int32(10)),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		// Can't have more buckets than documents.
		assert.LessOrEqual(t, len(results), 2)
	})

	t.Run("EmptyCollection", func(t *testing.T) {
		t.Parallel()

		emptyColl := env.collection(t)

		ctx := context.Background()
		cursor, err := emptyColl.Aggregate(ctx, bson.A{
			d(e("$bucketAuto", d(
				e("groupBy", "$score"),
				e("buckets", int32(3)),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 0)
	})

	t.Run("OneBucket", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucketAuto", d(
				e("groupBy", "$score"),
				e("buckets", int32(1)),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 1)
	})
}

// TestAggPipeline_bucketAutoErrors tests $bucketAuto error cases.
func TestAggPipeline_bucketAutoErrors(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	t.Run("MissingGroupBy", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucketAuto", d(
				e("buckets", int32(3)),
			))),
		})
		require.Error(t, err)
	})

	t.Run("MissingBuckets", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucketAuto", d(
				e("groupBy", "$score"),
			))),
		})
		require.Error(t, err)
	})

	t.Run("ZeroBuckets", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucketAuto", d(
				e("groupBy", "$score"),
				e("buckets", int32(0)),
			))),
		})
		require.Error(t, err)
	})

	t.Run("NegativeBuckets", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucketAuto", d(
				e("groupBy", "$score"),
				e("buckets", int32(-1)),
			))),
		})
		require.Error(t, err)
	})
}

// ─── $facet ───────────────────────────────────────────────────────────────────

// TestAggPipeline_facet tests the $facet aggregation stage.
func TestAggPipeline_facet(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("category", "A"), e("price", int32(10))),
		d(e("_id", int32(2)), e("category", "B"), e("price", int32(20))),
		d(e("_id", int32(3)), e("category", "A"), e("price", int32(30))),
		d(e("_id", int32(4)), e("category", "C"), e("price", int32(40))),
		d(e("_id", int32(5)), e("category", "B"), e("price", int32(50))),
	)

	t.Run("TwoSubPipelines", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$facet", d(
				e("byCategory", bson.A{
					d(e("$sortByCount", "$category")),
				}),
				e("priceBuckets", bson.A{
					d(e("$bucket", d(
						e("groupBy", "$price"),
						e("boundaries", bson.A{int32(0), int32(25), int32(60)}),
					))),
				}),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		// $facet always returns exactly one document.
		require.Len(t, results, 1)

		result := results[0]
		_, hasCategory := result.Map()["byCategory"]
		assert.True(t, hasCategory, "result missing byCategory field")
		_, hasBuckets := result.Map()["priceBuckets"]
		assert.True(t, hasBuckets, "result missing priceBuckets field")
	})

	t.Run("SingleSubPipeline", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$facet", d(
				e("categories", bson.A{
					d(e("$group", d(e("_id", "$category")))),
					d(e("$sort", d(e("_id", int32(1))))),
				}),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)

		categoriesRaw, ok := results[0].Map()["categories"]
		require.True(t, ok, "result missing categories field")
		categories, ok := categoriesRaw.(bson.A)
		require.True(t, ok, "categories is not an array")
		// A, B, C → 3 groups.
		assert.Len(t, categories, 3)
	})

	t.Run("EmptyCollection", func(t *testing.T) {
		t.Parallel()

		emptyColl := env.collection(t)

		ctx := context.Background()
		cursor, err := emptyColl.Aggregate(ctx, bson.A{
			d(e("$facet", d(
				e("items", bson.A{
					d(e("$count", "total")),
				}),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		// $facet always emits one document even with empty input.
		assert.Len(t, results, 1)
	})

	t.Run("SubPipelineWithMatch", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$facet", d(
				e("cheap", bson.A{
					d(e("$match", d(e("price", d(e("$lte", int32(20))))))),
				}),
				e("expensive", bson.A{
					d(e("$match", d(e("price", d(e("$gt", int32(30))))))),
				}),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)

		cheapRaw := results[0].Map()["cheap"]
		cheap, ok := cheapRaw.(bson.A)
		require.True(t, ok, "cheap is not an array")
		// price <= 20: ids 1,2 → 2 docs.
		assert.Len(t, cheap, 2)

		expensiveRaw := results[0].Map()["expensive"]
		expensive, ok := expensiveRaw.(bson.A)
		require.True(t, ok, "expensive is not an array")
		// price > 30: ids 4,5 → 2 docs.
		assert.Len(t, expensive, 2)
	})
}

// TestAggPipeline_facetErrors tests $facet error cases.
func TestAggPipeline_facetErrors(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	t.Run("NonDocumentSpec", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$facet", "not-a-doc")),
		})
		require.Error(t, err)
	})

	t.Run("EmptySpec", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$facet", d())),
		})
		require.Error(t, err)
	})

	t.Run("FieldValueNotArray", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$facet", d(e("myField", "not-an-array")))),
		})
		require.Error(t, err)
	})
}
