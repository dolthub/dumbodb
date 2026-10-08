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

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func decimal(t *testing.T, s string) types.Decimal128 {
	t.Helper()
	d, err := bson.ParseDecimal128(s)
	require.NoError(t, err)
	h, l := d.GetBytes()
	return types.Decimal128{H: h, L: l}
}

// Before the fix arguments of any other type evaluated as 0, so
// {$range: ["bad", 3]} returned [0, 1, 2] and Decimal128 values silently
// became 0. Expected results and codes match mongod 8.0.28.
func TestRangeArgumentTypes(t *testing.T) {
	for _, c := range []struct {
		args []any
		code handlererrors.ErrorCode
	}{
		{[]any{"a", int32(3)}, handlererrors.ErrRangeStartNotNumeric},
		{[]any{int32(0), "b"}, handlererrors.ErrRangeEndNotNumeric},
		{[]any{int32(0), int32(3), "s"}, handlererrors.ErrRangeStepNotNumeric},
		{[]any{types.Null, int32(3)}, handlererrors.ErrRangeStartNotNumeric},
		{[]any{int32(0), types.Null}, handlererrors.ErrRangeEndNotNumeric},
		{[]any{int32(0), int32(3), types.Null}, handlererrors.ErrRangeStepNotNumeric},
		{[]any{true, int32(3)}, handlererrors.ErrRangeStartNotNumeric},
		{[]any{must.NotFail(types.NewDocument("$literal", types.MakeDocument(0))), int32(3)}, handlererrors.ErrRangeStartNotNumeric},
		{[]any{decimal(t, "1.5"), int32(3)}, handlererrors.ErrRangeStartNotInt32},
		{[]any{int32(0), decimal(t, "1E+20")}, handlererrors.ErrRangeEndNotInt32},
		{[]any{int32(0), int32(3), decimal(t, "NaN")}, handlererrors.ErrRangeStepNotInt32},
		{[]any{int32(0), int32(3), int32(0)}, handlererrors.ErrRangeStepZero},
		{[]any{math.NaN(), int32(3)}, handlererrors.ErrRangeStartNotInt32},
	} {
		_, err := processRange(t, c.args...)
		requireCode(t, err, c.code)
	}

	res, err := processRange(t, decimal(t, "1"), int32(3))
	require.NoError(t, err)
	require.Equal(t, []any{int32(1), int32(2)}, arrayValues(res.(*types.Array)))

	res, err = processRange(t, int32(0), int32(10), decimal(t, "2"))
	require.NoError(t, err)
	require.Equal(t, []any{int32(0), int32(2), int32(4), int32(6), int32(8)}, arrayValues(res.(*types.Array)))

	res, err = processRange(t, int64(1), int32(3))
	require.NoError(t, err)
	require.Equal(t, []any{int32(1), int32(2)}, arrayValues(res.(*types.Array)))
}
