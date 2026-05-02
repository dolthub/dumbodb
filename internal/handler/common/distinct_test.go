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

package common

import (
	"fmt"
	"testing"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestDistinctSet_NumericEquality verifies that int32, int64, and float64
// values that are numerically equal collapse to a single distinct entry,
// matching MongoDB's distinct semantics.
func TestDistinctSet_NumericEquality(t *testing.T) {
	t.Parallel()

	s := newDistinctSet()
	s.add(int32(5))
	s.add(int64(5))
	s.add(float64(5.0))
	s.add(float64(5.5))
	s.add(int32(7))
	s.add(int64(7))

	arr := s.array()
	if arr.Len() != 3 {
		t.Fatalf("expected 3 distinct values, got %d (%v)", arr.Len(), arr)
	}
}

// TestDistinctSet_TypeBuckets verifies that values of different types with
// otherwise-equal scalar payloads (e.g. string "5" vs int 5) stay distinct.
func TestDistinctSet_TypeBuckets(t *testing.T) {
	t.Parallel()

	s := newDistinctSet()
	s.add(int32(5))
	s.add("5")
	s.add(true)
	s.add(false)
	s.add(types.Null)
	s.add(nil)
	s.add(types.MinKey)
	s.add(types.MaxKey)

	// nil and types.Null collapse; "5" / int 5 / true / false / min / max
	// each produce their own bucket  -- 7 entries total.
	if got, want := s.array().Len(), 7; got != want {
		t.Fatalf("expected %d distinct, got %d", want, got)
	}
}

// TestDistinctSet_Decimal128 verifies the Decimal128 carve-out: convertible
// decimals dedup by numeric value within the Decimal128 bucket but never
// against int / float of the same magnitude.
func TestDistinctSet_Decimal128(t *testing.T) {
	t.Parallel()

	d42a := types.Decimal128{H: 0x3040000000000000, L: 0x000000000000002a} // 42
	d42b := types.Decimal128{H: 0x303e000000000000, L: 0x00000000000001a4} // 42.0 (different bits, same value)

	s := newDistinctSet()
	s.add(d42a)
	s.add(d42b)
	s.add(int32(42))

	// Decimal128 collapses to one entry; int32(42) is its own bucket.
	if got, want := s.array().Len(), 2; got != want {
		t.Fatalf("expected %d distinct, got %d (%v)", want, got, s.array())
	}
}

// TestDistinctSet_Composite verifies that arrays and documents fall through to
// the structural-compare slow path and dedup correctly.
func TestDistinctSet_Composite(t *testing.T) {
	t.Parallel()

	s := newDistinctSet()
	s.add(must.NotFail(types.NewArray(int32(1), int32(2))))
	s.add(must.NotFail(types.NewArray(int32(1), int32(2))))
	s.add(must.NotFail(types.NewArray(int32(1), int32(3))))

	if got, want := s.array().Len(), 2; got != want {
		t.Fatalf("expected %d distinct arrays, got %d", want, got)
	}
}

// BenchmarkDistinctSet_HighDup exercises the hot path the bead targets: a
// large stream of values where most repeat. Validates that the new
// hash-keyed accumulator scales linearly rather than the prior O(n²) growth.
func BenchmarkDistinctSet_HighDup(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			vals := make([]any, n)
			for i := range vals {
				vals[i] = int32(i % 100) // 100 unique values
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s := newDistinctSet()
				for _, v := range vals {
					s.add(v)
				}
				_ = s.array()
			}
		})
	}
}

// BenchmarkDistinctSet_Strings benchmarks string-valued distinct dedup, which
// goes through encodeString in distinctKey.
func BenchmarkDistinctSet_Strings(b *testing.B) {
	vals := make([]any, 50000)
	for i := range vals {
		vals[i] = fmt.Sprintf("user-%d", i%500)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := newDistinctSet()
		for _, v := range vals {
			s.add(v)
		}
		_ = s.array()
	}
}

// BenchmarkDistinctSet_AllUnique stresses the worst case for the prior O(n²)
// linear-scan accumulator: every value is unique so the prior code paid
// arr.Contains(v) over every prior entry. Linear-scaling time here is the
// real win the bead is after.
func BenchmarkDistinctSet_AllUnique(b *testing.B) {
	for _, n := range []int{1000, 10000, 50000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			vals := make([]any, n)
			for i := range vals {
				vals[i] = int64(i)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s := newDistinctSet()
				for _, v := range vals {
					s.add(v)
				}
				_ = s.array()
			}
		})
	}
}

// TestDistinctSet_DateAndOID smoke-tests that types with structured byte
// representations dedup correctly.
func TestDistinctSet_DateAndOID(t *testing.T) {
	t.Parallel()

	t1 := time.Unix(1000, 0).UTC()
	t2 := time.Unix(1000, 500).UTC() // same UnixMilli rounding

	oid1 := types.ObjectID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	oid2 := types.ObjectID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 13}

	s := newDistinctSet()
	s.add(t1)
	s.add(t2)
	s.add(oid1)
	s.add(oid2)

	// time.Time dedups by UnixMilli (1000000 == 1000000), ObjectIDs differ.
	if got, want := s.array().Len(), 3; got != want {
		t.Fatalf("expected %d distinct, got %d", want, got)
	}
}
