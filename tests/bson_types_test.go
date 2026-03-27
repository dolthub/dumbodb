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

// Tests for BSON type precision: Decimal128 arithmetic, Int32/Int64 type
// preservation, Binary UUID handling, Timestamp $tsSecond/$tsIncrement,
// $type queries by number and alias, Date edge cases, ObjectId handling,
// Boolean/Null/Regex roundtrips, and Double special values.

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
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

// TestBSONTypes_TypeQueryByAlias verifies that {field: {$type: "alias"}} filters
// documents by BSON type using string alias names.
func TestBSONTypes_TypeQueryByAlias(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	dec, err := primitive.ParseDecimal128("1.5")
	require.NoError(t, err)

	insertDocs(t, coll,
		d(e("_id", "double"), e("v", float64(3.14))),
		d(e("_id", "string"), e("v", "hello")),
		d(e("_id", "object"), e("v", d(e("x", 1)))),
		d(e("_id", "array"), e("v", bson.A{})),
		d(e("_id", "bool"), e("v", true)),
		d(e("_id", "date"), e("v", time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC))),
		d(e("_id", "null"), e("v", nil)),
		d(e("_id", "int32"), e("v", int32(42))),
		d(e("_id", "int64"), e("v", int64(9876543210))),
		d(e("_id", "decimal"), e("v", dec)),
	)

	ctx := context.Background()

	tests := []struct {
		alias   string
		wantIDs []interface{}
	}{
		{"double", []interface{}{"double"}},
		{"string", []interface{}{"string"}},
		{"object", []interface{}{"object"}},
		{"array", []interface{}{"array"}},
		{"bool", []interface{}{"bool"}},
		{"date", []interface{}{"date"}},
		{"null", []interface{}{"null"}},
		{"int", []interface{}{"int32"}},
		{"long", []interface{}{"int64"}},
	}

	for _, tc := range tests {
		t.Run(tc.alias, func(t *testing.T) {
			t.Parallel()
			cursor, err := coll.Find(ctx,
				d(e("v", d(e("$type", tc.alias)))),
				options.Find().SetSort(d(e("_id", 1))),
			)
			require.NoError(t, err)

			var results []bson.D
			require.NoError(t, cursor.All(ctx, &results))

			ids := make([]interface{}, len(results))
			for i, r := range results {
				for _, kv := range r {
					if kv.Key == "_id" {
						ids[i] = kv.Value
					}
				}
			}
			assert.Equal(t, tc.wantIDs, ids, "$type %q must match correct documents", tc.alias)
		})
	}
}

// TestBSONTypes_TypeQueryByNumber verifies that {field: {$type: <number>}} filters
// documents by BSON type using numeric codes.
func TestBSONTypes_TypeQueryByNumber(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "double"), e("v", float64(1.1))),
		d(e("_id", "string"), e("v", "text")),
		d(e("_id", "bool"), e("v", false)),
		d(e("_id", "int32"), e("v", int32(10))),
		d(e("_id", "int64"), e("v", int64(20))),
	)

	ctx := context.Background()

	// BSON type codes: double=1, string=2, bool=8, int=16, long=18.
	tests := []struct {
		code    int32
		wantIDs []interface{}
	}{
		{1, []interface{}{"double"}},
		{2, []interface{}{"string"}},
		{8, []interface{}{"bool"}},
		{16, []interface{}{"int32"}},
		{18, []interface{}{"int64"}},
	}

	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			cursor, err := coll.Find(ctx,
				d(e("v", d(e("$type", tc.code)))),
				options.Find().SetSort(d(e("_id", 1))),
			)
			require.NoError(t, err)

			var results []bson.D
			require.NoError(t, cursor.All(ctx, &results))

			ids := make([]interface{}, len(results))
			for i, r := range results {
				for _, kv := range r {
					if kv.Key == "_id" {
						ids[i] = kv.Value
					}
				}
			}
			assert.Equal(t, tc.wantIDs, ids, "$type code %d must match correct documents", tc.code)
		})
	}
}

