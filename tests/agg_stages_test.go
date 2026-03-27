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

// Package tests contains parity tests for aggregation pipeline stages.
//
// Test naming convention:
//   - DongoFull: normal test, expected to pass on Dongo.
//   - DongoXFail: test expected to fail on Dongo (known limitation); uses dongoXFail() to skip.
//
// Covered stages: $match, $group, $sort, $limit, $skip, $project, $unwind,
// $addFields, $set, $unset, $count, $replaceRoot, $replaceWith, $sortByCount,
// $facet, $bucket, $bucketAuto, $lookup, $out, $merge, $graphLookup.
// Also covers unsupported stage errors and multi-stage pipeline combinations.
package tests

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// ─── $match ──────────────────────────────────────────────────────────────────

// TestAggStage_match tests $match pipeline stage. (DongoFull)
func TestAggStage_match(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("x", int32(1))),
		d(e("_id", "b"), e("x", int32(2))),
		d(e("_id", "c"), e("x", int32(3))),
		d(e("_id", "d"), e("x", int32(4))),
	)

	t.Run("EqualityFilter", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$match", d(e("x", int32(2))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.Equal(t, "b", results[0].Map()["_id"])
	})

	t.Run("ComparisonOperator", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$match", d(e("x", d(e("$gt", int32(2))))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 2)
	})

	t.Run("NoMatchReturnsEmpty", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$match", d(e("x", int32(99))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 0)
	})

	t.Run("MatchAllWithExists", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$match", d(e("_id", d(e("$exists", true)))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 4)
	})

	t.Run("AndCondition", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$match", d(
				e("x", d(e("$gte", int32(2)), e("$lte", int32(3)))),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 2)
	})

	t.Run("InOperator", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$match", d(e("x", d(e("$in", bson.A{int32(1), int32(3)})))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 2)
	})
}

// ─── $group ───────────────────────────────────────────────────────────────────

