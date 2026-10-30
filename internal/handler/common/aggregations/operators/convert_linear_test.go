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
	"strings"
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lineartest"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Numeric strings parse in linear time (workspace-8tj.16, workspace-8tj.17).
func TestNumericStringParsingIsLinear(t *testing.T) {
	t.Parallel()

	inputs := map[string]func(n int) string{
		"Sevens":          func(n int) string { return strings.Repeat("7", n) },
		"LeadingZeros":    func(n int) string { return strings.Repeat("0", n) + "1" },
		"Fraction":        func(n int) string { return "0." + strings.Repeat("7", n) },
		"LongExponent":    func(n int) string { return "1e" + strings.Repeat("9", n) },
		"SevensThenPoint": func(n int) string { return strings.Repeat("7", n) + ".5e-3" },
	}
	parsers := map[string]func(s string){
		"Long":    func(s string) { _, _ = parseIntegerString(s, -1<<63, 1<<63-1) },
		"Decimal": func(s string) { _, _ = parseDecimalString(s) },
		"Double":  func(s string) { _, _ = parseDoubleString(s) },
	}
	for parserName, parse := range parsers {
		for inputName, input := range inputs {
			t.Run(parserName+"/"+inputName, func(t *testing.T) {
				t.Parallel()

				lineartest.Assert(t, 10000, func(n int) { parse(input(n)) })
			})
		}
	}
}

// Constant folding evaluates each operator once (workspace-8tj.20).
func TestConstantFoldingIsLinear(t *testing.T) {
	t.Parallel()

	chain := func(n int) any {
		expr := any(int32(1))
		for range n {
			expr = must.NotFail(types.NewDocument("$add", must.NotFail(types.NewArray(int32(1), expr))))
		}
		return expr
	}
	lineartest.Assert(t, 500, func(n int) { _ = CheckConstantSubexpressions(chain(n)) })
}