// TestBSONTypes_TypeQueryNumberAlias verifies the "number" alias matches double, int32,
// and int64 but not string, bool, date, or object.
func TestBSONTypes_TypeQueryNumberAlias(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "double"), e("v", float64(3.14))),
		d(e("_id", "int32"), e("v", int32(1))),
		d(e("_id", "int64"), e("v", int64(1000000000000))),
		d(e("_id", "string"), e("v", "nope")),
		d(e("_id", "bool"), e("v", true)),
	)

	ids := queryIDs(t, coll, d(e("v", d(e("$type", "number")))))

	assert.ElementsMatch(t, []interface{}{"double", "int32", "int64"}, ids,
		"$type 'number' must match double, int, and long only")
}

// TestBSONTypes_TypeQueryArray verifies that $type matches array values.
func TestBSONTypes_TypeQueryArray(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "arr"), e("v", bson.A{1, 2})),
		d(e("_id", "str"), e("v", "not an array")),
		d(e("_id", "obj"), e("v", d(e("a", 1)))),
	)

	ids := queryIDs(t, coll, d(e("v", d(e("$type", "array")))))

	assert.Equal(t, []interface{}{"arr"}, ids, "$type 'array' must match only array fields")
}

// TestBSONTypes_DateRoundTrip verifies that a Date value survives a store-and-retrieve
// cycle with millisecond precision.
func TestBSONTypes_DateRoundTrip(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	ts := time.Date(2024, 6, 15, 12, 30, 45, 123000000, time.UTC)
	insertDocs(t, coll, d(e("_id", "a"), e("v", ts)))

	ctx := context.Background()
	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))

	for _, kv := range result {
		if kv.Key == "v" {
			got, ok := kv.Value.(primitive.DateTime)
			if !ok {
				t.Fatalf("expected primitive.DateTime, got %T", kv.Value)
			}
			gotTime := got.Time()
			assert.Equal(t, ts.UTC().Truncate(time.Millisecond), gotTime.UTC(),
				"Date must round-trip with millisecond precision")
		}
	}
}

// TestBSONTypes_DatePreEpoch verifies that pre-epoch dates (before 1970-01-01) are
// stored and retrieved correctly.
func TestBSONTypes_DatePreEpoch(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	// 1960-07-04 00:00:00 UTC — well before Unix epoch.
	preEpoch := time.Date(1960, 7, 4, 0, 0, 0, 0, time.UTC)
	insertDocs(t, coll, d(e("_id", "a"), e("v", preEpoch)))

	ctx := context.Background()
	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))

	for _, kv := range result {
		if kv.Key == "v" {
			got, ok := kv.Value.(primitive.DateTime)
			if !ok {
				t.Fatalf("expected primitive.DateTime, got %T", kv.Value)
			}
			gotTime := got.Time()
			assert.Equal(t, preEpoch.UTC(), gotTime.UTC(), "pre-epoch Date must round-trip correctly")
		}
	}
}

// TestBSONTypes_DateMaxValue verifies that the maximum BSON Date value (year 9999)
// can be stored and retrieved.
func TestBSONTypes_DateMaxValue(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	// Maximum representable date in MongoDB: December 31, 9999.
	maxDate := time.Date(9999, 12, 31, 23, 59, 59, 999000000, time.UTC)
	insertDocs(t, coll, d(e("_id", "a"), e("v", maxDate)))

	ctx := context.Background()
	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))

	for _, kv := range result {
		if kv.Key == "v" {
			got, ok := kv.Value.(primitive.DateTime)
			if !ok {
				t.Fatalf("expected primitive.DateTime, got %T", kv.Value)
			}
			gotTime := got.Time()
			assert.Equal(t, maxDate.UTC().Truncate(time.Millisecond), gotTime.UTC(),
				"max Date value must round-trip correctly")
		}
	}
}

// TestBSONTypes_ObjectIDRoundTrip verifies that ObjectId values survive a
// store-and-retrieve cycle with their exact 12-byte value preserved.
func TestBSONTypes_ObjectIDRoundTrip(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	oid := primitive.NewObjectID()
	insertDocs(t, coll, d(e("_id", oid), e("v", "marker")))

	ctx := context.Background()
	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", oid))).Decode(&result))

	for _, kv := range result {
		if kv.Key == "_id" {
			got, ok := kv.Value.(primitive.ObjectID)
			require.True(t, ok, "ObjectId must be retrieved as primitive.ObjectID")
			assert.Equal(t, oid, got, "ObjectId must round-trip exactly")
		}
	}
}