// TestAggStage_group tests $group pipeline stage with various accumulators. (DongoFull)
func TestAggStage_group(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a1"), e("cat", "A"), e("v", int32(10))),
		d(e("_id", "a2"), e("cat", "A"), e("v", int32(20))),
		d(e("_id", "b1"), e("cat", "B"), e("v", int32(5))),
		d(e("_id", "b2"), e("cat", "B"), e("v", int32(15))),
		d(e("_id", "b3"), e("cat", "B"), e("v", int32(25))),
	)

	t.Run("CountAll", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$group", d(
				e("_id", nil),
				e("total", d(e("$sum", int32(1)))),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.EqualValues(t, 5, results[0].Map()["total"])
	})

	t.Run("GroupByField", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$group", d(
				e("_id", "$cat"),
				e("count", d(e("$sum", int32(1)))),
			))),
			d(e("$sort", d(e("_id", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		assert.Equal(t, "A", results[0].Map()["_id"])
		assert.EqualValues(t, 2, results[0].Map()["count"])
		assert.Equal(t, "B", results[1].Map()["_id"])
		assert.EqualValues(t, 3, results[1].Map()["count"])
	})

	t.Run("SumAccumulator", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$group", d(
				e("_id", "$cat"),
				e("total", d(e("$sum", "$v"))),
			))),
			d(e("$sort", d(e("_id", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		assert.EqualValues(t, 30, results[0].Map()["total"]) // A: 10+20
		assert.EqualValues(t, 45, results[1].Map()["total"]) // B: 5+15+25
	})

	t.Run("AvgAccumulator", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$group", d(
				e("_id", "$cat"),
				e("avg", d(e("$avg", "$v"))),
			))),
			d(e("$sort", d(e("_id", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		assert.EqualValues(t, 15.0, results[0].Map()["avg"]) // A: (10+20)/2
		assert.EqualValues(t, 15.0, results[1].Map()["avg"]) // B: (5+15+25)/3
	})

	t.Run("MinAccumulator", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$group", d(
				e("_id", "$cat"),
				e("lo", d(e("$min", "$v"))),
			))),
			d(e("$sort", d(e("_id", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		assert.EqualValues(t, 10, results[0].Map()["lo"]) // cat A: min is 10
		assert.EqualValues(t, 5, results[1].Map()["lo"])  // cat B: min is 5
	})

	t.Run("MaxAccumulator", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$group", d(
				e("_id", "$cat"),
				e("hi", d(e("$max", "$v"))),
			))),
			d(e("$sort", d(e("_id", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		assert.EqualValues(t, 20, results[0].Map()["hi"]) // cat A: max is 20
		assert.EqualValues(t, 25, results[1].Map()["hi"]) // cat B: max is 25
	})

	// TestAggStage_group/MinMaxTogether documents that using multiple accumulators
	// in the same $group stage does not yet work correctly — the group iterator is
	// consumed by the first accumulator, leaving subsequent ones empty. (DongoXFail)
	t.Run("MinMaxTogether", func(t *testing.T) {
		dongoXFail(t, "multiple accumulators per $group share iterator; second accumulator sees empty input")

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$group", d(
				e("_id", "$cat"),
				e("lo", d(e("$min", "$v"))),
				e("hi", d(e("$max", "$v"))),
			))),
			d(e("$sort", d(e("_id", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		assert.EqualValues(t, 10, results[0].Map()["lo"])
		assert.EqualValues(t, 20, results[0].Map()["hi"])
		assert.EqualValues(t, 5, results[1].Map()["lo"])
		assert.EqualValues(t, 25, results[1].Map()["hi"])
	})

	t.Run("PushAccumulator", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$group", d(
				e("_id", "$cat"),
				e("vals", d(e("$push", "$v"))),
			))),
			d(e("$sort", d(e("_id", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)

		// cat A: should have 2 elements
		valsA, ok := results[0].Map()["vals"].(bson.A)
		require.True(t, ok)
		assert.Len(t, valsA, 2)

		// cat B: should have 3 elements
		valsB, ok := results[1].Map()["vals"].(bson.A)
		require.True(t, ok)
		assert.Len(t, valsB, 3)
	})

	t.Run("FirstAccumulator", func(t *testing.T) {
		t.Parallel()

		// Insert docs in known order so $first is predictable.
		env2 := startDongo(t)
		coll2 := env2.collection(t)
		insertDocs(t, coll2,
			d(e("_id", "x1"), e("g", "X"), e("v", int32(1))),
			d(e("_id", "x2"), e("g", "X"), e("v", int32(2))),
			d(e("_id", "x3"), e("g", "X"), e("v", int32(3))),
		)

		ctx := context.Background()
		cursor, err := coll2.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("_id", int32(1))))),
			d(e("$group", d(
				e("_id", "$g"),
				e("first", d(e("$first", "$v"))),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.EqualValues(t, 1, results[0].Map()["first"])
	})

	t.Run("LastAccumulator", func(t *testing.T) {
		t.Parallel()

		// Insert docs in known order so $last is predictable.
		env2 := startDongo(t)
		coll2 := env2.collection(t)
		insertDocs(t, coll2,
			d(e("_id", "x1"), e("g", "X"), e("v", int32(1))),
			d(e("_id", "x2"), e("g", "X"), e("v", int32(2))),
			d(e("_id", "x3"), e("g", "X"), e("v", int32(3))),
		)

		ctx := context.Background()
		cursor, err := coll2.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("_id", int32(1))))),
			d(e("$group", d(
				e("_id", "$g"),
				e("last", d(e("$last", "$v"))),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.EqualValues(t, 3, results[0].Map()["last"])
	})

	// TestAggStage_group/FirstLastTogether documents that $first and $last in the
	// same $group share the iterator — $last sees empty input after $first consumes
	// it. (DongoXFail)
	t.Run("FirstLastTogether", func(t *testing.T) {
		dongoXFail(t, "multiple accumulators per $group share iterator; $last sees empty input after $first")

		env2 := startDongo(t)
		coll2 := env2.collection(t)
		insertDocs(t, coll2,
			d(e("_id", "x1"), e("g", "X"), e("v", int32(1))),
			d(e("_id", "x2"), e("g", "X"), e("v", int32(2))),
			d(e("_id", "x3"), e("g", "X"), e("v", int32(3))),
		)

		ctx := context.Background()
		cursor, err := coll2.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("_id", int32(1))))),
			d(e("$group", d(
				e("_id", "$g"),
				e("first", d(e("$first", "$v"))),
				e("last", d(e("$last", "$v"))),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.EqualValues(t, 1, results[0].Map()["first"])
		assert.EqualValues(t, 3, results[0].Map()["last"])
	})

	t.Run("NullIdGroupsAll", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$group", d(
				e("_id", nil),
				e("n", d(e("$sum", int32(1)))),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.EqualValues(t, 5, results[0].Map()["n"])
	})
}

// TestAggStage_groupErrors tests $group error cases. (DongoFull)
func TestAggStage_groupErrors(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	t.Run("MissingIDField", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$group", d(e("x", d(e("$sum", int32(1))))))),
		})
		// MongoDB requires _id field in $group.
		require.Error(t, err)
	})

	t.Run("NonDocumentSpec", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$group", "not-a-doc")),
		})
		require.Error(t, err)
	})
}

// ─── $sort ────────────────────────────────────────────────────────────────────

// TestAggStage_sort tests $sort pipeline stage. (DongoFull)
func TestAggStage_sort(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "c"), e("v", int32(3))),
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
	)

	t.Run("SortAscending", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("v", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 3)
		assert.Equal(t, "a", results[0].Map()["_id"])
		assert.Equal(t, "b", results[1].Map()["_id"])
		assert.Equal(t, "c", results[2].Map()["_id"])
	})

	t.Run("SortDescending", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("v", int32(-1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 3)
		assert.Equal(t, "c", results[0].Map()["_id"])
		assert.Equal(t, "b", results[1].Map()["_id"])
		assert.Equal(t, "a", results[2].Map()["_id"])
	})

	t.Run("SortByID", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("_id", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 3)
		assert.Equal(t, "a", results[0].Map()["_id"])
		assert.Equal(t, "b", results[1].Map()["_id"])
		assert.Equal(t, "c", results[2].Map()["_id"])
	})

	t.Run("SortEmptyCollection", func(t *testing.T) {
		t.Parallel()

		emptyColl := env.collection(t)

		ctx := context.Background()
		cursor, err := emptyColl.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("v", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 0)
	})
}

