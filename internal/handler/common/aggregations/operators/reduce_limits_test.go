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

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// An accumulator that holds its depth long enough to back off mongod's
// unbounded interval must still be caught soon after it starts nesting.
func TestReduceLimiterCapsDepthCheckBackoff(t *testing.T) {
	limiter := newReduceLimiter()
	acc := any(types.MakeArray(0))

	for range 1 << 20 {
		require.NoError(t, limiter.check(acc))
	}

	for i := 0; ; i++ {
		acc = must.NotFail(types.NewArray(acc))
		if err := limiter.check(acc); err != nil {
			var cmdErr *handlererrors.CommandError
			require.ErrorAs(t, err, &cmdErr)
			require.Equal(t, handlererrors.ErrOverflow, cmdErr.Code())
			require.Less(t, i, maxReduceDepth+maxReduceDepthCheckInterval)
			return
		}
	}
}

func TestApproximateValueSizeStopsAtLimit(t *testing.T) {
	acc := any(must.NotFail(types.NewArray(int32(1))))
	for range 64 {
		acc = must.NotFail(types.NewArray(acc, acc))
	}
	require.Greater(t, approximateValueSize(acc, maxReduceBytes), maxReduceBytes)
}
