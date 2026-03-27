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

// agg_expr_test.go tests $addFields / $set / $project with expression operators:
// arithmetic ($add, $subtract, $multiply, $divide),
// conditional ($cond, $ifNull),
// string ($concat, $toLower, $toUpper),
// type conversion ($toInt, $toString, $toDouble, $toDate).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ─── $addFields arithmetic ────────────────────────────────────────────────────

// TestAggExpr_AddFields_Add tests $addFields with $add. (DongoFull)
func TestAggExpr_AddFields_Add(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("x", int32(3)), e("y", int32(4))),
		d(e("_id", "b"), e("x", int32(10)), e("y", int32(20))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("sum", d(e("$add", bson.A{"$x", "$y"})))))),
		d(e("$sort", d(e("_id", int32(1))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)

	assert.Equal(t, int32(7), results[0].Map()["sum"])
	assert.Equal(t, int32(30), results[1].Map()["sum"])
}

// TestAggExpr_AddFields_AddLiteral tests $add with literal numbers. (DongoFull)
func TestAggExpr_AddFields_AddLiteral(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(5))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("result", d(e("$add", bson.A{"$v", int32(10)})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(15), results[0].Map()["result"])
}

// TestAggExpr_AddFields_Subtract tests $addFields with $subtract. (DongoFull)
func TestAggExpr_AddFields_Subtract(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("x", int32(10)), e("y", int32(3))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("diff", d(e("$subtract", bson.A{"$x", "$y"})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(7), results[0].Map()["diff"])
}

// TestAggExpr_AddFields_Multiply tests $addFields with $multiply. (DongoFull)
func TestAggExpr_AddFields_Multiply(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("price", int32(5)), e("qty", int32(3))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("total", d(e("$multiply", bson.A{"$price", "$qty"})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(15), results[0].Map()["total"])
}

// TestAggExpr_AddFields_Divide tests $addFields with $divide. (DongoFull)
func TestAggExpr_AddFields_Divide(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("total", int32(20)), e("qty", int32(4))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("unitPrice", d(e("$divide", bson.A{"$total", "$qty"})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, float64(5), results[0].Map()["unitPrice"])
}

// TestAggExpr_AddFields_NullPropagation tests that null in $add returns null. (DongoFull)
func TestAggExpr_AddFields_NullPropagation(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	// Document missing "y" field → y evaluates to null → sum is null.
	insertDocs(t, coll,
		d(e("_id", "a"), e("x", int32(5))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("sum", d(e("$add", bson.A{"$x", "$y"})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Nil(t, results[0].Map()["sum"])
}

// ─── $addFields conditional ───────────────────────────────────────────────────

// TestAggExpr_AddFields_CondArrayForm tests $cond with array form. (DongoFull)
func TestAggExpr_AddFields_CondArrayForm(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "pass"), e("score", int32(80))),
		d(e("_id", "fail"), e("score", int32(40))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("grade", d(e("$cond", bson.A{
			d(e("$gte", bson.A{"$score", int32(60)})),
			"pass",
			"fail",
		})))))),
		d(e("$sort", d(e("_id", int32(1))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)

	assert.Equal(t, "fail", results[0].Map()["grade"])
	assert.Equal(t, "pass", results[1].Map()["grade"])
}

// TestAggExpr_AddFields_CondDocumentForm tests $cond with document form. (DongoFull)
func TestAggExpr_AddFields_CondDocumentForm(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("active", true)),
		d(e("_id", "b"), e("active", false)),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("label", d(e("$cond", d(
			e("if", "$active"),
			e("then", "yes"),
			e("else", "no"),
		))))))),
		d(e("$sort", d(e("_id", int32(1))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)

	assert.Equal(t, "yes", results[0].Map()["label"])
	assert.Equal(t, "no", results[1].Map()["label"])
}

// TestAggExpr_AddFields_IfNull tests $ifNull returns replacement for missing field. (DongoFull)
func TestAggExpr_AddFields_IfNull(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(42))),
		d(e("_id", "b")), // no "v" field
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("val", d(e("$ifNull", bson.A{"$v", int32(0)})))))),
		d(e("$sort", d(e("_id", int32(1))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)

	assert.Equal(t, int32(42), results[0].Map()["val"])
	assert.Equal(t, int32(0), results[1].Map()["val"])
}

// ─── $addFields string operators ──────────────────────────────────────────────

// TestAggExpr_AddFields_Concat tests $concat string concatenation. (DongoFull)
func TestAggExpr_AddFields_Concat(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("first", "hello"), e("last", "world")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("greeting", d(e("$concat", bson.A{"$first", " ", "$last"})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "hello world", results[0].Map()["greeting"])
}

// TestAggExpr_AddFields_ToLower tests $toLower. (DongoFull)
func TestAggExpr_AddFields_ToLower(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("name", "HELLO")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("lower", d(e("$toLower", "$name")))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "hello", results[0].Map()["lower"])
}

// TestAggExpr_AddFields_ToUpper tests $toUpper. (DongoFull)
func TestAggExpr_AddFields_ToUpper(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("name", "hello")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("upper", d(e("$toUpper", "$name")))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "HELLO", results[0].Map()["upper"])
}

