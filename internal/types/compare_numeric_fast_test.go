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

package types

import (
	"fmt"
	"math"
	"testing"
)

// The fast path must agree with the exact rational comparison everywhere it
// answers.
func TestCompareNumericFast_MatchesExactComparison(t *testing.T) {
	const p53 = int64(1) << 53
	values := []any{
		int32(0), int32(1), int32(-1), int32(math.MaxInt32), int32(math.MinInt32),
		int64(0), int64(1), int64(-1), int64(math.MaxInt64), int64(math.MinInt64),
		p53 - 1, p53, p53 + 1, -p53 + 1, -p53, -p53 - 1,
		0.0, math.Copysign(0, -1), 1.0, -1.0, 0.5, -0.5,
		float64(p53 - 1), float64(p53), float64(p53) + 2, -float64(p53),
		float64(math.MaxInt64), float64(math.MinInt64),
		math.SmallestNonzeroFloat64, math.MaxFloat64, -math.MaxFloat64,
		math.Inf(1), math.Inf(-1), math.NaN(),
	}

	exact := func(a, b any) CompareResult {
		c1, r1, _ := numericParts(a)
		c2, r2, _ := numericParts(b)
		if c1 != c2 {
			return compareOrdered(c1, c2)
		}
		if c1 == numFinite {
			return CompareResult(r1.Cmp(r2))
		}
		return Equal
	}

	for _, a := range values {
		for _, b := range values {
			got, ok := compareNumericFast(a, b)
			if !ok {
				continue
			}
			if want := exact(a, b); got != want {
				t.Errorf("compareNumericFast(%s, %s) = %v, want %v", describe(a), describe(b), got, want)
			}
		}
	}
}

func describe(v any) string { return fmt.Sprintf("%T(%v)", v, v) }
