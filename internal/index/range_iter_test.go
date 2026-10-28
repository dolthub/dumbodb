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
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/stretchr/testify/require"
)

func TestRangeIDIterMatchesRangeLookup(t *testing.T) {
	ctx := context.Background()
	m, err := NewEmptyMap(ctx, tree.NewTestNodeStore())
	require.NoError(t, err)
	mut := m.Mutate()
	for i := 0; i < 3000; i++ {
		id := make([]byte, 20)
		binary.BigEndian.PutUint32(id, uint32(i))
		require.NoError(t, InsertEntry(ctx, mut, []any{int32(i % 7)}, id))
	}
	m, err = mut.Map(ctx)
	require.NoError(t, err)

	for _, tc := range []struct {
		name        string
		start, stop []byte
	}{
		{"one value", LowerBoundInclusive(int32(3)), UpperBoundInclusive(int32(3))},
		{"first value", LowerBoundInclusive(int32(0)), UpperBoundInclusive(int32(0))},
		{"last value", LowerBoundInclusive(int32(6)), UpperBoundInclusive(int32(6))},
		{"several values", LowerBoundExclusive(int32(1)), UpperBoundExclusive(int32(5))},
		{"open start", nil, UpperBoundInclusive(int32(2))},
		{"open stop", LowerBoundInclusive(int32(5)), nil},
		{"everything", nil, nil},
		{"empty", LowerBoundInclusive(int32(10)), UpperBoundInclusive(int32(10))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := RangeLookup(ctx, m, tc.start, tc.stop)
			require.NoError(t, err)
			for _, reverse := range []bool{false, true} {
				it, err := NewRangeIDIter(ctx, m, tc.start, tc.stop, reverse)
				require.NoError(t, err)
				var got [][]byte
				for {
					id, err := it.Next(ctx)
					require.NoError(t, err)
					if id == nil {
						break
					}
					got = append(got, id)
				}
				expected := slices.Clone(want)
				if reverse {
					slices.Reverse(expected)
				}
				require.Equal(t, len(expected), len(got), "reverse=%v", reverse)
				for i := range expected {
					require.Equal(t, expected[i], got[i], "reverse=%v at %d", reverse, i)
				}
			}
		})
	}
}