// ─── $limit ───────────────────────────────────────────────────────────────────

// TestAggStage_limit tests $limit pipeline stage. (DongoFull)
func TestAggStage_limit(t *testing.T) {
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

	t.Run("LimitOne", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("_id", int32(1))))),
			d(e("$limit", int64(1))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.Equal(t, "a", results[0].Map()["_id"])
	})

	t.Run("LimitThree", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("_id", int32(1))))),
			d(e("$limit", int64(3))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 3)
	})

	t.Run("LimitExceedsCollection", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$limit", int64(100))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 5)
	})

	t.Run("LimitZeroError", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$limit", int64(0))),
		})
		require.Error(t, err)

		var cmdErr mongo.CommandError
		require.ErrorAs(t, err, &cmdErr)
		assert.EqualValues(t, 15958, cmdErr.Code)
	})

	t.Run("LimitNegativeError", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$limit", int64(-1))),
		})
		// MongoDB rejects negative $limit values.
		require.Error(t, err)
	})
}

// ─── $skip ────────────────────────────────────────────────────────────────────

// TestAggStage_skip tests $skip pipeline stage. (DongoFull)
func TestAggStage_skip(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a")),
		d(e("_id", "b")),
		d(e("_id", "c")),
		d(e("_id", "d")),
	)

	t.Run("SkipOne", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("_id", int32(1))))),
			d(e("$skip", int64(1))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 3)
		assert.Equal(t, "b", results[0].Map()["_id"])
	})

	t.Run("SkipAll", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$skip", int64(100))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 0)
	})

	t.Run("SkipZero", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$skip", int64(0))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 4)
	})

	t.Run("SkipNegativeError", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$skip", int64(-1))),
		})
		// MongoDB 8.0 rejects negative $skip values.
		require.Error(t, err)
	})
}

// ─── $project ─────────────────────────────────────────────────────────────────

// TestAggStage_project tests $project pipeline stage. (DongoFull)
func TestAggStage_project(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("x", int32(1)), e("y", int32(2)), e("z", int32(3))),
		d(e("_id", "b"), e("x", int32(4)), e("y", int32(5)), e("z", int32(6))),
	)

	t.Run("IncludeFields", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("_id", int32(1))))),
			d(e("$project", d(e("x", int32(1)), e("y", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)

		m := results[0].Map()
		assert.Contains(t, m, "_id")
		assert.Contains(t, m, "x")
		assert.Contains(t, m, "y")
		assert.NotContains(t, m, "z")
	})

	t.Run("ExcludeFields", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("_id", int32(1))))),
			d(e("$project", d(e("z", int32(0))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)

		m := results[0].Map()
		assert.Contains(t, m, "_id")
		assert.Contains(t, m, "x")
		assert.Contains(t, m, "y")
		assert.NotContains(t, m, "z")
	})

	t.Run("ExcludeID", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$project", d(e("_id", int32(0)), e("x", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)

		for _, r := range results {
			assert.NotContains(t, r.Map(), "_id")
			assert.Contains(t, r.Map(), "x")
		}
	})

	t.Run("ComputedField", func(t *testing.T) {
		dongoXFail(t, "$add expression operator not yet implemented in $project")

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("_id", int32(1))))),
			d(e("$project", d(
				e("_id", int32(0)),
				e("sum", d(e("$add", bson.A{"$x", "$y"}))),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		assert.EqualValues(t, 3, results[0].Map()["sum"])  // 1+2
		assert.EqualValues(t, 9, results[1].Map()["sum"])  // 4+5
	})
}

// ─── $unwind ──────────────────────────────────────────────────────────────────

