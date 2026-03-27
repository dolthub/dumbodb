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
)

// TestAggWindow_documentNumber tests the $documentNumber window function.
func TestAggWindow_documentNumber(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("score", int32(10))),
		d(e("_id", "b"), e("score", int32(20))),
		d(e("_id", "c"), e("score", int32(30))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("score", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("score", int32(1)))),
			e("output", d(
				e("pos", d(e("$documentNumber", d()))),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// documentNumber should be 1-based position in partition.
	for i, r := range results {
		var pos interface{}
		for _, kv := range r {
			if kv.Key == "pos" {
				pos = kv.Value
			}
		}
		assert.Equal(t, int64(i+1), pos, "expected documentNumber %d for document %d", i+1, i)
	}
}

// TestAggWindow_rank tests the $rank window function.
func TestAggWindow_rank(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("score", int32(10))),
		d(e("_id", "b"), e("score", int32(20))),
		d(e("_id", "c"), e("score", int32(20))), // tie with b
		d(e("_id", "dd"), e("score", int32(30))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("score", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("score", int32(1)))),
			e("output", d(
				e("rnk", d(e("$rank", d()))),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 4)

	// Collect ranks by score.
	rankByScore := map[int32]int64{}
	for _, r := range results {
		var score int32
		var rnk int64
		for _, kv := range r {
			switch kv.Key {
			case "score":
				score = kv.Value.(int32)
			case "rnk":
				rnk = kv.Value.(int64)
			}
		}
		rankByScore[score] = rnk
	}

	assert.Equal(t, int64(1), rankByScore[10])
	// score 20 appears twice; both get rank 2.
	assert.Equal(t, int64(2), rankByScore[20])
	// score 30 gets rank 4 (gap because of tie at 2).
	assert.Equal(t, int64(4), rankByScore[30])
}

// TestAggWindow_denseRank tests the $denseRank window function.
func TestAggWindow_denseRank(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("score", int32(10))),
		d(e("_id", "b"), e("score", int32(20))),
		d(e("_id", "c"), e("score", int32(20))), // tie with b
		d(e("_id", "dd"), e("score", int32(30))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("score", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("score", int32(1)))),
			e("output", d(
				e("dr", d(e("$denseRank", d()))),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 4)

	rankByScore := map[int32]int64{}
	for _, r := range results {
		var score int32
		var dr int64
		for _, kv := range r {
			switch kv.Key {
			case "score":
				score = kv.Value.(int32)
			case "dr":
				dr = kv.Value.(int64)
			}
		}
		rankByScore[score] = dr
	}

	assert.Equal(t, int64(1), rankByScore[10])
	assert.Equal(t, int64(2), rankByScore[20])
	// dense rank: no gaps, so score 30 gets dense rank 3.
	assert.Equal(t, int64(3), rankByScore[30])
}

// TestAggWindow_sumUnbounded tests $sum with unbounded window.
func TestAggWindow_sumUnbounded(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
		d(e("_id", "c"), e("v", int32(3))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("total", d(
					e("$sum", "$v"),
					e("window", d(
						e("documents", bson.A{"unbounded", "unbounded"}),
					)),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// All documents should have total = 1+2+3 = 6.
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "total" {
				assert.EqualValues(t, int32(6), kv.Value)
			}
		}
	}
}

// TestAggWindow_sumCumulative tests $sum with cumulative (unbounded to current) window.
func TestAggWindow_sumCumulative(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
		d(e("_id", "c"), e("v", int32(3))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("cumSum", d(
					e("$sum", "$v"),
					e("window", d(
						e("documents", bson.A{"unbounded", "current"}),
					)),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// Expected cumulative sums: 1, 3, 6.
	expected := []interface{}{int32(1), int32(3), int32(6)}
	for i, r := range results {
		for _, kv := range r {
			if kv.Key == "cumSum" {
				assert.EqualValues(t, expected[i], kv.Value, "doc %d", i)
			}
		}
	}
}

// TestAggWindow_avgUnbounded tests $avg with unbounded window.
func TestAggWindow_avgUnbounded(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(10))),
		d(e("_id", "b"), e("v", int32(20))),
		d(e("_id", "c"), e("v", int32(30))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("avg", d(
					e("$avg", "$v"),
					e("window", d(
						e("documents", bson.A{"unbounded", "unbounded"}),
					)),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// avg(10, 20, 30) = 20.0
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "avg" {
				assert.InDelta(t, 20.0, kv.Value, 0.001)
			}
		}
	}
}

// TestAggWindow_minMax tests $min and $max with unbounded window.
func TestAggWindow_minMax(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(10))),
		d(e("_id", "b"), e("v", int32(20))),
		d(e("_id", "c"), e("v", int32(30))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("lo", d(
					e("$min", "$v"),
					e("window", d(
						e("documents", bson.A{"unbounded", "unbounded"}),
					)),
				)),
				e("hi", d(
					e("$max", "$v"),
					e("window", d(
						e("documents", bson.A{"unbounded", "unbounded"}),
					)),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	for _, r := range results {
		for _, kv := range r {
			switch kv.Key {
			case "lo":
				assert.EqualValues(t, int32(10), kv.Value)
			case "hi":
				assert.EqualValues(t, int32(30), kv.Value)
			}
		}
	}
}

// TestAggWindow_partitionBy tests that partitionBy creates separate partitions.
func TestAggWindow_partitionBy(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("dept", "eng"), e("salary", int32(100))),
		d(e("_id", "b"), e("dept", "eng"), e("salary", int32(200))),
		d(e("_id", "c"), e("dept", "sales"), e("salary", int32(150))),
		d(e("_id", "dd"), e("dept", "sales"), e("salary", int32(250))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("dept", int32(1)), e("salary", int32(1))))),
		d(e("$setWindowFields", d(
			e("partitionBy", "$dept"),
			e("sortBy", d(e("salary", int32(1)))),
			e("output", d(
				e("deptTotal", d(
					e("$sum", "$salary"),
					e("window", d(
						e("documents", bson.A{"unbounded", "unbounded"}),
					)),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 4)

	// eng partition total = 100+200=300, sales partition total = 150+250=400.
	for _, r := range results {
		var dept string
		var deptTotal interface{}
		for _, kv := range r {
			switch kv.Key {
			case "dept":
				dept = kv.Value.(string)
			case "deptTotal":
				deptTotal = kv.Value
			}
		}

		switch dept {
		case "eng":
			assert.EqualValues(t, int32(300), deptTotal)
		case "sales":
			assert.EqualValues(t, int32(400), deptTotal)
		}
	}
}

// TestAggWindow_noPartitionSingleGroup tests $setWindowFields without partitionBy (one group).
func TestAggWindow_noPartitionSingleGroup(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "x"), e("v", int32(5))),
		d(e("_id", "y"), e("v", int32(10))),
		d(e("_id", "z"), e("v", int32(15))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("grandTotal", d(e("$sum", "$v"))),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// No window spec = default (entire partition), total = 5+10+15 = 30.
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "grandTotal" {
				assert.EqualValues(t, int32(30), kv.Value)
			}
		}
	}
}
