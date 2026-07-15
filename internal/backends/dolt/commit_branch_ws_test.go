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
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dolt/go/store/datas"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

func headMeta(t *testing.T, state *dbState) *datas.CommitMeta {
	t.Helper()
	headVal, ok := mainDS(t, state).MaybeHead()
	require.True(t, ok, "branch has no HEAD")
	meta, err := datas.GetCommitMeta(context.Background(), headVal)
	require.NoError(t, err)
	return meta
}

// TestCommitBranchWS: commit the working root once, leave a clean tree, no-op when root==HEAD.
func TestCommitBranchWS(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-commit-branch-ws-*")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	ctx := context.Background()
	b := &Backend{
		dataDir: dir,
		l:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		dbs:     make(map[string]*dbState),
	}
	defer b.Close()

	state, err := b.getOrOpenDB(ctx, "testdb", true)
	require.NoError(t, err)
	initAddr, ok := mainDS(t, state).MaybeHeadAddr()
	require.True(t, ok, "no HEAD after init")

	db, err := b.Database("testdb")
	require.NoError(t, err)
	coll, err := db.Collection("col")
	require.NoError(t, err)
	doc, err := types.NewDocument("_id", int64(1), "x", int64(42))
	require.NoError(t, err)
	doc.SetRecordID(1)
	_, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}})
	require.NoError(t, err)

	state.mu.RLock()
	committed, err := state.commitBranchWS(ctx, defaultBranch, "auto: insert 1 doc into col")
	state.mu.RUnlock()
	require.NoError(t, err)
	require.True(t, committed, "expected a commit for a dirty working set")

	afterAddr, ok := mainDS(t, state).MaybeHeadAddr()
	require.True(t, ok)
	require.NotEqual(t, initAddr, afterAddr, "HEAD must advance")
	require.Equal(t, "auto: insert 1 doc into col", headMeta(t, state).Description)

	ws, err := state.loadBranchWS(ctx, defaultBranch)
	require.NoError(t, err)
	wHash, err := ws.WorkingRoot().HashOf()
	require.NoError(t, err)
	headRoot, err := headRootValueForBranch(ctx, state, defaultBranch)
	require.NoError(t, err)
	hHash, err := headRoot.HashOf()
	require.NoError(t, err)
	require.Equal(t, hHash, wHash, "working root must equal HEAD after commit")

	state.mu.RLock()
	committed, err = state.commitBranchWS(ctx, defaultBranch, "should not happen")
	state.mu.RUnlock()
	require.NoError(t, err)
	require.False(t, committed, "clean tree must not produce a commit")

	guardAddr, ok := mainDS(t, state).MaybeHeadAddr()
	require.True(t, ok)
	require.Equal(t, afterAddr, guardAddr, "HEAD must not move on a no-op commit")
}