// TestAggStage_unwind tests $unwind pipeline stage. (DongoFull)
func TestAggStage_unwind(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("tags", bson.A{"x", "y", "z"})),
		d(e("_id", "b"), e("tags", bson.A{"p", "q"})),
		d(e("_id", "c"), e("tags", bson.A{})),
		d(e("_id", "d")), // missing field
	)

	t.Run("BasicUnwind", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$match", d(e("_id", "a")))),
			d(e("$unwind", "$tags")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 3)
	})

	t.Run("UnwindEmptyArraySkipped", func(t *testing.T) {
		t.Parallel()

		// By default empty arrays produce no output documents.
		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$match", d(e("_id", "c")))),
			d(e("$unwind", "$tags")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 0)
	})

	t.Run("UnwindMissingFieldSkipped", func(t *testing.T) {
		t.Parallel()

		// By default missing fields produce no output documents.
		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$match", d(e("_id", "d")))),
			d(e("$unwind", "$tags")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 0)
	})

	t.Run("PreserveNullAndEmptyArrays", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$unwind", d(
				e("path", "$tags"),
				e("preserveNullAndEmptyArrays", true),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		// a: 3 docs, b: 2 docs, c: 1 (empty array preserved), d: 1 (missing field preserved)
		assert.Len(t, results, 7)
	})

	t.Run("IncludeArrayIndex", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$match", d(e("_id", "a")))),
			d(e("$unwind", d(
				e("path", "$tags"),
				e("includeArrayIndex", "idx"),
			))),
			d(e("$sort", d(e("idx", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 3)
		assert.EqualValues(t, int64(0), results[0].Map()["idx"])
		assert.EqualValues(t, int64(1), results[1].Map()["idx"])
		assert.EqualValues(t, int64(2), results[2].Map()["idx"])
	})
}

// ─── $addFields / $set ────────────────────────────────────────────────────────

// TestAggStage_addFields tests $addFields pipeline stage. (DongoFull)
func TestAggStage_addFields(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("x", int32(10)), e("y", int32(5))),
		d(e("_id", "b"), e("x", int32(20)), e("y", int32(3))),
	)

	t.Run("AddLiteralField", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$addFields", d(e("status", "active")))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		for _, r := range results {
			assert.Equal(t, "active", r.Map()["status"])
		}
	})

	t.Run("AddComputedField", func(t *testing.T) {
		dongoXFail(t, "$add expression operator not yet implemented in $addFields")

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("_id", int32(1))))),
			d(e("$addFields", d(e("total", d(e("$add", bson.A{"$x", "$y"})))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		assert.EqualValues(t, 15, results[0].Map()["total"])
		assert.EqualValues(t, 23, results[1].Map()["total"])
	})

	t.Run("OriginalFieldsPreserved", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$addFields", d(e("extra", int32(99))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		for _, r := range results {
			m := r.Map()
			assert.Contains(t, m, "x")
			assert.Contains(t, m, "y")
			assert.Contains(t, m, "extra")
		}
	})
}

// TestAggStage_set tests $set pipeline stage (alias for $addFields). (DongoFull)
func TestAggStage_set(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
	)

	t.Run("SetNewField", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$set", d(e("doubled", d(e("$multiply", bson.A{"$v", int32(2)})))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.EqualValues(t, 2, results[0].Map()["doubled"])
	})

	t.Run("SetOverwritesField", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$set", d(e("v", int32(42))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.EqualValues(t, 42, results[0].Map()["v"])
	})
}

// ─── $unset ───────────────────────────────────────────────────────────────────

// TestAggStage_unset tests $unset pipeline stage. (DongoFull)
func TestAggStage_unset(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("x", int32(1)), e("y", int32(2)), e("z", int32(3))),
	)

	t.Run("UnsetSingleField", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$unset", "z")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		m := results[0].Map()
		assert.Contains(t, m, "x")
		assert.Contains(t, m, "y")
		assert.NotContains(t, m, "z")
	})

	t.Run("UnsetMultipleFields", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$unset", bson.A{"y", "z"})),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		m := results[0].Map()
		assert.Contains(t, m, "x")
		assert.NotContains(t, m, "y")
		assert.NotContains(t, m, "z")
	})
}

// ─── $count ───────────────────────────────────────────────────────────────────

// TestAggStage_count tests $count pipeline stage. (DongoFull)
func TestAggStage_count(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("active", true)),
		d(e("_id", "b"), e("active", true)),
		d(e("_id", "c"), e("active", false)),
	)

	t.Run("CountAll", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$count", "total")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.EqualValues(t, 3, results[0].Map()["total"])
	})

	t.Run("CountAfterMatch", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$match", d(e("active", true)))),
			d(e("$count", "n")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.EqualValues(t, 2, results[0].Map()["n"])
	})

	t.Run("CountEmptyCollection", func(t *testing.T) {
		t.Parallel()

		emptyColl := env.collection(t)

		ctx := context.Background()
		cursor, err := emptyColl.Aggregate(ctx, bson.A{
			d(e("$count", "n")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		// No documents → no output from $count.
		assert.Len(t, results, 0)
	})
}

// TestAggStage_countErrors tests $count error cases. (DongoFull)
func TestAggStage_countErrors(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	t.Run("EmptyFieldName", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$count", "")),
		})
		require.Error(t, err)
	})

	t.Run("FieldNameWithDot", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$count", "a.b")),
		})
		require.Error(t, err)
	})
}

// ─── $replaceRoot / $replaceWith ──────────────────────────────────────────────

// TestAggStage_replaceRoot tests $replaceRoot pipeline stage. (DongoFull)
func TestAggStage_replaceRoot(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("nested", d(e("x", int32(1)), e("y", int32(2))))),
		d(e("_id", "b"), e("nested", d(e("x", int32(3)), e("y", int32(4))))),
	)

	t.Run("ReplaceWithNestedDoc", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("_id", int32(1))))),
			d(e("$replaceRoot", d(e("newRoot", "$nested")))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		m := results[0].Map()
		assert.EqualValues(t, 1, m["x"])
		assert.EqualValues(t, 2, m["y"])
		assert.NotContains(t, m, "_id")
	})
}

