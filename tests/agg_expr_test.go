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
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// aggregate runs a pipeline and decodes the first result into result.
func aggregate(tb testing.TB, env *dumboDBTestEnv, coll string, pipeline bson.A) []bson.D {
	tb.Helper()

	ctx := context.Background()
	c := env.Client.Database("testdb").Collection(coll)
	cursor, err := c.Aggregate(ctx, pipeline)
	require.NoError(tb, err)

	var results []bson.D
	require.NoError(tb, cursor.All(ctx, &results))

	return results
}

// TestExpr_abs tests the $abs aggregation expression operator. (DumboDBFull)
// $abs returns the absolute value of a number.
func TestExpr_abs(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(-5))),
		d(e("_id", "b"), e("v", int32(3))),
		d(e("_id", "c"), e("v", float64(-2.5))),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{{"r", bson.D{{"$abs", "$v"}}}}}},
		bson.D{{"$sort", bson.D{{"_id", int32(1)}}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	assert.Equal(t, int32(5), dmap(results[0])["r"])
	assert.Equal(t, int32(3), dmap(results[1])["r"])
	assert.Equal(t, 2.5, dmap(results[2])["r"])
}

// TestExpr_exp_ln tests the $exp and $ln aggregation expression operators. (DumboDBFull)
// $exp raises e to the given power; $ln computes the natural logarithm.
func TestExpr_exp_ln(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"exp1", bson.D{{"$exp", int32(1)}}},
			{"ln_e", bson.D{{"$ln", math.E}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := dmap(results[0])
	expVal, _ := m["exp1"].(float64)
	lnVal, _ := m["ln_e"].(float64)

	assert.InDelta(t, math.E, expVal, 1e-9, "$exp(1) should be e")
	assert.InDelta(t, 1.0, lnVal, 1e-9, "$ln(e) should be 1")
}

// TestExpr_zip tests the $zip aggregation expression operator. (DumboDBFull)
// $zip transposes an array of arrays.
func TestExpr_zip(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("a", bson.A{int32(1), int32(2), int32(3)}), e("b", bson.A{"one", "two", "three"})),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"r", bson.D{{"$zip", bson.D{{"inputs", bson.A{"$a", "$b"}}}}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	r, _ := dmap(results[0])["r"].(bson.A)
	require.Len(t, r, 3)

	first, _ := r[0].(bson.A)
	assert.Equal(t, int32(1), first[0])
	assert.Equal(t, "one", first[1])
}

// TestExpr_dateAdd tests the $dateAdd aggregation expression operator. (DumboDBFull)
// $dateAdd adds a duration to a date.
func TestExpr_dateAdd(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	insertDocs(t, coll,
		d(e("_id", "a"), e("date", base)),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"r", bson.D{{"$dateAdd", bson.D{
				{"startDate", "$date"},
				{"unit", "day"},
				{"amount", int32(10)},
			}}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	// The Go MongoDB driver decodes dates in bson.D as bson.DateTime (int64 ms).
	// Convert to time.Time for comparison.
	gotRaw := dmap(results[0])["r"]
	var gotTime time.Time
	switch v := gotRaw.(type) {
	case time.Time:
		gotTime = v.UTC()
	case bson.DateTime:
		gotTime = v.Time().UTC()
	case int64:
		gotTime = time.UnixMilli(v).UTC()
	default:
		t.Fatalf("unexpected type for date result: %T", gotRaw)
	}

	expected := base.AddDate(0, 0, 10)
	assert.Equal(t, expected.UTC(), gotTime)
}

// TestExpr_dateDiff tests the $dateDiff aggregation expression operator. (DumboDBFull)
// $dateDiff returns the difference between two dates in the specified unit.
func TestExpr_dateDiff(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	insertDocs(t, coll,
		d(e("_id", "a"), e("start", start), e("end", end)),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"r", bson.D{{"$dateDiff", bson.D{
				{"startDate", "$start"},
				{"endDate", "$end"},
				{"unit", "month"},
			}}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	got, _ := dmap(results[0])["r"].(int64)
	assert.Equal(t, int64(3), got, "$dateDiff of 3 months")
}

// TestExpr_cond_true tests the $cond aggregation expression operator. (DumboDBFull)
// $cond returns one value if the condition is true, another if false.
func TestExpr_cond_true(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("score", int32(80))),
		d(e("_id", "b"), e("score", int32(40))),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"grade", bson.D{{"$cond", bson.A{
				bson.D{{"$gte", bson.A{"$score", int32(60)}}},
				"pass",
				"fail",
			}}}},
		}}},
		bson.D{{"$sort", bson.D{{"grade", int32(1)}}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)

	assert.Equal(t, "fail", dmap(results[0])["grade"])
	assert.Equal(t, "pass", dmap(results[1])["grade"])
}

// TestExpr_convert_int_to_string tests the $convert operator converting int to string. (DumboDBFull)
func TestExpr_convert_int_to_string(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(42))),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"r", bson.D{{"$convert", bson.D{
				{"input", "$v"},
				{"to", "string"},
			}}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "42", dmap(results[0])["r"])
}

