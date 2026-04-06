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

package operators

import (
	"math"
	"testing"

	"github.com/dolthub/docudolt/internal/util/must"

	"github.com/dolthub/docudolt/internal/types"
)

// TestProbeModNaNDivisor is a regression test for do-9ni / do-sl4f.
// When $mod is used with a NaN divisor, docudolt must return NaN (matching MongoDB),
// not crash or return an error.
func TestProbeModNaNDivisor(t *testing.T) {
	t.Parallel()

	op, err := newMod(float64(10), math.NaN())
	if err != nil {
		t.Fatalf("newMod: unexpected error: %v", err)
	}

	doc := must.NotFail(types.NewDocument("x", float64(10)))

	result, err := op.Process(doc)
	if err != nil {
		t.Fatalf("$mod with NaN divisor: expected no error, got: %v", err)
	}

	f, ok := result.(float64)
	if !ok {
		t.Fatalf("$mod with NaN divisor: expected float64 result, got %T", result)
	}

	if !math.IsNaN(f) {
		t.Errorf("$mod with NaN divisor: expected NaN result, got %v", f)
	}
}
