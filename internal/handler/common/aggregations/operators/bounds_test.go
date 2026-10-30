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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// nearMaxInt is the largest float64 below 2^63: it converts to a valid int,
// and adding a start above 1023 to it overflows int.
const nearMaxInt = float64(math.MaxInt64 - 1023)

func processOperator(t *testing.T, newOp func(...any) (Operator, error), args ...any) (any, error) {
	t.Helper()
	op, err := newOp(args...)
	require.NoError(t, err)
	var res any
	require.NotPanics(t, func() { res, err = op.Process(types.MakeDocument(0)) })
	return res, err
}

// Before the fix start+length overflowed to a negative end, which passed the
// "end > n" clamp and panicked slicing the input.
func TestSubstringOperatorsSurviveExtremeArguments(t *testing.T) {
	for name, newOp := range map[string]func(...any) (Operator, error){
		"$substr":      newSubstr,
		"$substrBytes": newSubstrBytesOp,
		"$substrCP":    newSubstrCP,
	} {
		t.Run(name, func(t *testing.T) {
			res, err := processOperator(t, newOp, strings.Repeat("x", 2000)+"hello", int32(2000), nearMaxInt)
			if name == "$substrCP" {
				// Its length must fit in 32 bits, as in MongoDB.
				requireCode(t, err, handlererrors.ErrSubstrCPLengthNotInt)
			} else {
				require.NoError(t, err)
				require.Equal(t, "hello", res)
			}

			for _, extreme := range []any{math.Inf(1), math.Inf(-1), math.NaN(), 1e300, -1e300, int64(math.MaxInt64), int64(math.MinInt64)} {
				_, _ = processOperator(t, newOp, "hello", extreme, int32(1))
				_, _ = processOperator(t, newOp, "hello", int32(1), extreme)
			}
		})
	}
}

func TestSliceSurvivesExtremeArguments(t *testing.T) {
	items := func() *types.Array { return must.NotFail(types.NewArray(int32(1), int32(2), int32(3))) }

	long := types.MakeArray(2003)
	for i := range 2003 {
		long.Append(int32(i))
	}
	res, err := processOperator(t, newSlice, long, int32(2000), int32(math.MaxInt32))
	require.NoError(t, err)
	require.Equal(t, []any{int32(2000), int32(2001), int32(2002)}, arrayValues(res.(*types.Array)))

	// A count that does not fit in 32 bits is rejected, as in MongoDB.
	_, err = processOperator(t, newSlice, long, int32(2000), nearMaxInt)
	requireCode(t, err, handlererrors.ErrSliceThirdNotInt)

	_, err = processOperator(t, newSlice, items(), int32(1), int32(-1))
	requireCode(t, err, handlererrors.ErrSliceThirdArgNotPositive)

	for _, extreme := range []any{math.Inf(1), math.Inf(-1), math.NaN(), 1e300, -1e300, int64(math.MaxInt64), int64(math.MinInt64)} {
		_, _ = processOperator(t, newSlice, items(), extreme)
		_, _ = processOperator(t, newSlice, items(), extreme, int32(1))
		_, _ = processOperator(t, newSlice, items(), int32(1), extreme)
	}
}