// TestExpr_convert_with_onError tests the $convert operator with an onError fallback. (DumboDBFull)
// When the conversion fails, onError value is returned instead.
func TestExpr_convert_with_onError(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", "not-a-number")),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"r", bson.D{{"$convert", bson.D{
				{"input", "$v"},
				{"to", "int"},
				{"onError", int32(-1)},
			}}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(-1), dmap(results[0])["r"])
}

// TestExpr_cmp_operators tests the $cmp comparison expression operator. (DumboDBFull)
// $cmp returns -1, 0, or 1 for less, equal, or greater.
func TestExpr_cmp_operators(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("x", int32(1)), e("y", int32(2))),
		d(e("_id", "b"), e("x", int32(3)), e("y", int32(3))),
		d(e("_id", "c"), e("x", int32(5)), e("y", int32(4))),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"r", bson.D{{"$cmp", bson.A{"$x", "$y"}}}},
		}}},
		bson.D{{"$sort", bson.D{{"_id", int32(1)}}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	assert.Equal(t, int32(-1), dmap(results[0])["r"], "1 < 2 should give -1")
	assert.Equal(t, int32(0), dmap(results[1])["r"], "3 == 3 should give 0")
	assert.Equal(t, int32(1), dmap(results[2])["r"], "5 > 4 should give 1")
}

// TestAccum_mergeObjects tests $mergeObjects as an accumulator in $group. (DumboDBFull)
// When used in $group, $mergeObjects merges documents within each group.
func TestAccum_mergeObjects(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	// Each document has a "props" sub-document; $mergeObjects accumulates them.
	insertDocs(t, coll,
		d(e("_id", "a"), e("cat", "x"), e("props", d(e("color", "red")))),
		d(e("_id", "b"), e("cat", "x"), e("props", d(e("size", "large")))),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$group", bson.D{
			{"_id", "$cat"},
			{"merged", bson.D{{"$mergeObjects", "$props"}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	mergedMap, _ := dmap(results[0])["merged"].(bson.M)
	// Both sub-documents should be merged together.
	assert.Equal(t, "red", mergedMap["color"])
	assert.Equal(t, "large", mergedMap["size"])
}

// TestExpr_trunc tests the $trunc aggregation expression operator. (DumboDBFull)
// $trunc truncates a number to the specified decimal place.
func TestExpr_trunc(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", float64(3.7))),
		d(e("_id", "b"), e("v", float64(-2.9))),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"r", bson.D{{"$trunc", bson.A{"$v", int32(0)}}}},
		}}},
		bson.D{{"$sort", bson.D{{"_id", int32(1)}}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)

	assert.Equal(t, float64(3), dmap(results[0])["r"])
	assert.Equal(t, float64(-2), dmap(results[1])["r"])
}

// TestExpr_objectToArray tests the $objectToArray aggregation expression operator. (DumboDBFull)
// $objectToArray converts a document into an array of {k, v} pairs.
func TestExpr_objectToArray(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("obj", d(e("x", int32(1)), e("y", int32(2))))),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"r", bson.D{{"$objectToArray", "$obj"}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	arr, _ := dmap(results[0])["r"].(bson.A)
	require.Len(t, arr, 2)

	// First element should be {k: "x", v: 1}
	first, _ := arr[0].(bson.M)
	fm := dmap(first)
	assert.Equal(t, "x", fm["k"])
	assert.Equal(t, int32(1), fm["v"])
}

// TestExpr_literal tests the $literal aggregation expression operator. (DumboDBFull)
// $literal returns a value without evaluating it as an expression.
func TestExpr_literal(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(10))),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			// $literal returns "$v" as a string, not as a field reference.
			{"r", bson.D{{"$literal", "$v"}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	// $literal "$v" should return the string "$v", not the field value 10.
	assert.Equal(t, "$v", dmap(results[0])["r"])
}

// TestExpr_let tests the $let aggregation expression operator. (DumboDBFull)
// $let binds variables and evaluates an expression in that context.
func TestExpr_let(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("price", int32(10)), e("qty", int32(3))),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"total", bson.D{{"$let", bson.D{
				{"vars", bson.D{
					{"p", "$price"},
					{"q", "$qty"},
				}},
				{"in", bson.D{{"$multiply", bson.A{"$$p", "$$q"}}}},
			}}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(30), dmap(results[0])["total"])
}

