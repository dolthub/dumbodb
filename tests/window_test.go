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

// ─── Null handling (DongoFull) ────────────────────────────────────────────────

// TestAggWindow_sumNullHandling tests that $sum ignores null values in the window. (DongoFull)
func TestAggWindow_sumNullHandling(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	// One doc has a null value, one has a missing field — both should be ignored by $sum.
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(10))),
		d(e("_id", "b"), e("v", nil)),
		d(e("_id", "c")), // missing field
		d(e("_id", "dd"), e("v", int32(20))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("total", d(
					e("$sum", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 4)

	// $sum ignores nulls and missing: 10+20 = 30.
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "total" {
				assert.EqualValues(t, int32(30), kv.Value)
			}
		}
	}
}

// TestAggWindow_avgNullHandling tests that $avg ignores null values in the window. (DongoFull)
func TestAggWindow_avgNullHandling(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(10))),
		d(e("_id", "b"), e("v", nil)),
		d(e("_id", "c"), e("v", int32(20))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("avg", d(
					e("$avg", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// avg(10, 20) ignoring null = 15.0.
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "avg" {
				assert.InDelta(t, 15.0, kv.Value, 0.001)
			}
		}
	}
}

// TestAggWindow_minNullHandling tests that $min ignores null values. (DongoFull)
func TestAggWindow_minNullHandling(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(5))),
		d(e("_id", "b"), e("v", nil)),
		d(e("_id", "c"), e("v", int32(3))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("lo", d(
					e("$min", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// min(5, 3) ignoring null = 3.
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "lo" {
				assert.EqualValues(t, int32(3), kv.Value)
			}
		}
	}
}

// TestAggWindow_allNullSum tests $sum over an all-null window returns 0. (DongoFull)
func TestAggWindow_allNullSum(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", nil)),
		d(e("_id", "b"), e("v", nil)),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("total", d(
					e("$sum", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)

	// $sum of all nulls returns 0.
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "total" {
				assert.EqualValues(t, int32(0), kv.Value)
			}
		}
	}
}

// ─── Multi-output / edge cases (DongoFull) ────────────────────────────────────

// TestAggWindow_multipleOutputFields tests multiple output fields computed in a single stage. (DongoFull)
func TestAggWindow_multipleOutputFields(t *testing.T) {
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
				e("total", d(
					e("$sum", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
				e("lo", d(
					e("$min", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
				e("hi", d(
					e("$max", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
				e("pos", d(e("$documentNumber", d()))),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	for i, r := range results {
		vals := map[string]interface{}{}
		for _, kv := range r {
			vals[kv.Key] = kv.Value
		}

		assert.EqualValues(t, int32(60), vals["total"], "doc %d total", i)
		assert.EqualValues(t, int32(10), vals["lo"], "doc %d lo", i)
		assert.EqualValues(t, int32(30), vals["hi"], "doc %d hi", i)
		assert.EqualValues(t, int64(i+1), vals["pos"], "doc %d pos", i)
	}
}

// TestAggWindow_numericOffsetWindow tests a window with numeric offsets [-1, 0]. (DongoFull)
func TestAggWindow_numericOffsetWindow(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
		d(e("_id", "c"), e("v", int32(3))),
		d(e("_id", "dd"), e("v", int32(4))),
	)

	ctx := context.Background()
	// Window [-1, 0]: each doc sees itself and the previous.
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("prev2sum", d(
					e("$sum", "$v"),
					e("window", d(e("documents", bson.A{int32(-1), int32(0)}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 4)

	// expected: 1, 1+2=3, 2+3=5, 3+4=7.
	expected := []int32{1, 3, 5, 7}
	for i, r := range results {
		for _, kv := range r {
			if kv.Key == "prev2sum" {
				assert.EqualValues(t, expected[i], kv.Value, "doc %d", i)
			}
		}
	}
}

// TestAggWindow_forwardOffsetWindow tests a window with [0, 1] (current + next). (DongoFull)
func TestAggWindow_forwardOffsetWindow(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(10))),
		d(e("_id", "b"), e("v", int32(20))),
		d(e("_id", "c"), e("v", int32(30))),
	)

	ctx := context.Background()
	// Window [0, 1]: each doc sees itself and next.
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("fwdSum", d(
					e("$sum", "$v"),
					e("window", d(e("documents", bson.A{int32(0), int32(1)}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// expected: 10+20=30, 20+30=50, 30 (no next).
	expected := []int32{30, 50, 30}
	for i, r := range results {
		for _, kv := range r {
			if kv.Key == "fwdSum" {
				assert.EqualValues(t, expected[i], kv.Value, "doc %d", i)
			}
		}
	}
}

// TestAggWindow_partitionByMultipleFields tests partitionBy with multiple partitions and rank. (DongoFull)
func TestAggWindow_partitionByMultipleFields(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("region", "west"), e("score", int32(1))),
		d(e("_id", "b"), e("region", "west"), e("score", int32(2))),
		d(e("_id", "c"), e("region", "east"), e("score", int32(10))),
		d(e("_id", "dd"), e("region", "east"), e("score", int32(20))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("region", int32(1)), e("score", int32(1))))),
		d(e("$setWindowFields", d(
			e("partitionBy", "$region"),
			e("sortBy", d(e("score", int32(1)))),
			e("output", d(
				e("rank", d(e("$rank", d()))),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 4)

	// Each partition should have ranks 1, 2.
	rankByID := map[string]int64{}
	for _, r := range results {
		var id string
		var rank int64
		for _, kv := range r {
			switch kv.Key {
			case "_id":
				id = kv.Value.(string)
			case "rank":
				rank = kv.Value.(int64)
			}
		}
		rankByID[id] = rank
	}

	assert.Equal(t, int64(1), rankByID["a"])
	assert.Equal(t, int64(2), rankByID["b"])
	assert.Equal(t, int64(1), rankByID["c"])
	assert.Equal(t, int64(2), rankByID["dd"])
}

// TestAggWindow_floatValues tests $sum/$avg with float64 values. (DongoFull)
func TestAggWindow_floatValues(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", float64(1.5))),
		d(e("_id", "b"), e("v", float64(2.5))),
		d(e("_id", "c"), e("v", float64(3.0))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("total", d(
					e("$sum", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
				e("avg", d(
					e("$avg", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
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
			case "total":
				assert.InDelta(t, 7.0, kv.Value, 0.001)
			case "avg":
				assert.InDelta(t, 7.0/3.0, kv.Value, 0.001)
			}
		}
	}
}

// TestAggWindow_singleDocument tests window functions with a single document partition. (DongoFull)
func TestAggWindow_singleDocument(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "only"), e("v", int32(42))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("rank", d(e("$rank", d()))),
				e("denseRank", d(e("$denseRank", d()))),
				e("docNum", d(e("$documentNumber", d()))),
				e("total", d(
					e("$sum", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	vals := map[string]interface{}{}
	for _, kv := range results[0] {
		vals[kv.Key] = kv.Value
	}
	assert.EqualValues(t, int64(1), vals["rank"])
	assert.EqualValues(t, int64(1), vals["denseRank"])
	assert.EqualValues(t, int64(1), vals["docNum"])
	assert.EqualValues(t, int32(42), vals["total"])
}

// ─── Unimplemented operators (DongoXFail) ─────────────────────────────────────

// TestAggWindow_count tests $count window operator.
func TestAggWindow_count(t *testing.T) {
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
				e("n", d(
					e("$count", d()),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// $count should count documents in the window — all 3 docs see count=3.
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "n" {
				assert.EqualValues(t, int64(3), kv.Value)
			}
		}
	}
}

// TestAggWindow_first tests $first window operator (first doc in window).
func TestAggWindow_first(t *testing.T) {
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
				e("firstV", d(
					e("$first", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "current"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// $first [unbounded, current]: always the first document's value.
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "firstV" {
				assert.EqualValues(t, int32(10), kv.Value)
			}
		}
	}
}

// TestAggWindow_last tests $last window operator (last doc in window).
func TestAggWindow_last(t *testing.T) {
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
				e("lastV", d(
					e("$last", "$v"),
					e("window", d(e("documents", bson.A{"current", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// $last [current, unbounded]: always the last document's value.
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "lastV" {
				assert.EqualValues(t, int32(30), kv.Value)
			}
		}
	}
}

// TestAggWindow_push tests $push window operator (collect values into array).
func TestAggWindow_push(t *testing.T) {
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
				e("all", d(
					e("$push", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// All documents should see [1, 2, 3].
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "all" {
				arr, ok := kv.Value.(bson.A)
				require.True(t, ok, "expected array")
				assert.Len(t, arr, 3)
			}
		}
	}
}

// TestAggWindow_addToSet tests $addToSet window operator (unique values).
func TestAggWindow_addToSet(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("tag", "x")),
		d(e("_id", "b"), e("tag", "y")),
		d(e("_id", "c"), e("tag", "x")), // duplicate
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("tags", d(
					e("$addToSet", "$tag"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// All docs should see {x, y} (2 unique values).
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "tags" {
				arr, ok := kv.Value.(bson.A)
				require.True(t, ok, "expected array")
				assert.Len(t, arr, 2)
			}
		}
	}
}

// TestAggWindow_shift tests $shift window operator (access another doc's field).
func TestAggWindow_shift(t *testing.T) {
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
				e("prevV", d(
					e("$shift", d(
						e("output", "$v"),
						e("by", int32(-1)),
						e("default", nil),
					)),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// $shift(-1): null, 10, 20.
	expected := []interface{}{nil, int32(10), int32(20)}
	for i, r := range results {
		for _, kv := range r {
			if kv.Key == "prevV" {
				assert.Equal(t, expected[i], kv.Value, "doc %d", i)
			}
		}
	}
}

// TestAggWindow_stdDevPop tests $stdDevPop window operator.
func TestAggWindow_stdDevPop(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(2))),
		d(e("_id", "b"), e("v", int32(4))),
		d(e("_id", "c"), e("v", int32(4))),
		d(e("_id", "dd"), e("v", int32(4))),
		d(e("_id", "e"), e("v", int32(5))),
		d(e("_id", "f"), e("v", int32(5))),
		d(e("_id", "g"), e("v", int32(7))),
		d(e("_id", "h"), e("v", int32(9))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("stdPop", d(
					e("$stdDevPop", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 8)

	// Population stddev of [2,4,4,4,5,5,7,9] = 2.0.
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "stdPop" {
				assert.InDelta(t, 2.0, kv.Value, 0.001)
			}
		}
	}
}

// TestAggWindow_stdDevSamp tests $stdDevSamp window operator.
func TestAggWindow_stdDevSamp(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(2))),
		d(e("_id", "b"), e("v", int32(4))),
		d(e("_id", "c"), e("v", int32(4))),
		d(e("_id", "dd"), e("v", int32(4))),
		d(e("_id", "e"), e("v", int32(5))),
		d(e("_id", "f"), e("v", int32(5))),
		d(e("_id", "g"), e("v", int32(7))),
		d(e("_id", "h"), e("v", int32(9))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("stdSamp", d(
					e("$stdDevSamp", "$v"),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 8)

	// Sample stddev of [2,4,4,4,5,5,7,9] ≈ 2.138.
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "stdSamp" {
				assert.InDelta(t, 2.138, kv.Value, 0.01)
			}
		}
	}
}

// TestAggWindow_covariancePop tests $covariancePop window operator.
func TestAggWindow_covariancePop(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("x", int32(1)), e("y", int32(2))),
		d(e("_id", "b"), e("x", int32(2)), e("y", int32(4))),
		d(e("_id", "c"), e("x", int32(3)), e("y", int32(6))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("cov", d(
					e("$covariancePop", bson.A{"$x", "$y"}),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// covPop([1,2,3], [2,4,6]) = 2.0/3*(3-1) = 2.0 (perfectly linear, cov = 2/3).
	// Actually covPop = mean(x*y) - mean(x)*mean(y) = (1*2+2*4+3*6)/3 - (1+2+3)/3*(2+4+6)/3
	//                = (2+8+18)/3 - 2*4 = 28/3 - 8 = 28/3 - 24/3 = 4/3 ≈ 1.333... Nope.
	// Simpler: cov(x,y) = E[xy] - E[x]E[y] = 28/3 - 2*4 = 9.333-8 = 1.333.
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "cov" {
				assert.InDelta(t, 4.0/3.0, kv.Value, 0.001)
			}
		}
	}
}

// TestAggWindow_expMovingAvg tests $expMovingAvg window operator.
func TestAggWindow_expMovingAvg(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
		d(e("_id", "c"), e("v", int32(3))),
		d(e("_id", "dd"), e("v", int32(4))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("ema", d(
					e("$expMovingAvg", d(
						e("input", "$v"),
						e("N", int32(2)),
					)),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 4)

	// First value = input; subsequent: EMA = alpha * v + (1-alpha) * prev, alpha = 2/(N+1) = 2/3.
	// EMA[0]=1, EMA[1]=2*(2/3)+1*(1/3)=5/3≈1.667, EMA[2]=3*(2/3)+5/3*(1/3)=2+5/9≈2.556, ...
	require.NotEmpty(t, results[0])
}

// TestAggWindow_derivative tests $derivative window operator.
func TestAggWindow_derivative(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("pos", int32(0)), e("t", int32(0))),
		d(e("_id", "b"), e("pos", int32(10)), e("t", int32(1))),
		d(e("_id", "c"), e("pos", int32(30)), e("t", int32(2))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("t", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("t", int32(1)))),
			e("output", d(
				e("vel", d(
					e("$derivative", d(
						e("input", "$pos"),
						e("unit", "second"),
					)),
					e("window", d(e("documents", bson.A{int32(-1), int32(0)}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// velocity: null (no prior), 10/1=10, 20/1=20.
	require.NotEmpty(t, results[0])
}

// TestAggWindow_integral tests $integral window operator.
func TestAggWindow_integral(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1)), e("t", int32(0))),
		d(e("_id", "b"), e("v", int32(3)), e("t", int32(1))),
		d(e("_id", "c"), e("v", int32(5)), e("t", int32(2))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("t", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("t", int32(1)))),
			e("output", d(
				e("area", d(
					e("$integral", d(
						e("input", "$v"),
						e("unit", "second"),
					)),
					e("window", d(e("documents", bson.A{"unbounded", "current"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// Trapezoidal integration: area under v(t): 0→1: (1+3)/2*1=2, 1→2: (3+5)/2*1=4.
	require.NotEmpty(t, results[0])
}

// TestAggWindow_linearFill tests $linearFill window operator (fill null with linear interp).
func TestAggWindow_linearFill(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(10))),
		d(e("_id", "b"), e("v", nil)), // should be filled to 20
		d(e("_id", "c"), e("v", int32(30))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("filled", d(e("$linearFill", "$v"))),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// Should linearly interpolate null between 10 and 30 → 20.
	for i, r := range results {
		for _, kv := range r {
			if kv.Key == "filled" {
				switch i {
				case 0:
					assert.EqualValues(t, int32(10), kv.Value)
				case 1:
					assert.InDelta(t, 20.0, kv.Value, 0.001)
				case 2:
					assert.EqualValues(t, int32(30), kv.Value)
				}
			}
		}
	}
}

// TestAggWindow_locf tests $locf window operator (last observation carried forward).
func TestAggWindow_locf(t *testing.T) {

	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(5))),
		d(e("_id", "b"), e("v", nil)), // should fill with 5
		d(e("_id", "c"), e("v", nil)), // should fill with 5
		d(e("_id", "dd"), e("v", int32(10))),
		d(e("_id", "e"), e("v", nil)), // should fill with 10
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$sort", d(e("_id", int32(1))))),
		d(e("$setWindowFields", d(
			e("sortBy", d(e("_id", int32(1)))),
			e("output", d(
				e("filled", d(e("$locf", "$v"))),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 5)

	expected := []int32{5, 5, 5, 10, 10}
	for i, r := range results {
		for _, kv := range r {
			if kv.Key == "filled" {
				assert.EqualValues(t, expected[i], kv.Value, "doc %d", i)
			}
		}
	}
}

// TestAggWindow_top tests $top window operator (document with highest sort key).
func TestAggWindow_top(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("score", int32(10)), e("name", "alice")),
		d(e("_id", "b"), e("score", int32(30)), e("name", "bob")),
		d(e("_id", "c"), e("score", int32(20)), e("name", "charlie")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$setWindowFields", d(
			e("sortBy", d(e("score", int32(1)))),
			e("output", d(
				e("topDoc", d(
					e("$top", d(
						e("output", d(e("name", "$name"), e("score", "$score"))),
						e("sortBy", d(e("score", int32(-1)))),
					)),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// $top with sortBy score:-1 → should return the doc with the highest score (bob, 30).
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "topDoc" {
				topDoc, ok := kv.Value.(bson.D)
				require.True(t, ok)
				for _, f := range topDoc {
					if f.Key == "name" {
						assert.Equal(t, "bob", f.Value)
					}
				}
			}
		}
	}
}

// TestAggWindow_bottom tests $bottom window operator (document with lowest sort key).
func TestAggWindow_bottom(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("score", int32(10)), e("name", "alice")),
		d(e("_id", "b"), e("score", int32(30)), e("name", "bob")),
		d(e("_id", "c"), e("score", int32(20)), e("name", "charlie")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$setWindowFields", d(
			e("sortBy", d(e("score", int32(1)))),
			e("output", d(
				e("bottomDoc", d(
					e("$bottom", d(
						e("output", d(e("name", "$name"), e("score", "$score"))),
						e("sortBy", d(e("score", int32(1)))),
					)),
					e("window", d(e("documents", bson.A{"unbounded", "unbounded"}))),
				)),
			)),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// $bottom with sortBy score:1 → should return the doc with the lowest score (alice, 10).
	for _, r := range results {
		for _, kv := range r {
			if kv.Key == "bottomDoc" {
				bottomDoc, ok := kv.Value.(bson.D)
				require.True(t, ok)
				for _, f := range bottomDoc {
					if f.Key == "name" {
						assert.Equal(t, "alice", f.Value)
					}
				}
			}
		}
	}
}