// TestBSONTypes_ObjectIDTimestamp verifies that the timestamp embedded in an ObjectId
// (first 4 bytes, big-endian seconds since epoch) is accessible correctly.
func TestBSONTypes_ObjectIDTimestamp(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	before := time.Now().UTC().Truncate(time.Second)
	oid := primitive.NewObjectID()
	after := time.Now().UTC().Add(time.Second).Truncate(time.Second)

	insertDocs(t, coll, d(e("_id", oid)))

	ctx := context.Background()
	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", oid))).Decode(&result))

	for _, kv := range result {
		if kv.Key == "_id" {
			got, ok := kv.Value.(primitive.ObjectID)
			require.True(t, ok)
			ts := got.Timestamp()
			assert.True(t, !ts.Before(before) && !ts.After(after),
				"ObjectId timestamp %v must be between %v and %v", ts, before, after)
		}
	}
}

// TestBSONTypes_ObjectIDTypeQuery verifies that $type "objectId" filter matches
// ObjectId fields correctly.
func TestBSONTypes_ObjectIDTypeQuery(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	oid := primitive.NewObjectID()
	insertDocs(t, coll,
		d(e("_id", "oid-doc"), e("v", oid)),
		d(e("_id", "str-doc"), e("v", "not-an-oid")),
	)

	ids := queryIDs(t, coll, d(e("v", d(e("$type", "objectId")))))

	assert.Equal(t, []interface{}{"oid-doc"}, ids, "$type 'objectId' must match only ObjectId fields")
}

// TestBSONTypes_BooleanRoundTrip verifies that true and false bool values survive
// a store-and-retrieve cycle.
func TestBSONTypes_BooleanRoundTrip(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "t"), e("v", true)),
		d(e("_id", "f"), e("v", false)),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(d(e("_id", 1))))
	require.NoError(t, err)

	var docs []bson.D
	require.NoError(t, cursor.All(ctx, &docs))
	require.Len(t, docs, 2)

	byID := make(map[string]bson.D)
	for _, doc := range docs {
		for _, kv := range doc {
			if kv.Key == "_id" {
				byID[kv.Value.(string)] = doc
			}
		}
	}

	for _, kv := range byID["t"] {
		if kv.Key == "v" {
			v, ok := kv.Value.(bool)
			assert.True(t, ok, "bool 'true' must come back as bool, got %T", kv.Value)
			assert.True(t, v, "stored true must be retrieved as true")
		}
	}
	for _, kv := range byID["f"] {
		if kv.Key == "v" {
			v, ok := kv.Value.(bool)
			assert.True(t, ok, "bool 'false' must come back as bool, got %T", kv.Value)
			assert.False(t, v, "stored false must be retrieved as false")
		}
	}
}

// TestBSONTypes_NullRoundTrip verifies that null values are stored and retrieved
// correctly and that {field: null} matches both missing fields and explicit nulls.
func TestBSONTypes_NullRoundTrip(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "null-val"), e("v", nil)),
		d(e("_id", "no-field")),
		d(e("_id", "has-val"), e("v", int32(1))),
	)

	ctx := context.Background()

	t.Run("ExplicitNullRoundtrip", func(t *testing.T) {
		t.Parallel()
		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "null-val"))).Decode(&result))
		for _, kv := range result {
			if kv.Key == "v" {
				assert.Nil(t, kv.Value, "explicit null must round-trip as nil")
			}
		}
	})

	t.Run("NullQueryMatchesMissing", func(t *testing.T) {
		t.Parallel()
		ids := queryIDs(t, coll, d(e("v", nil)))
		assert.ElementsMatch(t, []interface{}{"null-val", "no-field"}, ids,
			"{v: null} must match both explicit null and missing field")
	})

	t.Run("TypeQueryNull", func(t *testing.T) {
		t.Parallel()
		ids := queryIDs(t, coll, d(e("v", d(e("$type", "null")))))
		assert.Equal(t, []interface{}{"null-val"}, ids,
			"$type 'null' must match only explicit null (not missing fields)")
	})
}

