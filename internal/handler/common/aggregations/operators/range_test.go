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
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

func processRange(t *testing.T, args ...any) (any, error) {
	t.Helper()
	op, err := newRange(args...)
	require.NoError(t, err)
	return op.Process(types.MakeDocument(0))
}

func requireCode(t *testing.T, err error, code handlererrors.ErrorCode) {
	t.Helper()
	require.Error(t, err)
	var cmdErr *handlererrors.CommandError
	require.True(t, errors.As(err, &cmdErr), "expected a command error, got %T: %v", err, err)
	require.Equal(t, code, cmdErr.Code(), "%v", err)
}

func arrayValues(a *types.Array) []any {
	values := make([]any, a.Len())
	for i := range values {
		values[i], _ = a.Get(i)
	}
	return values
}

func TestRangeProducesExpectedValues(t *testing.T) {
	res, err := processRange(t, int32(0), int32(5), int32(2))
	require.NoError(t, err)
	require.Equal(t, []any{int32(0), int32(2), int32(4)}, arrayValues(res.(*types.Array)))

	res, err = processRange(t, int32(3), int32(0), int32(-1))
	require.NoError(t, err)
	require.Equal(t, []any{int32(3), int32(2), int32(1)}, arrayValues(res.(*types.Array)))
}

// A single tiny expression must not be able to allocate unbounded memory: an
// out-of-memory condition is fatal to the whole server, not just the request.
func TestRangeRejectsOutputOverMemoryLimit(t *testing.T) {
	_, err := processRange(t, int32(0), int32(10_000_000))
	requireCode(t, err, handlererrors.ErrExceededMemoryLimit)

	_, err = processRange(t, int32(math.MaxInt32), int32(math.MinInt32), int32(-1))
	requireCode(t, err, handlererrors.ErrExceededMemoryLimit)
}

func TestRangeRequiresInt32RepresentableArguments(t *testing.T) {
	_, err := processRange(t, int64(1)<<40, int32(0))
	requireCode(t, err, handlererrors.ErrRangeStartNotInt32)

	_, err = processRange(t, int32(0), float64(1<<40))
	requireCode(t, err, handlererrors.ErrRangeEndNotInt32)

	_, err = processRange(t, int32(0), int32(10), int64(math.MaxInt64))
	requireCode(t, err, handlererrors.ErrRangeStepNotInt32)

	_, err = processRange(t, int32(0), float64(1.5))
	requireCode(t, err, handlererrors.ErrRangeEndNotInt32)
}
