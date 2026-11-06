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

package aggregations_test

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/types"
)

func dec128(t *testing.T, s string) types.Decimal128 {
	t.Helper()

	p, err := bson.ParseDecimal128(s)
	if err != nil {
		t.Fatalf("ParseDecimal128(%q): %v", s, err)
	}

	h, l := p.GetBytes()

	return types.Decimal128{H: h, L: l}
}

// sameNumber reports whether two aggregation numeric results are equal,
// comparing Decimal128 by its bit representation and other numbers by ==.
func sameNumber(a, b any) bool {
	switch av := a.(type) {
	case types.Decimal128:
		bv, ok := b.(types.Decimal128)
		return ok && av.H == bv.H && av.L == bv.L
	default:
		return a == b
	}
}

// TestNumberSumIncrementalMatchesBatch verifies the incremental NumberSum
// produces the same result as the batch SumNumbers over the same sequence.
func TestNumberSumIncrementalMatchesBatch(t *testing.T) {
	cases := map[string][]any{
		"empty":            {},
		"ints":             {int32(1), int32(2), int32(3)},
		"int32 overflow":   {int32(2000000000), int32(2000000000)},
		"int64":            {int64(5), int32(7)},
		"floats":           {1.5, 2.25, -0.75},
		"int and float":    {int32(3), 4.5, int64(2)},
		"non-numeric skip": {int32(1), "x", true, 2.0},
		"single decimal":   {dec128(t, "42.1")},
		"int and decimal":  {int32(5), dec128(t, "2.5")},
		"single float+dec": {3.5, dec128(t, "1.25")},
	}

	for name, vs := range cases {
		t.Run(name, func(t *testing.T) {
			want := aggregations.SumNumbers(vs...)

			s := aggregations.NewNumberSum()
			for _, v := range vs {
				s.Add(v)
			}
			got := s.Result()

			if !sameNumber(got, want) {
				t.Errorf("NumberSum=%v (%T), SumNumbers=%v (%T)", got, got, want, want)
			}
		})
	}
}

// TestNumberAvgIncrementalMatchesBatch verifies the incremental NumberAvg
// produces the same result as the batch AvgNumbers over the same sequence.
func TestNumberAvgIncrementalMatchesBatch(t *testing.T) {
	cases := map[string][]any{
		"empty":           {},
		"no numeric":      {"x", true},
		"ints":            {int32(2), int32(4), int32(6)},
		"floats":          {1.0, 2.0, 3.0, 4.0},
		"int and float":   {int32(1), 2.0, int64(3)},
		"single decimal":  {dec128(t, "10")},
		"int and decimal": {int32(4), dec128(t, "2")},
	}

	for name, vs := range cases {
		t.Run(name, func(t *testing.T) {
			want := aggregations.AvgNumbers(vs...)

			a := aggregations.NewNumberAvg()
			for _, v := range vs {
				a.Add(v)
			}
			got := a.Result()

			if !sameNumber(got, want) {
				t.Errorf("NumberAvg=%v (%T), AvgNumbers=%v (%T)", got, got, want, want)
			}
		})
	}
}

// Doubles summed before a Decimal128 arrives carry their exact sum into it,
// as MongoDB's double-double sum does, so the order of values does not matter.
func TestNumberSumCarriesDoubleErrorIntoDecimal(t *testing.T) {
	want := dec128(t, "0.6000000000000000166533453693773481")
	for _, order := range [][]any{
		{0.1, 0.2, dec128(t, "0.3")},
		{dec128(t, "0.3"), 0.1, 0.2},
		{0.1, dec128(t, "0.3"), 0.2},
	} {
		s := aggregations.NewNumberSum()
		for _, v := range order {
			s.Add(v)
		}
		if got := s.Result(); !sameNumber(got, want) {
			t.Errorf("sum of %v = %v, want %v", order, got, want)
		}
	}
	a, b := 0.1, 0.2
	s := aggregations.NewNumberSum()
	s.Add(a)
	s.Add(b)
	if got := s.Result(); got != a+b {
		t.Errorf("double-only sum = %v, want %v", got, a+b)
	}

	// An exact total past 34 digits rounds instead of collapsing to zero.
	s = aggregations.NewNumberSum()
	s.Add(dec128(t, "1E20"))
	s.Add(dec128(t, "1E-20"))
	if got, want := s.Result(), dec128(t, "100000000000000000000.0000000000000"); !sameNumber(got, want) {
		t.Errorf("1E20 + 1E-20 = %v, want %v", got, want)
	}
}
