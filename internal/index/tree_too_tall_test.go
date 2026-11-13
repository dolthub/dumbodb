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

// TestTreeTooTall writes keys far over MaxKeyLength, which Dolt arranges into
// a binary tree, until the tree is too tall to grow.
func TestTreeTooTall(t *testing.T) {
	ctx := context.Background()
	m, err := NewEmptyMap(ctx, tree.NewTestNodeStore())
	require.NoError(t, err)

	pad := strings.Repeat("x", 3000)
	kb := val.NewTupleBuilder(idxKeyDesc, nil)
	vb := val.NewTupleBuilder(idxValDesc, nil)
	const batch = 1000
	for start := 0; start < 100*batch; start += batch {
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
		if m, err = mut.Map(ctx); err != nil {
			require.ErrorIs(t, err, ErrTreeTooTall)
			return
		}
	}
	t.Fatalf("tree reached %d levels without passing the limit", m.Height())
}
