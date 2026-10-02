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

	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/stretchr/testify/require"
)

func TestInitializeBranchWorkingSet(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1)))
	commitDB(t, b, "testdb", "base")
	state, ok := b.lookupDbStateForDsess("testdb")
	require.True(t, ok)

	require.Error(t, initializeBranchWorkingSet(ctx, state.doltDB, "no-such-branch"),
		"a failure to resolve the branch head must be reported, not swallowed")

	branchFrom(t, b, "testdb", "main", "feat")
	ws, err := state.doltDB.ResolveWorkingSet(ctx, doltref.NewWorkingSetRef("heads/feat"))
	require.NoError(t, err)
	head, err := state.doltDB.ResolveCommitRef(ctx, doltref.NewBranchRef("feat"))
	require.NoError(t, err)
	headRoot, err := head.GetRootValue(ctx)
	require.NoError(t, err)
	wantHash, err := headRoot.HashOf()
	require.NoError(t, err)
	gotHash, err := ws.WorkingRoot().HashOf()
	require.NoError(t, err)
	require.Equal(t, wantHash, gotHash)
}