// TestExpr_expr_in_match tests using $expr in a $match stage. (DumboDBFull)
// $expr allows aggregation expressions in $match.
func TestExpr_expr_in_match(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("budget", int32(200)), e("spent", int32(150))),
		d(e("_id", "b"), e("budget", int32(100)), e("spent", int32(120))),
		d(e("_id", "c"), e("budget", int32(300)), e("spent", int32(250))),
	)

	// Match documents where spent < budget (under budget).
	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$match", bson.D{
			{"$expr", bson.D{{"$lt", bson.A{"$spent", "$budget"}}}},
		}}},
		bson.D{{"$sort", bson.D{{"_id", int32(1)}}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)

	assert.Equal(t, "a", dmap(results[0])["_id"])
	assert.Equal(t, "c", dmap(results[1])["_id"])
}

// TestExpr_project_type_check tests type check operators like $isNumber. (DumboDBFull)
func TestExpr_project_type_check(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(42))),
		d(e("_id", "b"), e("v", "hello")),
		d(e("_id", "c"), e("v", true)),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"isNum", bson.D{{"$isNumber", "$v"}}},
			{"isStr", bson.D{{"$isString", "$v"}}},
		}}},
		bson.D{{"$sort", bson.D{{"_id", int32(1)}}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// Document a: int32 is a number, not a string.
	assert.Equal(t, true, dmap(results[0])["isNum"])
	assert.Equal(t, false, dmap(results[0])["isStr"])

	// Document b: string is not a number, is a string.
	assert.Equal(t, false, dmap(results[1])["isNum"])
	assert.Equal(t, true, dmap(results[1])["isStr"])

	// Document c: bool is not a number, not a string.
	assert.Equal(t, false, dmap(results[2])["isNum"])
	assert.Equal(t, false, dmap(results[2])["isStr"])
}

// TestExpr_project_objectToArray_back tests the $objectToArray operator
// by verifying the returned array has the correct key-value structure. (DumboDBFull)
func TestExpr_project_objectToArray_back(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("specs", d(e("color", "red"), e("size", "large")))),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"pairs", bson.D{{"$objectToArray", "$specs"}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	pairs, _ := dmap(results[0])["pairs"].(bson.A)
	require.Len(t, pairs, 2)

	keys := make([]string, 0, 2)
	for _, p := range pairs {
		pd, _ := p.(bson.M)
		keys = append(keys, dmap(pd)["k"].(string))
	}

	assert.Contains(t, keys, "color")
	assert.Contains(t, keys, "size")
}

// TestExpr_project_reduce_sum tests $reduce with $sum to sum an array. (DumboDBFull)
func TestExpr_project_reduce_sum(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("nums", bson.A{int32(1), int32(2), int32(3), int32(4)})),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"sum", bson.D{{"$reduce", bson.D{
				{"input", "$nums"},
				{"initialValue", int32(0)},
				{"in", bson.D{{"$add", bson.A{"$$value", "$$this"}}}},
			}}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(10), dmap(results[0])["sum"])
}