// TestBSONTypes_RegexRoundTrip verifies that regex values (pattern + options) survive
// a store-and-retrieve cycle.
func TestBSONTypes_RegexRoundTrip(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	re := primitive.Regex{Pattern: "^foo", Options: "i"}
	insertDocs(t, coll, d(e("_id", "a"), e("v", re)))

	ctx := context.Background()
	var result bson.D
	require.NoError(t, coll.FindOne(ctx, d(e("_id", "a"))).Decode(&result))

	for _, kv := range result {
		if kv.Key == "v" {
			got, ok := kv.Value.(primitive.Regex)
			require.True(t, ok, "regex must come back as primitive.Regex, got %T", kv.Value)
			assert.Equal(t, re.Pattern, got.Pattern, "regex pattern must be preserved")
			assert.Equal(t, re.Options, got.Options, "regex options must be preserved")
		}
	}
}

// TestBSONTypes_RegexTypeQuery verifies that $type "regex" matches regex fields.
func TestBSONTypes_RegexTypeQuery(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "re-doc"), e("v", primitive.Regex{Pattern: "abc", Options: ""})),
		d(e("_id", "str-doc"), e("v", "abc")),
	)

	ids := queryIDs(t, coll, d(e("v", d(e("$type", "regex")))))

	assert.Equal(t, []interface{}{"re-doc"}, ids, "$type 'regex' must match only regex fields")
}

// TestBSONTypes_DoubleTypeQuery verifies that $type "double" matches float64 fields
// and not integer fields.
func TestBSONTypes_DoubleTypeQuery(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "double"), e("v", float64(3.14))),
		d(e("_id", "int32"), e("v", int32(3))),
		d(e("_id", "int64"), e("v", int64(3))),
	)

	ids := queryIDs(t, coll, d(e("v", d(e("$type", "double")))))

	assert.Equal(t, []interface{}{"double"}, ids,
		"$type 'double' must match only float64 fields, not int32 or int64")
}

// TestBSONTypes_BinDataSubtypePreservation verifies that binary data with different
// subtypes (generic=0, UUID=4) are stored with their subtypes preserved.
func TestBSONTypes_BinDataSubtypePreservation(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	genericBin := primitive.Binary{Subtype: 0x00, Data: []byte{0x01, 0x02, 0x03}}
	uuidBin := primitive.Binary{Subtype: 0x04, Data: make([]byte, 16)} // UUID subtype

	insertDocs(t, coll,
		d(e("_id", "generic"), e("v", genericBin)),
		d(e("_id", "uuid"), e("v", uuidBin)),
	)

	ctx := context.Background()
	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(d(e("_id", 1))))
	require.NoError(t, err)

	var docs []bson.D
	require.NoError(t, cursor.All(ctx, &docs))

	byID := make(map[string]primitive.Binary)
	for _, doc := range docs {
		var id string
		for _, kv := range doc {
			if kv.Key == "_id" {
				id = kv.Value.(string)
			}
		}
		for _, kv := range doc {
			if kv.Key == "v" {
				byID[id] = kv.Value.(primitive.Binary)
			}
		}
	}

	assert.Equal(t, uint8(0x00), byID["generic"].Subtype, "generic binData subtype must be 0x00")
	assert.Equal(t, []byte{0x01, 0x02, 0x03}, byID["generic"].Data, "generic binData bytes must be preserved")
	assert.Equal(t, uint8(0x04), byID["uuid"].Subtype, "UUID binData subtype must be 0x04")
}

// TestBSONTypes_BinDataTypeQuery verifies that $type "binData" matches binary
// fields and not string fields.
func TestBSONTypes_BinDataTypeQuery(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "bin"), e("v", primitive.Binary{Subtype: 0x00, Data: []byte{0xDE, 0xAD}})),
		d(e("_id", "str"), e("v", "binary-like string")),
	)

	ids := queryIDs(t, coll, d(e("v", d(e("$type", "binData")))))

	assert.Equal(t, []interface{}{"bin"}, ids, "$type 'binData' must match only binary fields")
}