// TestAggStage_replaceWith tests $replaceWith (alias for $replaceRoot). (DongoFull)
func TestAggStage_replaceWith(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("info", d(e("v", int32(42))))),
	)

	t.Run("ReplaceWithNestedDoc", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$replaceWith", "$info")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.EqualValues(t, 42, results[0].Map()["v"])
	})
}

// ─── $sortByCount ─────────────────────────────────────────────────────────────

// TestAggStage_sortByCount tests $sortByCount pipeline stage. (DongoFull)
func TestAggStage_sortByCount(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "1"), e("tag", "go")),
		d(e("_id", "2"), e("tag", "go")),
		d(e("_id", "3"), e("tag", "go")),
		d(e("_id", "4"), e("tag", "rust")),
		d(e("_id", "5"), e("tag", "rust")),
		d(e("_id", "6"), e("tag", "c")),
	)

	t.Run("SortByCountDescending", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sortByCount", "$tag")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 3)

		// Results sorted by count descending.
		assert.Equal(t, "go", results[0].Map()["_id"])
		assert.EqualValues(t, 3, results[0].Map()["count"])
		assert.Equal(t, "rust", results[1].Map()["_id"])
		assert.EqualValues(t, 2, results[1].Map()["count"])
		assert.Equal(t, "c", results[2].Map()["_id"])
		assert.EqualValues(t, 1, results[2].Map()["count"])
	})
}

// ─── $facet ───────────────────────────────────────────────────────────────────

// TestAggStage_facet tests $facet pipeline stage. (DongoFull)
func TestAggStage_facet(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("price", int32(10)), e("cat", "food")),
		d(e("_id", "b"), e("price", int32(20)), e("cat", "food")),
		d(e("_id", "c"), e("price", int32(30)), e("cat", "gear")),
		d(e("_id", "d"), e("price", int32(40)), e("cat", "gear")),
		d(e("_id", "e"), e("price", int32(50)), e("cat", "gear")),
	)

	t.Run("MultipleFacets", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$facet", d(
				e("byCategory", bson.A{
					d(e("$sortByCount", "$cat")),
				}),
				e("totalCount", bson.A{
					d(e("$count", "n")),
				}),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)

		m := results[0].Map()

		byCategory, ok := m["byCategory"].(bson.A)
		require.True(t, ok)
		assert.Len(t, byCategory, 2)

		totalCount, ok := m["totalCount"].(bson.A)
		require.True(t, ok)
		require.Len(t, totalCount, 1)
		assert.EqualValues(t, 5, totalCount[0].(bson.D).Map()["n"])
	})
}

// ─── $bucket ──────────────────────────────────────────────────────────────────

// TestAggStage_bucket tests $bucket pipeline stage. (DongoFull)
func TestAggStage_bucket(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("price", int32(5))),
		d(e("_id", "b"), e("price", int32(15))),
		d(e("_id", "c"), e("price", int32(25))),
		d(e("_id", "d"), e("price", int32(35))),
		d(e("_id", "e"), e("price", int32(45))),
	)

	t.Run("BasicBuckets", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucket", d(
				e("groupBy", "$price"),
				e("boundaries", bson.A{int32(0), int32(20), int32(40), int32(60)}),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 3)

		// Bucket [0,20): price 5, 15 → count 2
		assert.EqualValues(t, 0, results[0].Map()["_id"])
		assert.EqualValues(t, 2, results[0].Map()["count"])
		// Bucket [20,40): price 25, 35 → count 2
		assert.EqualValues(t, 20, results[1].Map()["_id"])
		assert.EqualValues(t, 2, results[1].Map()["count"])
		// Bucket [40,60): price 45 → count 1
		assert.EqualValues(t, 40, results[2].Map()["_id"])
		assert.EqualValues(t, 1, results[2].Map()["count"])
	})

	t.Run("WithDefault", func(t *testing.T) {
		t.Parallel()

		// Insert an out-of-range value.
		env2 := startDongo(t)
		coll2 := env2.collection(t)
		insertDocs(t, coll2,
			d(e("_id", "x"), e("price", int32(5))),
			d(e("_id", "y"), e("price", int32(999))), // beyond boundaries
		)

		ctx := context.Background()
		cursor, err := coll2.Aggregate(ctx, bson.A{
			d(e("$bucket", d(
				e("groupBy", "$price"),
				e("boundaries", bson.A{int32(0), int32(100)}),
				e("default", "other"),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)

		// Find the "other" bucket.
		var foundOther bool
		for _, r := range results {
			if r.Map()["_id"] == "other" {
				assert.EqualValues(t, 1, r.Map()["count"])
				foundOther = true
			}
		}
		assert.True(t, foundOther, "expected 'other' default bucket")
	})

	t.Run("MissingBoundariesError", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucket", d(
				e("groupBy", "$price"),
			))),
		})
		require.Error(t, err)
	})
}