// ─── $addFields type conversion ───────────────────────────────────────────────

// TestAggExpr_AddFields_ToInt tests $toInt conversion. (DongoFull)
func TestAggExpr_AddFields_ToInt(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", float64(3.7))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("asInt", d(e("$toInt", "$v")))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(3), results[0].Map()["asInt"])
}

// TestAggExpr_AddFields_ToString tests $toString conversion. (DongoFull)
func TestAggExpr_AddFields_ToString(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(42))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("asStr", d(e("$toString", "$v")))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "42", results[0].Map()["asStr"])
}

// TestAggExpr_AddFields_ToDouble tests $toDouble conversion. (DongoFull)
func TestAggExpr_AddFields_ToDouble(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(5))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("asDbl", d(e("$toDouble", "$v")))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, float64(5), results[0].Map()["asDbl"])
}

// TestAggExpr_AddFields_ToDate tests $toDate from epoch milliseconds. (DongoFull)
func TestAggExpr_AddFields_ToDate(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	// 0ms = Unix epoch
	insertDocs(t, coll,
		d(e("_id", "a"), e("ms", int64(0))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("dt", d(e("$toDate", "$ms")))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	// The MongoDB Go driver decodes BSON dates as primitive.DateTime (int64 milliseconds).
	dt, ok := results[0].Map()["dt"].(primitive.DateTime)
	require.True(t, ok, "expected primitive.DateTime, got %T", results[0].Map()["dt"])
	assert.Equal(t, primitive.DateTime(0), dt)
}

// ─── $project with expressions ────────────────────────────────────────────────

// TestAggExpr_Project_Add tests $project with $add expression. (DongoFull)
func TestAggExpr_Project_Add(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("a", int32(2)), e("b", int32(3))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(
			e("_id", int32(0)),
			e("sum", d(e("$add", bson.A{"$a", "$b"}))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(5), results[0].Map()["sum"])
}

// TestAggExpr_Project_Concat tests $project with $concat expression. (DongoFull)
func TestAggExpr_Project_Concat(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("first", "foo"), e("last", "bar")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(
			e("_id", int32(0)),
			e("full", d(e("$concat", bson.A{"$first", "-", "$last"}))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "foo-bar", results[0].Map()["full"])
}

// TestAggExpr_Project_Cond tests $project with $cond expression. (DongoFull)
func TestAggExpr_Project_Cond(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("qty", int32(5))),
		d(e("_id", "b"), e("qty", int32(15))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(
			e("_id", int32(0)),
			e("discount", d(e("$cond", bson.A{
				d(e("$gte", bson.A{"$qty", int32(10)})),
				"yes",
				"no",
			}))),
		))),
		d(e("$sort", d(e("discount", int32(1))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)

	assert.Equal(t, "no", results[0].Map()["discount"])
	assert.Equal(t, "yes", results[1].Map()["discount"])
}

// TestAggExpr_Project_ToUpper tests $project with $toUpper expression. (DongoFull)
func TestAggExpr_Project_ToUpper(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("city", "london")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(
			e("_id", int32(0)),
			e("city", d(e("$toUpper", "$city"))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "LONDON", results[0].Map()["city"])
}

// TestAggExpr_Project_NestedExpressions tests chained expressions. (DongoFull)
func TestAggExpr_Project_NestedExpressions(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	// Compute (a + b) * 2
	insertDocs(t, coll,
		d(e("_id", "a"), e("a", int32(3)), e("b", int32(4))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(
			e("_id", int32(0)),
			e("result", d(e("$multiply", bson.A{
				d(e("$add", bson.A{"$a", "$b"})),
				int32(2),
			}))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(14), results[0].Map()["result"])
}

// TestAggExpr_Set_AddFields tests $set (alias for $addFields). (DongoFull)
func TestAggExpr_Set_AddFields(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("x", int32(10))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$set", d(e("doubled", d(e("$multiply", bson.A{"$x", int32(2)})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(20), results[0].Map()["doubled"])
}

// TestAggExpr_AddFields_IfNullChain tests $ifNull with multiple fallbacks. (DongoFull)
func TestAggExpr_AddFields_IfNullChain(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a")), // no a, no b fields
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		// $ifNull: [$a, $b, "default"] → $a is null, $b is null, returns "default"
		d(e("$addFields", d(e("val", d(e("$ifNull", bson.A{"$a", "$b", "default"})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "default", results[0].Map()["val"])
}

// TestAggExpr_AddFields_SubtractFloat tests $subtract with float64. (DongoFull)
func TestAggExpr_AddFields_SubtractFloat(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("x", float64(10.5)), e("y", float64(3.2))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("diff", d(e("$subtract", bson.A{"$x", "$y"})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	diff, ok := results[0].Map()["diff"].(float64)
	require.True(t, ok)
	assert.InDelta(t, 7.3, diff, 0.0001)
}

// ─── Arithmetic math operators ────────────────────────────────────────────────

// TestAggExpr_Math_Abs tests $abs. (DongoFull)
func TestAggExpr_Math_Abs(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("val", int32(-5))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("abs", d(e("$abs", "$val")))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, int32(5), results[0].Map()["abs"])
}

// TestAggExpr_Math_CeilFloor tests $ceil and $floor. (DongoFull)
func TestAggExpr_Math_CeilFloor(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("x", float64(4.3))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("c", d(e("$ceil", "$x"))),
			e("f", d(e("$floor", "$x"))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, float64(5), results[0].Map()["c"])
	assert.Equal(t, float64(4), results[0].Map()["f"])
}

// TestAggExpr_Math_Sqrt tests $sqrt. (DongoFull)
func TestAggExpr_Math_Sqrt(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"), e("x", float64(9))))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("s", d(e("$sqrt", "$x")))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.InDelta(t, float64(3), results[0].Map()["s"], 0.001)
}

// TestAggExpr_Math_PowMod tests $pow and $mod. (DongoFull)
func TestAggExpr_Math_PowMod(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"), e("x", int32(3)), e("y", int32(2))))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("p", d(e("$pow", bson.A{"$x", "$y"}))),
			e("m", d(e("$mod", bson.A{"$x", "$y"}))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.InDelta(t, float64(9), results[0].Map()["p"], 0.001)
	assert.Equal(t, int32(1), results[0].Map()["m"])
}

// TestAggExpr_Math_RoundTrunc tests $round and $trunc. (DongoFull)
func TestAggExpr_Math_RoundTrunc(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"), e("x", float64(4.7))))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("r", d(e("$round", "$x"))),
			e("tr", d(e("$trunc", "$x"))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, float64(5), results[0].Map()["r"])
	assert.Equal(t, float64(4), results[0].Map()["tr"])
}

// ─── Array operators ──────────────────────────────────────────────────────────

// TestAggExpr_Array_Size tests $size. (DongoFull)
func TestAggExpr_Array_Size(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{1, 2, 3})))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("sz", d(e("$size", "$arr")))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)
	assert.Equal(t, int32(3), results[0].Map()["sz"])
}

// TestAggExpr_Array_SliceReverseConcat tests $slice, $reverseArray, $concatArrays. (DongoFull)
func TestAggExpr_Array_SliceReverseConcat(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{int32(1), int32(2), int32(3), int32(4)})))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("sl", d(e("$slice", bson.A{"$arr", int32(2)}))),
			e("rv", d(e("$reverseArray", "$arr"))),
			e("ca", d(e("$concatArrays", bson.A{bson.A{int32(0)}, "$arr"}))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, bson.A{int32(1), int32(2)}, m["sl"])
	assert.Equal(t, bson.A{int32(4), int32(3), int32(2), int32(1)}, m["rv"])
	assert.Equal(t, bson.A{int32(0), int32(1), int32(2), int32(3), int32(4)}, m["ca"])
}

// TestAggExpr_Array_FilterMapReduce tests $filter, $map, $reduce. (DongoFull)
func TestAggExpr_Array_FilterMapReduce(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"), e("nums", bson.A{int32(1), int32(2), int32(3), int32(4)})))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("evens", d(e("$filter", d(
				e("input", "$nums"),
				e("as", "n"),
				e("cond", d(e("$eq", bson.A{"$$n", int32(2)}))),
			)))),
			e("doubled", d(e("$map", d(
				e("input", "$nums"),
				e("as", "n"),
				e("in", d(e("$multiply", bson.A{"$$n", int32(2)}))),
			)))),
			e("sum", d(e("$reduce", d(
				e("input", "$nums"),
				e("initialValue", int32(0)),
				e("in", d(e("$add", bson.A{"$$value", "$$this"}))),
			)))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, bson.A{int32(2)}, m["evens"])
	assert.Equal(t, bson.A{int32(2), int32(4), int32(6), int32(8)}, m["doubled"])
	assert.Equal(t, int32(10), m["sum"])
}

// TestAggExpr_Array_RangeInIsArray tests $range, $in, $isArray. (DongoFull)
func TestAggExpr_Array_RangeInIsArray(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"), e("arr", bson.A{int32(10), int32(20)})))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("rng", d(e("$range", bson.A{int32(0), int32(4)}))),
			e("inArr", d(e("$in", bson.A{int32(10), "$arr"}))),
			e("isArr", d(e("$isArray", "$arr"))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, bson.A{int32(0), int32(1), int32(2), int32(3)}, m["rng"])
	assert.Equal(t, true, m["inArr"])
	assert.Equal(t, true, m["isArr"])
}

// ─── String operators ─────────────────────────────────────────────────────────

// TestAggExpr_String_StrLen tests $strLenBytes and $strLenCP. (DongoFull)
func TestAggExpr_String_StrLen(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"), e("s", "hello")))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("lb", d(e("$strLenBytes", "$s"))),
			e("lcp", d(e("$strLenCP", "$s"))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, int32(5), m["lb"])
	assert.Equal(t, int32(5), m["lcp"])
}

// TestAggExpr_String_SubstrSplit tests $substr, $substrCP, $split. (DongoFull)
func TestAggExpr_String_SubstrSplit(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"), e("s", "hello world")))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("sub", d(e("$substr", bson.A{"$s", int32(0), int32(5)}))),
			e("scp", d(e("$substrCP", bson.A{"$s", int32(6), int32(5)}))),
			e("sp", d(e("$split", bson.A{"$s", " "}))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, "hello", m["sub"])
	assert.Equal(t, "world", m["scp"])
	assert.Equal(t, bson.A{"hello", "world"}, m["sp"])
}

// TestAggExpr_String_Trim tests $trim, $ltrim, $rtrim. (DongoFull)
func TestAggExpr_String_Trim(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"), e("s", "  hello  ")))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("tr", d(e("$trim", d(e("input", "$s"))))),
			e("lt", d(e("$ltrim", d(e("input", "$s"))))),
			e("rt", d(e("$rtrim", d(e("input", "$s"))))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, "hello", m["tr"])
	assert.Equal(t, "hello  ", m["lt"])
	assert.Equal(t, "  hello", m["rt"])
}

// TestAggExpr_String_RegexMatch tests $regexMatch. (DongoFull)
func TestAggExpr_String_RegexMatch(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"), e("s", "hello world")))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("m1", d(e("$regexMatch", d(e("input", "$s"), e("regex", "hello"))))),
			e("m2", d(e("$regexMatch", d(e("input", "$s"), e("regex", "xyz"))))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, true, m["m1"])
	assert.Equal(t, false, m["m2"])
}

// ─── Date operators ───────────────────────────────────────────────────────────

// TestAggExpr_Date_Parts tests $year, $month, $dayOfMonth, $hour, $minute, $second. (DongoFull)
func TestAggExpr_Date_Parts(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ts := primitive.NewDateTimeFromTime(
		primitive.NewObjectID().Timestamp(), // we need a fixed time
	)
	// Use a fixed time: 2023-06-15 12:34:56 UTC
	fixedTime := primitive.DateTime(1686832496000) // ms since epoch

	insertDocs(t, coll, d(e("_id", "a"), e("dt", fixedTime)))
	_ = ts

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("yr", d(e("$year", "$dt"))),
			e("mo", d(e("$month", "$dt"))),
			e("dom", d(e("$dayOfMonth", "$dt"))),
			e("hr", d(e("$hour", "$dt"))),
			e("min", d(e("$minute", "$dt"))),
			e("sec", d(e("$second", "$dt"))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, int32(2023), m["yr"])
	assert.Equal(t, int32(6), m["mo"])
	assert.Equal(t, int32(15), m["dom"])
	assert.Equal(t, int32(12), m["hr"])
	assert.Equal(t, int32(34), m["min"])
	assert.Equal(t, int32(56), m["sec"])
}

// ─── Set operators ────────────────────────────────────────────────────────────

// TestAggExpr_Set_Union tests $setUnion. (DongoFull)
func TestAggExpr_Set_Union(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"),
		e("a", bson.A{int32(1), int32(2), int32(3)}),
		e("b", bson.A{int32(2), int32(3), int32(4)}),
	))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("u", d(e("$setUnion", bson.A{"$a", "$b"})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	u, ok := results[0].Map()["u"].(bson.A)
	require.True(t, ok)
	assert.Len(t, u, 4)
}

// TestAggExpr_Set_IntersectionDifference tests $setIntersection and $setDifference. (DongoFull)
func TestAggExpr_Set_IntersectionDifference(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"),
		e("a", bson.A{int32(1), int32(2), int32(3)}),
		e("b", bson.A{int32(2), int32(3), int32(4)}),
	))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("inter", d(e("$setIntersection", bson.A{"$a", "$b"}))),
			e("diff", d(e("$setDifference", bson.A{"$a", "$b"}))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	inter, ok := m["inter"].(bson.A)
	require.True(t, ok)
	assert.Len(t, inter, 2)

	diff, ok := m["diff"].(bson.A)
	require.True(t, ok)
	assert.Len(t, diff, 1)
}

// TestAggExpr_Set_EqualsIsSubset tests $setEquals and $setIsSubset. (DongoFull)
func TestAggExpr_Set_EqualsIsSubset(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"),
		e("a", bson.A{int32(1), int32(2)}),
		e("b", bson.A{int32(1), int32(2), int32(3)}),
	))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("eq", d(e("$setEquals", bson.A{"$a", "$a"}))),
			e("neq", d(e("$setEquals", bson.A{"$a", "$b"}))),
			e("sub", d(e("$setIsSubset", bson.A{"$a", "$b"}))),
			e("nsub", d(e("$setIsSubset", bson.A{"$b", "$a"}))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, true, m["eq"])
	assert.Equal(t, false, m["neq"])
	assert.Equal(t, true, m["sub"])
	assert.Equal(t, false, m["nsub"])
}

