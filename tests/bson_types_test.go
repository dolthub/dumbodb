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

// Tests for BSON type precision: Decimal128 arithmetic, Int32/Int64 type
// preservation, Binary UUID handling, and Timestamp $tsSecond/$tsIncrement.

package tests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// TestBSONTypes_Decimal128RoundTrip verifies that Decimal128 values survive
// a store-and-retrieve cycle without precision loss.
func TestBSONTypes_Decimal128RoundTrip(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	dec, err := primitive.ParseDecimal128("1234567890.0987654321")
	require.NoError(t, err)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", dec)),
	)

	ctx := context.Background()
	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))

	var got primitive.Decimal128
	for _, kv := range result {
		if kv.Key == "v" {
			got, _ = kv.Value.(primitive.Decimal128)
		}
	}

	assert.Equal(t, dec.String(), got.String(), "Decimal128 must round-trip without precision loss")
}

// TestBSONTypes_Int32Int64Preservation verifies that int32 and int64 are stored
// and retrieved with their exact types preserved.
func TestBSONTypes_Int32Int64Preservation(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "i32"), e("v", int32(42))),
		d(e("_id", "i64"), e("v", int64(9876543210))),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx, bson.D{}, nil)
	require.NoError(t, err)

	var docs []bson.D
	require.NoError(t, cursor.All(ctx, &docs))

	byID := make(map[string]bson.D, len(docs))
	for _, doc := range docs {
		for _, kv := range doc {
			if kv.Key == "_id" {
				byID[kv.Value.(string)] = doc
			}
		}
	}

	// int32 value must come back as int32.
	for _, kv := range byID["i32"] {
		if kv.Key == "v" {
			_, ok := kv.Value.(int32)
			assert.True(t, ok, "int32 must round-trip as int32, got %T", kv.Value)
		}
	}

	// int64 value must come back as int64.
	for _, kv := range byID["i64"] {
		if kv.Key == "v" {
			_, ok := kv.Value.(int64)
			assert.True(t, ok, "int64 must round-trip as int64, got %T", kv.Value)
		}
	}
}

// TestBSONTypes_BinaryUUIDRoundTrip verifies that Binary subtype 4 (UUID) survives
// a round-trip without subtype loss.
func TestBSONTypes_BinaryUUIDRoundTrip(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	uuidBytes := [16]byte{
		0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
		0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10,
	}
	uuidBin := primitive.Binary{Subtype: 0x04, Data: uuidBytes[:]}

	insertDocs(t, coll,
		d(e("_id", "uuid"), e("v", uuidBin)),
	)

	ctx := context.Background()
	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "uuid"))).Decode(&result))

	var gotBin primitive.Binary
	for _, kv := range result {
		if kv.Key == "v" {
			gotBin, _ = kv.Value.(primitive.Binary)
		}
	}

	assert.Equal(t, uint8(0x04), gotBin.Subtype, "Binary subtype 4 (UUID) must be preserved")
	assert.Equal(t, uuidBytes[:], gotBin.Data, "UUID bytes must be preserved exactly")
}

// TestBSONTypes_TimestampTsSecond verifies that $tsSecond returns the seconds
// component (high 32 bits) of a Timestamp value.
func TestBSONTypes_TimestampTsSecond(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	// Construct a timestamp: seconds=1700000000, increment=42.
	const seconds = int64(1700000000)
	const increment = int64(42)
	ts := primitive.Timestamp{T: uint32(seconds), I: uint32(increment)}

	insertDocs(t, coll,
		d(e("_id", "ts1"), e("ts", ts)),
	)

	ctx := context.Background()
	pipeline := bson.A{
		d(e("$project", d(
			e("sec", d(e("$tsSecond", "$ts"))),
		))),
	}
	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var sec int64
	for _, kv := range results[0] {
		if kv.Key == "sec" {
			switch v := kv.Value.(type) {
			case int32:
				sec = int64(v)
			case int64:
				sec = v
			}
		}
	}

	assert.Equal(t, seconds, sec, "$tsSecond must return the seconds component")
}

// TestBSONTypes_TimestampTsIncrement verifies that $tsIncrement returns the
// increment/ordinal component (low 32 bits) of a Timestamp value.
func TestBSONTypes_TimestampTsIncrement(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	const seconds = int64(1700000000)
	const increment = int64(42)
	ts := primitive.Timestamp{T: uint32(seconds), I: uint32(increment)}

	insertDocs(t, coll,
		d(e("_id", "ts1"), e("ts", ts)),
	)

	ctx := context.Background()
	pipeline := bson.A{
		d(e("$project", d(
			e("inc", d(e("$tsIncrement", "$ts"))),
		))),
	}
	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var inc int64
	for _, kv := range results[0] {
		if kv.Key == "inc" {
			switch v := kv.Value.(type) {
			case int32:
				inc = int64(v)
			case int64:
				inc = v
			}
		}
	}

	assert.Equal(t, increment, inc, "$tsIncrement must return the increment component")
}