// ─── $bucketAuto ──────────────────────────────────────────────────────────────

// TestAggStage_bucketAuto tests $bucketAuto pipeline stage. (DongoFull)
func TestAggStage_bucketAuto(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(10))),
		d(e("_id", "b"), e("v", int32(20))),
		d(e("_id", "c"), e("v", int32(30))),
		d(e("_id", "d"), e("v", int32(40))),
		d(e("_id", "e"), e("v", int32(50))),
		d(e("_id", "f"), e("v", int32(60))),
	)

	t.Run("TwoBuckets", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucketAuto", d(
				e("groupBy", "$v"),
				e("buckets", int32(2)),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 2)

		// Each bucket should have a count.
		for _, r := range results {
			assert.Contains(t, r.Map(), "count")
			assert.Contains(t, r.Map(), "_id")
		}
	})

	t.Run("ThreeBuckets", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucketAuto", d(
				e("groupBy", "$v"),
				e("buckets", int32(3)),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 3)
	})

	t.Run("MissingBucketsError", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		_, err := coll.Aggregate(ctx, bson.A{
			d(e("$bucketAuto", d(
				e("groupBy", "$v"),
			))),
		})
		require.Error(t, err)
	})
}

// ─── $lookup ──────────────────────────────────────────────────────────────────