// TestAggExpr_Set_AnyAllElements tests $anyElementTrue and $allElementsTrue. (DongoFull)
func TestAggExpr_Set_AnyAllElements(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"),
		e("mixed", bson.A{true, false, true}),
		e("allTrue", bson.A{true, true}),
		e("allFalse", bson.A{false, false}),
	))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("anyM", d(e("$anyElementTrue", bson.A{"$mixed"}))),
			e("allT", d(e("$allElementsTrue", bson.A{"$allTrue"}))),
			e("anyF", d(e("$anyElementTrue", bson.A{"$allFalse"}))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, true, m["anyM"])
	assert.Equal(t, true, m["allT"])
	assert.Equal(t, false, m["anyF"])
}

// ─── Conditional: $switch ─────────────────────────────────────────────────────

// TestAggExpr_Switch tests $switch. (DongoFull)
func TestAggExpr_Switch(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"), e("score", int32(75))))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("grade", d(e("$switch", d(
			e("branches", bson.A{
				d(e("case", d(e("$gte", bson.A{"$score", int32(90)}))), e("then", "A")),
				d(e("case", d(e("$gte", bson.A{"$score", int32(70)}))), e("then", "B")),
				d(e("case", d(e("$gte", bson.A{"$score", int32(60)}))), e("then", "C")),
			}),
			e("default", "F"),
		))))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "B", results[0].Map()["grade"])
}

// ─── Type conversion extensions ───────────────────────────────────────────────

// TestAggExpr_Convert_BoolLongDecimal tests $toBool, $toLong, $toDecimal. (DongoFull)
func TestAggExpr_Convert_BoolLongDecimal(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll, d(e("_id", "a"), e("n", int32(42)), e("z", int32(0))))

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(
			e("b1", d(e("$toBool", "$n"))),
			e("b0", d(e("$toBool", "$z"))),
			e("l", d(e("$toLong", "$n"))),
			e("dec", d(e("$toDecimal", "$n"))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	m := results[0].Map()
	assert.Equal(t, true, m["b1"])
	assert.Equal(t, false, m["b0"])
	assert.Equal(t, int64(42), m["l"])
	assert.InDelta(t, float64(42), m["dec"], 0.001)
}