// TestBSONTypes_Decimal128SumArithmetic verifies that $sum in $group produces a
// Decimal128 result when the input contains Decimal128 values.
func TestBSONTypes_Decimal128SumArithmetic(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	dec, err := primitive.ParseDecimal128("42.5")
	require.NoError(t, err)

	insertDocs(t, coll,
		d(e("_id", "d1"), e("v", dec)),
		d(e("_id", "d2"), e("v", dec)),
	)

	ctx := context.Background()
	pipeline := bson.A{
		d(e("$group", d(
			e("_id", nil),
			e("total", d(e("$sum", "$v"))),
		))),
	}
	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var total primitive.Decimal128
	var isDecimal bool
	for _, kv := range results[0] {
		if kv.Key == "total" {
			total, isDecimal = kv.Value.(primitive.Decimal128)
		}
	}

	assert.True(t, isDecimal, "$sum of Decimal128 values must return Decimal128, got non-Decimal128")

	// 42.5 + 42.5 = 85.0 — Decimal128 preserves the scale of the operands.
	expected, err := primitive.ParseDecimal128("85.0")
	require.NoError(t, err)
	assert.Equal(t, expected.String(), total.String(), "$sum result must be 85.0")
}

// TestBSONTypes_Decimal128AvgArithmetic verifies that $avg in $group produces a
// Decimal128 result when the input contains Decimal128 values.
func TestBSONTypes_Decimal128AvgArithmetic(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	dec10, err := primitive.ParseDecimal128("10")
	require.NoError(t, err)
	dec20, err := primitive.ParseDecimal128("20")
	require.NoError(t, err)
	dec30, err := primitive.ParseDecimal128("30")
	require.NoError(t, err)

	insertDocs(t, coll,
		d(e("_id", "a"), e("v", dec10)),
		d(e("_id", "b"), e("v", dec20)),
		d(e("_id", "c"), e("v", dec30)),
	)

	ctx := context.Background()
	pipeline := bson.A{
		d(e("$group", d(
			e("_id", nil),
			e("avg", d(e("$avg", "$v"))),
		))),
	}
	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var avg primitive.Decimal128
	var isDecimal bool
	for _, kv := range results[0] {
		if kv.Key == "avg" {
			avg, isDecimal = kv.Value.(primitive.Decimal128)
		}
	}

	assert.True(t, isDecimal, "$avg of Decimal128 values must return Decimal128")

	// avg(10, 20, 30) = 20
	expected, err := primitive.ParseDecimal128("20")
	require.NoError(t, err)
	assert.Equal(t, expected.String(), avg.String(), "$avg of [10,20,30] must be 20")
}

// TestBSONTypes_MultiplyOperator verifies that $multiply in $project correctly
// multiplies numeric values preserving type.
func TestBSONTypes_MultiplyOperator(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "a"), e("x", int32(3)), e("y", int32(4))),
		d(e("_id", "b"), e("x", int64(1000000)), e("y", int64(1000000))),
		d(e("_id", "c"), e("x", float64(2.5)), e("y", float64(4.0))),
	)

	ctx := context.Background()
	pipeline := bson.A{
		d(e("$project", d(
			e("result", d(e("$multiply", bson.A{"$x", "$y"}))),
		))),
		d(e("$sort", d(e("_id", int32(1))))),
	}
	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 3)

	// 3 * 4 = 12 (int32)
	for _, kv := range results[0] {
		if kv.Key == "result" {
			v, ok := kv.Value.(int32)
			assert.True(t, ok, "int32*int32 must return int32, got %T", kv.Value)
			assert.Equal(t, int32(12), v)
		}
	}

	// 1000000 * 1000000 = 1000000000000 (int64)
	for _, kv := range results[1] {
		if kv.Key == "result" {
			v, ok := kv.Value.(int64)
			assert.True(t, ok, "int64*int64 must return int64, got %T", kv.Value)
			assert.Equal(t, int64(1000000000000), v)
		}
	}

	// 2.5 * 4.0 = 10.0 (float64)
	for _, kv := range results[2] {
		if kv.Key == "result" {
			v, ok := kv.Value.(float64)
			assert.True(t, ok, "float64*float64 must return float64, got %T", kv.Value)
			assert.Equal(t, float64(10.0), v)
		}
	}
}

// TestBSONTypes_MultiplyDecimal128 verifies that $multiply returns Decimal128
// when any input is Decimal128.
func TestBSONTypes_MultiplyDecimal128(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	dec, err := primitive.ParseDecimal128("3.5")
	require.NoError(t, err)

	insertDocs(t, coll,
		d(e("_id", "a"), e("x", dec), e("y", int32(4))),
	)

	ctx := context.Background()
	pipeline := bson.A{
		d(e("$project", d(
			e("result", d(e("$multiply", bson.A{"$x", "$y"}))),
		))),
	}
	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	require.Len(t, results, 1)

	var result primitive.Decimal128
	var isDecimal bool
	for _, kv := range results[0] {
		if kv.Key == "result" {
			result, isDecimal = kv.Value.(primitive.Decimal128)
		}
	}

	assert.True(t, isDecimal, "Decimal128 * int32 must return Decimal128")

	// 3.5 * 4 = 14.0 — Decimal128 preserves the scale of the operands.
	expected, err := primitive.ParseDecimal128("14.0")
	require.NoError(t, err)
	assert.Equal(t, expected.String(), result.String(), "3.5 * 4 must equal 14.0")
}
