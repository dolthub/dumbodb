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

package dolt

import (
	"bytes"
	"context"
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/stretchr/testify/require"
)

func TestBuildValueReusingPriorMatchesFreshBuild(t *testing.T) {
	ctx := context.Background()
	ns := tree.NewTestNodeStore()

	old := bytes.Repeat([]byte("0123456789"), 100_000)
	oldValue, err := buildValue(ctx, ns, old, hash.Hash{})
	require.NoError(t, err)
	prior := storedBlobAddr(ctx, ns, oldValue)
	require.False(t, prior.IsEmpty(), "a 1MB document is stored out-of-band")

	updated := bytes.Clone(old)
	copy(updated[999_000:], "changed")
	reused, err := buildValue(ctx, ns, updated, prior)
	require.NoError(t, err)
	fresh, err := buildValue(ctx, ns, updated, hash.Hash{})
	require.NoError(t, err)
	require.Equal(t, fresh, reused)

	small, err := buildValue(ctx, ns, []byte("small"), hash.Hash{})
	require.NoError(t, err)
	require.True(t, storedBlobAddr(ctx, ns, small).IsEmpty(), "a small document is stored inline")
}
