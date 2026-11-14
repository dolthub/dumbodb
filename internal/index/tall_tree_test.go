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
	"fmt"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"
	"github.com/stretchr/testify/require"
)

// TestTallTree writes keys far over MaxKeyLength, which Dolt arranges into a
// nearly binary tree, past the 15 levels Dolt once allowed.
func TestTallTree(t *testing.T) {
	ctx := context.Background()
	m, err := NewEmptyMap(ctx, tree.NewTestNodeStore())
	require.NoError(t, err)

	pad := strings.Repeat("x", 3000)
	kb := val.NewTupleBuilder(idxKeyDesc, nil)
	vb := val.NewTupleBuilder(idxValDesc, nil)
	const batch = 1000
	const count = 40 * batch
	for start := 0; start < count; start += batch {
		mut := m.Mutate()
		for i := start; i < start+batch; i++ {
			kb.PutByteString(0, []byte(fmt.Sprintf("%08d", i)+pad))
			key, err := kb.Build(ctx, idxBufPool)
			require.NoError(t, err)
			vb.PutByteString(0, []byte{0})
			value, err := vb.Build(ctx, idxBufPool)
			require.NoError(t, err)
			require.NoError(t, mut.Put(ctx, key, value))
		}
		m, err = mut.Map(ctx)
		require.NoError(t, err)
	}
	require.Greater(t, m.Height(), 15)
	n, err := m.Count()
	require.NoError(t, err)
	require.Equal(t, count, n)
}