// TestBSONTypes_Int32Int64TypeDistinction verifies that $type distinguishes between
// int32 ("int") and int64 ("long").
func TestBSONTypes_Int32Int64TypeDistinction(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "i32"), e("v", int32(100))),
		d(e("_id", "i64"), e("v", int64(100))),
	)

	intIDs := queryIDs(t, coll, d(e("v", d(e("$type", "int")))))
	assert.Equal(t, []interface{}{"i32"}, intIDs, "$type 'int' must match int32 only")

	longIDs := queryIDs(t, coll, d(e("v", d(e("$type", "long")))))
	assert.Equal(t, []interface{}{"i64"}, longIDs, "$type 'long' must match int64 only")
}

// TestBSONTypes_NestedObjectRoundTrip verifies that nested document values survive
// a store-and-retrieve cycle and $type "object" matches them.
func TestBSONTypes_NestedObjectRoundTrip(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	nested := d(e("x", int32(1)), e("y", "two"), e("z", d(e("deep", true))))
	insertDocs(t, coll,
		d(e("_id", "obj"), e("v", nested)),
		d(e("_id", "str"), e("v", "not-an-object")),
	)

	ctx := context.Background()

	t.Run("RoundTrip", func(t *testing.T) {
		t.Parallel()
		var result bson.D
		require.NoError(t, coll.FindOne(ctx, d(e("_id", "obj"))).Decode(&result))
		for _, kv := range result {
			if kv.Key == "v" {
				_, ok := kv.Value.(bson.D)
				assert.True(t, ok, "nested object must round-trip as bson.D, got %T", kv.Value)
			}
		}
	})

	t.Run("TypeQuery", func(t *testing.T) {
		t.Parallel()
		ids := queryIDs(t, coll, d(e("v", d(e("$type", "object")))))
		assert.Equal(t, []interface{}{"obj"}, ids, "$type 'object' must match nested document fields only")
	})
}

// TestBSONTypes_DateTypeQuery verifies that $type "date" matches date fields.
func TestBSONTypes_DateTypeQuery(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "date"), e("v", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))),
		d(e("_id", "int64"), e("v", int64(1704067200000))),
		d(e("_id", "str"), e("v", "2024-01-01")),
	)

	ids := queryIDs(t, coll, d(e("v", d(e("$type", "date")))))

	assert.Equal(t, []interface{}{"date"}, ids, "$type 'date' must match only Date fields, not int64 or string")
}

// TestBSONTypes_TypeAggregationOperator verifies that the $type aggregation expression
// returns the correct type name string for various field types.
func TestBSONTypes_TypeAggregationOperator(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "double"), e("v", float64(1.5))),
		d(e("_id", "string"), e("v", "text")),
		d(e("_id", "bool"), e("v", true)),
		d(e("_id", "int32"), e("v", int32(42))),
		d(e("_id", "int64"), e("v", int64(9999999999))),
		d(e("_id", "date"), e("v", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))),
		d(e("_id", "null"), e("v", nil)),
		d(e("_id", "array"), e("v", bson.A{1, 2})),
		d(e("_id", "object"), e("v", d(e("a", 1)))),
	)

	ctx := context.Background()
	pipeline := bson.A{
		d(e("$project", d(
			e("typeName", d(e("$type", "$v"))),
		))),
		d(e("$sort", d(e("_id", int32(1))))),
	}
	cursor, err := coll.Aggregate(ctx, pipeline)
	require.NoError(t, err)

	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	typeByID := make(map[string]string)
	for _, doc := range results {
		var id, typeName string
		for _, kv := range doc {
			switch kv.Key {
			case "_id":
				id = kv.Value.(string)
			case "typeName":
				typeName, _ = kv.Value.(string)
			}
		}
		typeByID[id] = typeName
	}

	want := map[string]string{
		"double": "double",
		"string": "string",
		"bool":   "bool",
		"int32":  "int",
		"int64":  "long",
		"date":   "date",
		"null":   "null",
		"array":  "array",
		"object": "object",
	}
	for id, wantType := range want {
		assert.Equal(t, wantType, typeByID[id], "$type aggregation on %q must return %q", id, wantType)
	}
}
