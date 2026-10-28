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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestAccumulatorExpressions(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }
	src := doc("_id", int32(1), "a", arr(int32(3), 1.5, int64(7), types.Null, "s"), "x", int32(4), "y", 2.5)
	nOp := func(op string, n, input any) *types.Document { return doc(op, doc("n", n, "input", input)) }
	for _, tc := range []struct {
		name string
		expr *types.Document
		want any
	}{
		{"$max array", doc("$max", "$a"), "s"},
		{"$max args", doc("$max", arr("$x", "$y", int32(3))), int32(4)},
		{"$max args keep arrays", doc("$max", arr("$x", arr(int32(1)))), arr(int32(1))},
		{"$max missing", doc("$max", "$nope"), types.Null},
		{"$min array", doc("$min", "$a"), 1.5},
		{"$min nulls only", doc("$min", arr(types.Null, "$nope")), types.Null},
		{"$stdDevPop", doc("$stdDevPop", arr(int32(2), int32(4))), 1.0},
		{"$stdDevSamp single", doc("$stdDevSamp", arr(int32(3))), types.Null},
		{"$maxN", nOp("$maxN", int32(2), "$a"), arr("s", int64(7))},
		{"$minN", nOp("$minN", int32(2), "$a"), arr(1.5, int32(3))},
		{"$firstN keeps nulls", nOp("$firstN", int32(5), "$a"), arr(int32(3), 1.5, int64(7), types.Null, "s")},
		{"$lastN", nOp("$lastN", int64(2), "$a"), arr(types.Null, "s")},
		{"$median", doc("$median", doc("input", "$a", "method", "approximate")), 3.0},
		{"$percentile", doc("$percentile", doc("input", arr("$x", "$y", int32(10)), "p", arr(0.0, 0.5, 1.0), "method", "approximate")), arr(2.5, 4.0, 10.0)},
		{"$stdDevPop decimal", doc("$stdDevPop", arr(types.Decimal128{H: 0x3040000000000000, L: 4}, int32(2))), 1.0},
		{"$median decimal", doc("$median", doc("input", arr(types.Decimal128{H: 0x3040000000000000, L: 4}, int32(2)), "method", "approximate")), 2.0},
		{"$maxN decimal n", nOp("$maxN", types.Decimal128{H: 0x3040000000000000, L: 1}, "$a"), arr("s")},
		{"$percentile empty", doc("$percentile", doc("input", arr(), "p", arr(0.5), "method", "approximate")), arr(types.Null)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op, err := NewOperator(tc.expr)
			require.NoError(t, err)
			got, err := op.Process(src)
			require.NoError(t, err)
			require.Equal(t, types.Equal, types.Compare(tc.want, got), "got %v", got)
		})
	}

	for _, tc := range []struct {
		name string
		expr *types.Document
		msg  string
	}{
		{"input not an array", nOp("$maxN", int32(1), "$x"), "Input must be an array"},
		{"zero n", nOp("$maxN", int32(0), "$a"), "'n' must be greater than 0, found 0"},
		{"fractional n", nOp("$minN", 1.5, "$a"), "Value for 'n' must be of integral type, but found 1.5"},
		{"null n", nOp("$firstN", types.Null, "$a"), "Value for 'n' must be of integral type, but found null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op, err := NewOperator(tc.expr)
			require.NoError(t, err)
			_, err = op.Process(src)
			require.ErrorContains(t, err, tc.msg)
		})
	}

	_, err := NewOperator(doc("$median", doc("input", "$a", "method", "exact")))
	require.ErrorContains(t, err, "Currently only 'approximate' can be used as percentile 'method'.")
}
