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
	"testing"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
)

// BenchmarkEncodeTinyDecimal encodes an inexact 34-digit decimal near 1e-297,
// whose exponent arithmetic once cost milliseconds per value.
func BenchmarkEncodeTinyDecimal(b *testing.B) {
	d := types.Decimal128{H: uint64(6176-330)<<49 | 0x0001ED09BEAD87C0, L: 0x378D8E63FFFFFFFF}
	for i := 0; i < b.N; i++ {
		EncodeID(d)
	}
}

// TestEncodeExtremeDecimalIsCheap: a decimal beyond the double range, down to
// the smallest exponent, encodes faster than one inside it, without arithmetic
// on its full value, which once cost several times as much.
func TestEncodeExtremeDecimalIsCheap(t *testing.T) {
	const n = 1000
	cost := func(biased uint64) time.Duration {
		d := types.Decimal128{H: biased<<49 | 0x0001ED09BEAD87C0, L: 0x378D8E63FFFFFFFF}
		start := time.Now()
		for i := 0; i < n; i++ {
			EncodeID(d)
		}
		return time.Since(start)
	}
	inRange := cost(6176 - 330)
	for _, biased := range []uint64{0, 78, 6176 + 6111} {
		if extreme := cost(biased); extreme > inRange {
			t.Errorf("biased exponent %d: %v for %d encodes, more than %v in the double range", biased, extreme, n, inRange)
		}
	}
}
