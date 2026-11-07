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

package index

import (
	"bytes"
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Adjacent pairs of MongoDB's documented BSON type-comparison order
// must sort correctly at the byte level.
func TestKeyStringTypeBracketOrder(t *testing.T) {
	t.Parallel()

	ordered := []struct {
		name string
		v    any
	}{
		{"MinKey", types.MinKey},
		{"undefined (a top-level empty array)", types.Undefined},
		{"Null", types.Null},
		{"NaN", math.NaN()},
		{"-Inf", math.Inf(-1)},
		{"negative int", int64(-5)},
		{"negative fraction", float64(-0.5)},
		{"zero", int64(0)},
		{"positive fraction", float64(0.5)},
		{"positive int", int64(7)},
		{"+Inf", math.Inf(1)},
		{"string", "alpha"},
		{"object", types.MakeDocument(0)},
		{"array", types.MakeArray(0)},
		{"binary", types.Binary{Subtype: 0, B: []byte{1}}},
		{"objectid", types.ObjectID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}},
		{"bool false", false},
		{"bool true", true},
		{"date", time.Unix(1000, 0)},
		{"timestamp", types.Timestamp(1 << 40)},
		{"regex", types.Regex{Pattern: "^a", Options: "i"}},
		{"MaxKey", types.MaxKey},
	}

	for i := 0; i < len(ordered)-1; i++ {
		a, b := ordered[i], ordered[i+1]
		ka, kb := EncodeValue(a.v), EncodeValue(b.v)
		if bytes.Compare(ka, kb) >= 0 {
			t.Errorf("bracket order violated: %s (%x) must sort before %s (%x)",
				a.name, ka, b.name, kb)
		}
	}
}

func TestMixedNumericByteOrder(t *testing.T) {
	t.Parallel()

	ascending := []any{
		math.Inf(-1),
		float64(-1e15),
		float64(-256.5),
		int64(-256),
		float64(-255.5),
		int64(-255),
		int64(-3),
		float64(-2.9),
		float64(-2.5),
		int64(-2),
		float64(-0.5),
		float64(-0.25),
		int64(0),
		float64(0.25),
		float64(0.5),
		int64(1),
		float64(1.5),
		int64(2),
		float64(2.5),
		int64(3),
		float64(100.5),
		int64(255),
		float64(255.5),
		int64(256),
		float64(256.5),
		float64(1e15),
		math.Inf(1),
	}

	for i := 0; i < len(ascending)-1; i++ {
		ka, kb := EncodeValue(ascending[i]), EncodeValue(ascending[i+1])
		if bytes.Compare(ka, kb) >= 0 {
			t.Errorf("numeric order violated: %v (%x) must sort before %v (%x)",
				ascending[i], ka, ascending[i+1], kb)
		}
	}
}

func TestNumericEqualityUnification(t *testing.T) {
	t.Parallel()

	pairs := []struct{ a, b any }{
		{int32(2), int64(2)},
		{int64(2), float64(2.0)},
		{int32(0), float64(0.0)},
		{int64(-7), float64(-7.0)},
		{int64(1 << 40), float64(1 << 40)},
	}
	for _, p := range pairs {
		ka, kb := EncodeValue(p.a), EncodeValue(p.b)
		if !bytes.Equal(ka, kb) {
			t.Errorf("EncodeValue(%v)=%x != EncodeValue(%v)=%x", p.a, ka, p.b, kb)
		}
	}
}

func TestNumericOrderFuzz(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(42))
	vals := make([]float64, 0, 4000)
	for i := 0; i < 1000; i++ {
		vals = append(vals, (rng.Float64()-0.5)*2000)
		vals = append(vals, float64(rng.Intn(2000)-1000))
		vals = append(vals, (rng.Float64()-0.5)*2e12)
		vals = append(vals, rng.NormFloat64())
	}
	sort.Float64s(vals)
	for i := 0; i < len(vals)-1; i++ {
		if vals[i] == vals[i+1] {
			continue
		}
		ka, kb := EncodeValue(vals[i]), EncodeValue(vals[i+1])
		if bytes.Compare(ka, kb) >= 0 {
			t.Fatalf("fuzz order violated at %v (%x) vs %v (%x)",
				vals[i], ka, vals[i+1], kb)
		}
	}
}

func TestTimestampRegexEncodingOrder(t *testing.T) {
	t.Parallel()

	ts := []types.Timestamp{0, 1, types.Timestamp(1) << 32, types.Timestamp(2) << 32}
	for i := 0; i < len(ts)-1; i++ {
		if bytes.Compare(EncodeValue(ts[i]), EncodeValue(ts[i+1])) >= 0 {
			t.Errorf("timestamp order violated: %v before %v", ts[i], ts[i+1])
		}
	}

	res := []types.Regex{
		{Pattern: "^a", Options: ""},
		{Pattern: "^a", Options: "i"},
		{Pattern: "^b", Options: ""},
	}
	for i := 0; i < len(res)-1; i++ {
		if bytes.Compare(EncodeValue(res[i]), EncodeValue(res[i+1])) >= 0 {
			t.Errorf("regex order violated: %v before %v", res[i], res[i+1])
		}
	}
	if bytes.Equal(EncodeValue(res[0]), EncodeValue(res[1])) {
		t.Errorf("regex options must distinguish encodings")
	}
}

func TestEncodeValueLossy(t *testing.T) {
	t.Parallel()

	if !EncodeValueLossy(struct{}{}) {
		t.Errorf("a type without an encoding must be lossy")
	}
	for _, v := range []any{
		nil, types.Null, true, int32(1), int64(1), float64(1.5), math.NaN(), types.Decimal128{H: 1, L: 2},
		"s", types.ObjectID{}, time.Unix(0, 0), types.Binary{},
		types.Timestamp(9), types.Regex{Pattern: "a"},
		types.MakeDocument(0), types.MakeArray(0),
		types.MaxKey, types.MinKey,
	} {
		if EncodeValueLossy(v) {
			t.Errorf("EncodeValueLossy(%T) = true, want false", v)
		}
	}
}

func TestBracketRangeContainsValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		v       any
		members []any
		outside []any
	}{
		{
			name:    "numeric",
			v:       int64(5),
			members: []any{math.Inf(-1), int64(-3), float64(2.5), int64(9), math.Inf(1)},
			outside: []any{math.NaN(), types.Null, "s", time.Unix(0, 0), true},
		},
		{
			name:    "string",
			v:       "m",
			members: []any{"", "a", "zzz"},
			outside: []any{int64(5), types.MakeDocument(0), true},
		},
		{
			name:    "date",
			v:       time.Unix(50, 0),
			members: []any{time.Unix(0, 0), time.Unix(1e6, 0)},
			outside: []any{types.Timestamp(1), int64(5), true},
		},
	}
	for _, c := range cases {
		start, stop, ok := BracketRange(c.v)
		if !ok {
			t.Fatalf("%s: BracketRange not ok", c.name)
		}
		for _, m := range c.members {
			k := EncodeValue(m)
			if bytes.Compare(k, start) < 0 || bytes.Compare(k, stop) >= 0 {
				t.Errorf("%s: member %v (%x) outside bracket [%x, %x)", c.name, m, k, start, stop)
			}
		}
		for _, o := range c.outside {
			k := EncodeValue(o)
			if bytes.Compare(k, start) >= 0 && bytes.Compare(k, stop) < 0 {
				t.Errorf("%s: non-member %v (%x) inside bracket [%x, %x)", c.name, o, k, start, stop)
			}
		}
	}
}

// Doubles of any magnitude encode exactly, in order: large ones are not lossy.
func TestValueLossy_LargeFloat64(t *testing.T) {
	ordered := []float64{-1e30, -1e25, float64(-9e18), -1.5, 0, 1.5, float64(9e18), float64(5e19), 1e25, 1e30}
	for i, f := range ordered {
		if ValueLossy(f) {
			t.Errorf("ValueLossy(%v) = true, want false", f)
		}
		if i > 0 && bytes.Compare(EncodeValue(ordered[i-1]), EncodeValue(f)) >= 0 {
			t.Errorf("%v must encode before %v", ordered[i-1], f)
		}
	}
}

func decimal128(coefficient uint64, exponent int, negative bool) types.Decimal128 {
	h := uint64(exponent+6176) << 49
	if negative {
		h |= 1 << 63
	}
	return types.Decimal128{H: h, L: coefficient}
}

// A Decimal128 equal to an integer or double encodes as it, and inexact ones
// fall between their neighboring doubles.
func TestDecimal128Encoding(t *testing.T) {
	for _, c := range []struct {
		dec  types.Decimal128
		same any
	}{
		{decimal128(3, 0, false), int32(3)},
		{decimal128(30, -1, false), 3.0},
		{decimal128(35, -1, true), -3.5},
		{decimal128(9007199254740993, 0, false), int64(9007199254740993)},
		{decimal128(0, 5, true), int32(0)},
		{types.Decimal128{H: 0x7800000000000000}, math.Inf(1)},
		{types.Decimal128{H: 0x7c00000000000000}, math.NaN()},
	} {
		if !bytes.Equal(EncodeValue(c.dec), EncodeValue(c.same)) {
			t.Errorf("%v encodes %x, want %x like %v", c.dec, EncodeValue(c.dec), EncodeValue(c.same), c.same)
		}
	}
	ordered := []any{
		math.Inf(-1), decimal128(1, 400, true), -math.MaxFloat64, decimal128(1, 300, true), -1e299,
		decimal128(1, -1, true), decimal128(1, -400, true), int32(0), decimal128(1, -400, false), 1e-300,
		decimal128(1, -1, false), 0.1, decimal128(123456789012345678, -1, false), 1.2345678901234568e16,
		decimal128(1, 300, false), math.MaxFloat64, decimal128(1, 400, false), math.Inf(1),
	}
	for i := 1; i < len(ordered); i++ {
		if bytes.Compare(EncodeValue(ordered[i-1]), EncodeValue(ordered[i])) >= 0 {
			t.Errorf("%v (%x) must encode before %v (%x)", ordered[i-1], EncodeValue(ordered[i-1]), ordered[i], EncodeValue(ordered[i]))
		}
	}
	key := EncodeValue(int32(7))
	for _, v := range ordered {
		key = append(key, EncodeValue(v)...)
	}
	if _, ok := SplitKeyValues(key, len(ordered)+1); !ok {
		t.Errorf("decimals with continuations do not split")
	}
}

// EncodeValue orders embedded fields canonically, as stored, except within
// _id; EncodeID keeps an _id's order.
func TestEncodeCanonicalFieldOrder(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	ba := doc("b", int32(1), "a", int32(2), "_id", doc("y", int32(1), "x", int32(2)))
	ab := doc("_id", doc("y", int32(1), "x", int32(2)), "a", int32(2), "b", int32(1))
	if !bytes.Equal(EncodeValue(ba), EncodeValue(ab)) {
		t.Errorf("EncodeValue depends on field order: %x vs %x", EncodeValue(ba), EncodeValue(ab))
	}
	if bytes.Equal(EncodeValue(ba), EncodeValue(doc("_id", doc("x", int32(2), "y", int32(1)), "a", int32(2), "b", int32(1)))) {
		t.Error("EncodeValue reordered fields within _id")
	}
	if bytes.Equal(EncodeID(ba), EncodeID(ab)) {
		t.Error("EncodeID reordered fields")
	}
}