// TestAggStage_lookup tests $lookup pipeline stage. (DongoFull)
func TestAggStage_lookup(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	db := env.client.Database("testdb")

	// orders collection
	orders := db.Collection("orders")
	t.Cleanup(func() { _ = orders.Drop(context.Background()) })
	insertDocs(t, orders,
		d(e("_id", "o1"), e("item", "widget"), e("qty", int32(5))),
		d(e("_id", "o2"), e("item", "gadget"), e("qty", int32(2))),
	)

	// inventory collection
	inventory := db.Collection("inventory")
	t.Cleanup(func() { _ = inventory.Drop(context.Background()) })
	insertDocs(t, inventory,
		d(e("_id", "i1"), e("sku", "widget"), e("instock", int32(100))),
		d(e("_id", "i2"), e("sku", "gadget"), e("instock", int32(50))),
		d(e("_id", "i3"), e("sku", "doohickey"), e("instock", int32(0))),
	)

	t.Run("SimpleEqualityJoin", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := orders.Aggregate(ctx, bson.A{
			d(e("$lookup", d(
				e("from", inventory.Name()),
				e("localField", "item"),
				e("foreignField", "sku"),
				e("as", "stockInfo"),
			))),
			d(e("$sort", d(e("_id", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)

		// Each order should have stockInfo array with one match.
		for _, r := range results {
			stockInfo, ok := r.Map()["stockInfo"].(bson.A)
			require.True(t, ok, "stockInfo should be an array")
			assert.Len(t, stockInfo, 1)
		}
	})

	t.Run("NoMatchProducesEmptyArray", func(t *testing.T) {
		t.Parallel()

		// An order with an item that doesn't match any inventory.
		specialOrders := db.Collection("specialorders")
		t.Cleanup(func() { _ = specialOrders.Drop(context.Background()) })
		insertDocs(t, specialOrders,
			d(e("_id", "s1"), e("item", "unknown-item")),
		)

		ctx := context.Background()
		cursor, err := specialOrders.Aggregate(ctx, bson.A{
			d(e("$lookup", d(
				e("from", inventory.Name()),
				e("localField", "item"),
				e("foreignField", "sku"),
				e("as", "stockInfo"),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		stockInfo, ok := results[0].Map()["stockInfo"].(bson.A)
		require.True(t, ok)
		assert.Len(t, stockInfo, 0)
	})
}

// ─── $out ─────────────────────────────────────────────────────────────────────

// TestAggStage_out tests $out pipeline stage. (DongoFull)
func TestAggStage_out(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	db := env.client.Database("testdb")

	source := db.Collection("outsrc")
	t.Cleanup(func() { _ = source.Drop(context.Background()) })
	insertDocs(t, source,
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
		d(e("_id", "c"), e("v", int32(3))),
	)

	t.Run("OutToNewCollection", func(t *testing.T) {
		t.Parallel()

		targetName := "outtgt"
		target := db.Collection(targetName)
		t.Cleanup(func() { _ = target.Drop(context.Background()) })

		ctx := context.Background()
		cursor, err := source.Aggregate(ctx, bson.A{
			d(e("$match", d(e("v", d(e("$gte", int32(2))))))),
			d(e("$out", targetName)),
		})
		require.NoError(t, err)

		// $out returns no documents to the cursor.
		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 0)

		// Verify target collection contains the output.
		count, err := target.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 2, count)
	})
}

// ─── $merge ───────────────────────────────────────────────────────────────────

// TestAggStage_merge tests $merge pipeline stage. (DongoFull)
func TestAggStage_merge(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	db := env.client.Database("testdb")

	source := db.Collection("mergesrc")
	t.Cleanup(func() { _ = source.Drop(context.Background()) })
	insertDocs(t, source,
		d(e("_id", "a"), e("v", int32(10))),
		d(e("_id", "b"), e("v", int32(20))),
	)

	t.Run("MergeIntoNewCollection", func(t *testing.T) {
		t.Parallel()

		targetName := "mergetgt"
		target := db.Collection(targetName)
		t.Cleanup(func() { _ = target.Drop(context.Background()) })

		ctx := context.Background()
		cursor, err := source.Aggregate(ctx, bson.A{
			d(e("$merge", d(
				e("into", targetName),
				e("whenMatched", "replace"),
				e("whenNotMatched", "insert"),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		// $merge returns no documents to the cursor.
		assert.Len(t, results, 0)

		// Verify target has the merged documents.
		count, err := target.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 2, count)
	})

	t.Run("MergeStringForm", func(t *testing.T) {
		t.Parallel()

		targetName := "mergestr"
		target := db.Collection(targetName)
		t.Cleanup(func() { _ = target.Drop(context.Background()) })

		ctx := context.Background()
		cursor, err := source.Aggregate(ctx, bson.A{
			d(e("$merge", targetName)),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 0)

		count, err := target.CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		assert.EqualValues(t, 2, count)
	})
}

// ─── $graphLookup ─────────────────────────────────────────────────────────────

// TestAggStage_graphLookup tests $graphLookup pipeline stage. (DongoFull)
func TestAggStage_graphLookup(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	db := env.client.Database("testdb")

	// Build an org hierarchy: CEO → VP → Manager → Employee.
	employees := db.Collection("employees")
	t.Cleanup(func() { _ = employees.Drop(context.Background()) })
	insertDocs(t, employees,
		d(e("_id", "ceo"), e("reportsTo", nil)),
		d(e("_id", "vp"), e("reportsTo", "ceo")),
		d(e("_id", "mgr"), e("reportsTo", "vp")),
		d(e("_id", "emp"), e("reportsTo", "mgr")),
	)

	t.Run("TraverseHierarchyFromLeaf", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := employees.Aggregate(ctx, bson.A{
			d(e("$match", d(e("_id", "emp")))),
			d(e("$graphLookup", d(
				e("from", employees.Name()),
				e("startWith", "$reportsTo"),
				e("connectFromField", "reportsTo"),
				e("connectToField", "_id"),
				e("as", "chain"),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)

		chain, ok := results[0].Map()["chain"].(bson.A)
		require.True(t, ok)
		// emp → mgr → vp → ceo = 3 ancestors
		assert.Len(t, chain, 3)
	})

	t.Run("MaxDepthLimitsTraversal", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := employees.Aggregate(ctx, bson.A{
			d(e("$match", d(e("_id", "emp")))),
			d(e("$graphLookup", d(
				e("from", employees.Name()),
				e("startWith", "$reportsTo"),
				e("connectFromField", "reportsTo"),
				e("connectToField", "_id"),
				e("as", "chain"),
				e("maxDepth", int64(1)),
			))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)

		chain, ok := results[0].Map()["chain"].(bson.A)
		require.True(t, ok)
		// With maxDepth=1: emp→mgr (depth 0), mgr→vp (depth 1) → 2 ancestors
		assert.Len(t, chain, 2)
	})
}

// ─── Multi-stage pipelines ────────────────────────────────────────────────────

// TestAggPipeline_multiStage tests common multi-stage pipeline combinations. (DongoFull)
func TestAggPipeline_multiStage(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("dept", "eng"), e("salary", int32(100))),
		d(e("_id", "b"), e("dept", "eng"), e("salary", int32(200))),
		d(e("_id", "c"), e("dept", "mkt"), e("salary", int32(150))),
		d(e("_id", "d"), e("dept", "mkt"), e("salary", int32(120))),
		d(e("_id", "e"), e("dept", "eng"), e("salary", int32(90))),
	)

	t.Run("MatchGroupSort", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$match", d(e("dept", "eng")))),
			d(e("$group", d(
				e("_id", "$dept"),
				e("avgSalary", d(e("$avg", "$salary"))),
				e("headcount", d(e("$sum", int32(1)))),
			))),
			d(e("$sort", d(e("_id", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		m := results[0].Map()
		assert.Equal(t, "eng", m["_id"])
		assert.EqualValues(t, 3, m["headcount"])
		// avg of 100, 200, 90 = 130
		assert.InDelta(t, 130.0, m["avgSalary"], 0.01)
	})

	t.Run("SortLimitSkip", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$sort", d(e("salary", int32(-1))))),
			d(e("$skip", int64(1))),
			d(e("$limit", int64(2))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		assert.Len(t, results, 2)
	})

	t.Run("AddFieldsThenGroup", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$addFields", d(e("bonus", d(e("$multiply", bson.A{"$salary", 0.1})))))),
			d(e("$group", d(
				e("_id", "$dept"),
				e("totalBonus", d(e("$sum", "$bonus"))),
			))),
			d(e("$sort", d(e("_id", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 2)
		m0 := results[0].Map()
		assert.Equal(t, "eng", m0["_id"])
		// eng bonuses: 10+20+9 = 39
		assert.InDelta(t, 39.0, m0["totalBonus"], 0.01)
	})

	t.Run("UnwindThenGroup", func(t *testing.T) {
		t.Parallel()

		tagColl := env.collection(t)
		insertDocs(t, tagColl,
			d(e("_id", "p1"), e("tags", bson.A{"go", "db"})),
			d(e("_id", "p2"), e("tags", bson.A{"go", "api"})),
			d(e("_id", "p3"), e("tags", bson.A{"db", "api"})),
		)

		ctx := context.Background()
		cursor, err := tagColl.Aggregate(ctx, bson.A{
			d(e("$unwind", "$tags")),
			d(e("$sortByCount", "$tags")),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 3) // go:2, db:2, api:2

		// All tags appear twice.
		for _, r := range results {
			assert.EqualValues(t, 2, r.Map()["count"])
		}
	})

	t.Run("ProjectThenSort", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$project", d(e("dept", int32(1)), e("salary", int32(1))))),
			d(e("$sort", d(e("salary", int32(1))))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 5)

		// Verify sorted ascending.
		prev := int32(-1)
		for _, r := range results {
			cur, ok := r.Map()["salary"].(int32)
			require.True(t, ok)
			assert.GreaterOrEqual(t, cur, prev)
			prev = cur
		}
	})
}

// ─── Unsupported stages ───────────────────────────────────────────────────────

// TestAggStage_unsupportedErrors tests that unsupported stages return ErrNotImplemented. (DongoFull)
func TestAggStage_unsupportedErrors(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	unsupported := []string{
		"$changeStream",
		"$densify",
		"$fill",
		"$geoNear",
		"$indexStats",
		"$redact",
		"$search",
		"$unionWith",
	}

	for _, stage := range unsupported {
		stage := stage
		t.Run(stage, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			_, err := coll.Aggregate(ctx, bson.A{
				d(e(stage, d())),
			})
			require.Error(t, err, "expected error for unsupported stage %s", stage)

			var cmdErr mongo.CommandError
			require.ErrorAs(t, err, &cmdErr)
			// ErrNotImplemented (238) or ErrCommandNotFound (59)
			assert.True(t, cmdErr.Code == 238 || cmdErr.Code == 59,
				"expected code 238 or 59 for %s, got %d", stage, cmdErr.Code)
		})
	}
}

// TestAggStage_unknownStageError tests that a completely unknown stage returns an error. (DongoFull)
func TestAggStage_unknownStageError(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()
	_, err := coll.Aggregate(ctx, bson.A{
		d(e("$definitelyNotAStage", d())),
	})
	// Unknown stage must produce a CommandError.
	require.Error(t, err)
	var cmdErr mongo.CommandError
	require.ErrorAs(t, err, &cmdErr)
}

// TestAggStage_emptyPipeline tests that an empty pipeline returns all documents. (DongoFull)
func TestAggStage_emptyPipeline(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a")),
		d(e("_id", "b")),
		d(e("_id", "c")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 3)
}

// TestAggStage_invalidPipelineSpec tests error when pipeline stage spec has wrong field count. (DongoFull)
func TestAggStage_invalidPipelineSpec(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()
	_, err := coll.Aggregate(ctx, bson.A{
		d(e("$match", d()), e("$sort", d())), // two fields in one stage doc
	})
	require.Error(t, err)

	var cmdErr mongo.CommandError
	require.ErrorAs(t, err, &cmdErr)
	// ErrStageInvalid
	assert.EqualValues(t, 40323, cmdErr.Code)
}

// ─── $collStats ───────────────────────────────────────────────────────────────

// TestAggStage_collStats tests $collStats pipeline stage. (DongoFull)
func TestAggStage_collStats(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a")),
		d(e("_id", "b")),
	)

	t.Run("StorageStats", func(t *testing.T) {
		dongoXFail(t, "$collStats storageStats sub-spec causes server panic/EOF — not yet stable")

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$collStats", d(e("storageStats", d())))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.Contains(t, results[0].Map(), "storageStats")
	})

	t.Run("Count", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		cursor, err := coll.Aggregate(ctx, bson.A{
			d(e("$collStats", d(e("count", d())))),
		})
		require.NoError(t, err)

		var results []bson.D
		require.NoError(t, cursor.All(ctx, &results))
		require.Len(t, results, 1)
		assert.Contains(t, results[0].Map(), "count")
	})
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// aggIDs runs an aggregation pipeline and returns sorted _id values.
func aggIDs(tb testing.TB, coll *mongo.Collection, pipeline bson.A) []interface{} {
	tb.Helper()

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(tb, err)

	var results []bson.D
	require.NoError(tb, cursor.All(ctx, &results))

	ids := make([]interface{}, len(results))
	for i, r := range results {
		ids[i] = r.Map()["_id"]
	}

	sort.Slice(ids, func(i, j int) bool {
		return fmt.Sprintf("%v", ids[i]) < fmt.Sprintf("%v", ids[j])
	})
	return ids
}
