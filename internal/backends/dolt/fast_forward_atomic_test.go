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
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dolt/go/libraries/doltcore/ref"

	"github.com/dolthub/dumbodb/internal/backends"
)

func TestFastForwardRefusesDirtyTargetWithoutChangingHeadOrWorkingSet(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	insertDocForTest(t, ctx, b, "testdb", 1)
	commitBranch(t, b, "testdb", "main", "baseline")
	branchFrom(t, b, "testdb", "main", "feature")

	insertDocForTest(t, ctx, b, "testdb@feature", 2)
	commitBranch(t, b, "testdb", "feature", "advance feature")

	insertDocForTest(t, ctx, b, "testdb", 3)
	state, err := b.getOrOpenDB(ctx, "testdb", false)
	require.NoError(t, err)
	beforeHead, ok := mainDS(t, state).MaybeHeadAddr()
	require.True(t, ok)
	wsRef := ref.NewWorkingSetRef("heads/main")
	beforeWS, err := state.doltDB.ResolveWorkingSet(ctx, wsRef)
	require.NoError(t, err)
	beforeWSHash, err := beforeWS.HashOf()
	require.NoError(t, err)

	_, err = b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: "testdb",
		Into:   "main",
		From:   "feature",
	})
	require.Error(t, err)

	afterHead, ok := mainDS(t, state).MaybeHeadAddr()
	require.True(t, ok)
	require.Equal(t, beforeHead, afterHead)
	afterWS, err := state.doltDB.ResolveWorkingSet(ctx, wsRef)
	require.NoError(t, err)
	afterWSHash, err := afterWS.HashOf()
	require.NoError(t, err)
	require.Equal(t, beforeWSHash, afterWSHash)
}
