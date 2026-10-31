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

// Package lineartest checks that a function's running time grows linearly
// with its input size, independent of machine speed or the race detector.
package lineartest

import (
	"testing"
	"time"
)

// minBaseline is the shortest base run the ratio is trusted on.
const minBaseline = 20 * time.Millisecond

// maxRatio separates linear growth (4x for 4x the input) from quadratic (16x).
const maxRatio = 8.0

// Assert fails t when run(4n) takes more than maxRatio times as long as
// run(n). n is doubled first until run(n) takes at least minBaseline.
func Assert(t testing.TB, n int, run func(n int)) {
	t.Helper()

	base := fastest(n, run)
	for base < minBaseline {
		n *= 2
		base = fastest(n, run)
	}

	quadrupled := fastest(4*n, run)
	if ratio := float64(quadrupled) / float64(base); ratio > maxRatio {
		t.Errorf("4x input (n=%d) took %.1fx as long (%v vs %v); want about 4x", n, ratio, quadrupled, base)
	}
}

// fastest returns the best of three runs, to discount scheduler noise.
func fastest(n int, run func(n int)) time.Duration {
	best := time.Duration(1<<63 - 1)
	for range 3 {
		start := time.Now()
		run(n)
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return best
}