// TestExpr_project_in_operator tests the $in expression operator (array membership). (DumboDBFull)
func TestExpr_project_in_operator(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", "a"), e("tag", "sports")),
		d(e("_id", "b"), e("tag", "news")),
		d(e("_id", "c"), e("tag", "finance")),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"r", bson.D{{"$in", bson.A{"$tag", bson.A{"sports", "finance"}}}}},
		}}},
		bson.D{{"$sort", bson.D{{"_id", int32(1)}}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	assert.Equal(t, true, dmap(results[0])["r"], "sports is in the set")
	assert.Equal(t, false, dmap(results[1])["r"], "news is not in the set")
	assert.Equal(t, true, dmap(results[2])["r"], "finance is in the set")
}

// TestExpr_toDate_objectid tests that $toDate converts an ObjectID to its embedded timestamp. (DumboDBFull)
// MongoDB ObjectIDs encode a 4-byte Unix timestamp in their first 4 bytes; $toDate must
// extract that timestamp and return it as a date.
func TestExpr_toDate_objectid(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	// Use a fixed time truncated to second precision (ObjectID only stores seconds).
	ts := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)
	oid := bson.NewObjectIDFromTimestamp(ts)
	insertDocs(t, coll, bson.D{{Key: "_id", Value: oid}})

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"ts", bson.D{{"$toDate", "$_id"}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	got := dmap(results[0])["ts"].(bson.DateTime).Time().UTC()
	assert.Equal(t, ts, got, "$toDate should return the ObjectID's embedded timestamp")
}

// TestExpr_dateTrunc tests the $dateTrunc aggregation expression operator. (DumboDBFull)
// $dateTrunc truncates a date to the start of a specified time unit.
func TestExpr_dateTrunc(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	// 2024-03-15 14:32:45 UTC
	ts := bson.NewDateTimeFromTime(time.Date(2024, 3, 15, 14, 32, 45, 0, time.UTC))
	insertDocs(t, coll,
		d(e("_id", "a"), e("ts", ts)),
	)

	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"byDay", bson.D{{"$dateTrunc", bson.D{
				{"date", "$ts"},
				{"unit", "day"},
			}}}},
			{"byHour", bson.D{{"$dateTrunc", bson.D{
				{"date", "$ts"},
				{"unit", "hour"},
			}}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	byDay := dmap(results[0])["byDay"].(bson.DateTime).Time().UTC()
	assert.Equal(t, time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC), byDay)

	byHour := dmap(results[0])["byHour"].(bson.DateTime).Time().UTC()
	assert.Equal(t, time.Date(2024, 3, 15, 14, 0, 0, 0, time.UTC), byHour)
}

// TestExpr_mod_nan_divisor verifies that $mod with a NaN divisor returns NaN
// rather than an error (matching MongoDB's IEEE 754 behavior). (DumboDBFull)
func TestExpr_mod_nan_divisor(t *testing.T) {
	t.Parallel()
	env := startDumboDB(t)
	coll := env.Collection(t)

	ctx := context.Background()
	insertDocs(t, coll,
		d(e("_id", int32(1)), e("v", float64(10))),
	)

	// $mod with a literal NaN divisor: {$mod: [10, NaN]} -> NaN (no error)
	cursor, err := coll.Aggregate(ctx, bson.A{
		bson.D{{"$project", bson.D{
			{"_id", false},
			{"r", bson.D{{"$mod", bson.A{float64(10), math.NaN()}}}},
		}}},
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	r, ok := dmap(results[0])["r"].(float64)
	require.True(t, ok, "expected float64 result, got %T", dmap(results[0])["r"])
	assert.True(t, math.IsNaN(r), "expected NaN for $mod with NaN divisor, got %v", r)
}
