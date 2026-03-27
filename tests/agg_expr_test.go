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

// ─── arithmetic extension operators ──────────────────────────────────────────

// TestAggExpr_Abs tests $abs. (DongoFull)
func TestAggExpr_Abs(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(-5))),
		d(e("_id", "b"), e("v", float64(-3.7))),
		d(e("_id", "c"), e("v", int32(4))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$abs", "$v")))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	assert.Equal(t, int32(5), results[0].Map()["r"])
	assert.InDelta(t, 3.7, results[1].Map()["r"].(float64), 0.0001)
	assert.Equal(t, int32(4), results[2].Map()["r"])
}

// TestAggExpr_Ceil tests $ceil. (DongoFull)
func TestAggExpr_Ceil(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", float64(2.1))),
		d(e("_id", "b"), e("v", float64(-1.9))),
		d(e("_id", "c"), e("v", int32(3))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$ceil", "$v")))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	assert.Equal(t, float64(3), results[0].Map()["r"])
	assert.Equal(t, float64(-1), results[1].Map()["r"])
	assert.Equal(t, int32(3), results[2].Map()["r"])
}

// TestAggExpr_Floor tests $floor. (DongoFull)
func TestAggExpr_Floor(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", float64(2.9))),
		d(e("_id", "b"), e("v", float64(-1.1))),
		d(e("_id", "c"), e("v", int32(3))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$floor", "$v")))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	assert.Equal(t, float64(2), results[0].Map()["r"])
	assert.Equal(t, float64(-2), results[1].Map()["r"])
	assert.Equal(t, int32(3), results[2].Map()["r"])
}

// TestAggExpr_Round tests $round. (DongoFull)
func TestAggExpr_Round(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", float64(2.5))),
		d(e("_id", "b"), e("v", float64(3.456))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$round", bson.A{"$v", int32(2)})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 2)

	assert.InDelta(t, 2.5, results[0].Map()["r"].(float64), 0.0001)
	assert.InDelta(t, 3.46, results[1].Map()["r"].(float64), 0.0001)
}

// TestAggExpr_Mod tests $mod. (DongoFull)
func TestAggExpr_Mod(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(10))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$mod", bson.A{"$v", int32(3)})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(1), results[0].Map()["r"])
}

// TestAggExpr_Pow tests $pow. (DongoFull)
func TestAggExpr_Pow(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(2))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$pow", bson.A{"$v", int32(10)})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.InDelta(t, 1024.0, results[0].Map()["r"].(float64), 0.0001)
}

// TestAggExpr_Sqrt tests $sqrt. (DongoFull)
func TestAggExpr_Sqrt(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(16))),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$sqrt", "$v")))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.InDelta(t, 4.0, results[0].Map()["r"].(float64), 0.0001)
}

// ─── string extension operators ───────────────────────────────────────────────

// TestAggExpr_Trim tests $trim. (DongoFull)
func TestAggExpr_Trim(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("s", "  hello  ")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$trim", d(e("input", "$s")))))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "hello", results[0].Map()["r"])
}

// TestAggExpr_Ltrim tests $ltrim. (DongoFull)
func TestAggExpr_Ltrim(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("s", "  hello  ")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$ltrim", d(e("input", "$s")))))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "hello  ", results[0].Map()["r"])
}

// TestAggExpr_Rtrim tests $rtrim. (DongoFull)
func TestAggExpr_Rtrim(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("s", "  hello  ")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$rtrim", d(e("input", "$s")))))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "  hello", results[0].Map()["r"])
}

// TestAggExpr_SubstrBytes tests $substrBytes. (DongoFull)
func TestAggExpr_SubstrBytes(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("s", "Hello World")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$substrBytes", bson.A{"$s", int32(6), int32(5)})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "World", results[0].Map()["r"])
}

// TestAggExpr_StrLenBytes tests $strLenBytes. (DongoFull)
func TestAggExpr_StrLenBytes(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("s", "hello")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("r", d(e("$strLenBytes", "$s")))))),
		d(e("$project", d(e("_id", int32(0)), e("r", true)))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(5), results[0].Map()["r"])
}

// TestAggExpr_StrLenCP tests $strLenCP. (DongoFull)
func TestAggExpr_StrLenCP(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("s", "hello")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$addFields", d(e("r", d(e("$strLenCP", "$s")))))),
		d(e("$project", d(e("_id", int32(0)), e("r", true)))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(5), results[0].Map()["r"])
}

// TestAggExpr_Split tests $split. (DongoFull)
func TestAggExpr_Split(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("s", "a,b,c")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$split", bson.A{"$s", ","})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	arr, ok := results[0].Map()["r"].(bson.A)
	require.True(t, ok)
	assert.Equal(t, bson.A{"a", "b", "c"}, arr)
}

// TestAggExpr_Strcasecmp tests $strcasecmp. (DongoFull)
func TestAggExpr_Strcasecmp(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("s", "Hello")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$strcasecmp", bson.A{"$s", "hello"})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(0), results[0].Map()["r"])
}

// TestAggExpr_IndexOfBytes tests $indexOfBytes. (DongoFull)
func TestAggExpr_IndexOfBytes(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("s", "hello world")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(e("_id", int32(0)), e("r", d(e("$indexOfBytes", bson.A{"$s", "world"})))))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, int32(6), results[0].Map()["r"])
}

// ─── $strcasecmp and $substrBytes ─────────────────────────────────────────────

// TestAggExpr_String_Strcasecmp tests $strcasecmp. (DongoFull)
func TestAggExpr_String_Strcasecmp(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("s", "Hello")),
		d(e("_id", "b"), e("s", "apple")),
		d(e("_id", "c"), e("s", "Zebra")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(
			e("_id", int32(0)),
			e("eq", d(e("$strcasecmp", bson.A{"$s", "hello"}))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// "Hello" vs "hello" → 0 (equal case-insensitively)
	assert.Equal(t, int32(0), results[0].Map()["eq"])
	// "apple" vs "hello" → -1 (apple < hello)
	assert.Equal(t, int32(-1), results[1].Map()["eq"])
	// "Zebra" vs "hello" → 1 (zebra > hello)
	assert.Equal(t, int32(1), results[2].Map()["eq"])
}

// TestAggExpr_String_SubstrBytes tests $substrBytes. (DongoFull)
func TestAggExpr_String_SubstrBytes(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("s", "Hello World")),
	)

	ctx := context.Background()
	cursor, err := coll.Aggregate(ctx, bson.A{
		d(e("$project", d(
			e("_id", int32(0)),
			e("w", d(e("$substrBytes", bson.A{"$s", int32(6), int32(5)}))),
			e("h", d(e("$substrBytes", bson.A{"$s", int32(0), int32(5)}))),
		))),
	})
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	assert.Equal(t, "World", results[0].Map()["w"])
	assert.Equal(t, "Hello", results[0].Map()["h"])
}
