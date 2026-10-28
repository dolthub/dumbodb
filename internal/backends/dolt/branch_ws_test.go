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
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/prolly"
	dolttypes "github.com/dolthub/dolt/go/store/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestDbState(t *testing.T) *dbState {
	t.Helper()
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	state, err := be.getOrOpenDB(context.Background(), "test", true)
	require.NoError(t, err)
	return state
}

// mutateWS produces a new WorkingSet whose chunk hash differs from
// the input by replacing its working root with a freshly-built
// RootValue that holds a unique tagged AddressMap entry. The entry's
// value points at the database's schema chunk (a known-reachable
// hash) so the chunk store's dangling-ref check passes. Used by the
// tests to advance the on-disk WS without exercising the full
// insert/commit machinery.
func mutateWS(t *testing.T, state *dbState, cur *doltdb.WorkingSet, tag string) *doltdb.WorkingSet {
	t.Helper()
	ctx := context.Background()
	require.False(t, state.collSchemaHash.IsEmpty(), "test pre-condition: schema chunk must be written")
	am, err := prolly.NewEmptyAddressMap(state.ns)
	require.NoError(t, err)
	ed := am.Editor()
	require.NoError(t, ed.Add(ctx, "ws-test-"+tag, state.collSchemaHash))
	am, err = ed.Flush(ctx)
	require.NoError(t, err)
	rtvlMsg := buildRootValueFlatbuffer(am)
	rv, err := doltdb.NewRootValue(ctx, state.doltDB.ValueReadWriter(), state.doltDB.NodeStore(), dolttypes.SerialMessage(rtvlMsg))
	require.NoError(t, err)
	return cur.WithWorkingRoot(rv)
}

// TestBranchEntry_Singleton: the entry pointer returned for a given
// branch is stable across calls. Concurrent first-touch callers all
// see the same entry.
func TestBranchEntry_Singleton(t *testing.T) {
	state := newTestDbState(t)

	e1 := state.branchEntry("main")
	e2 := state.branchEntry("main")
	assert.Same(t, e1, e2, "branchEntry must return the same pointer for the same branch")

	// Different branches get different entries.
	feat := state.branchEntry("feat")
	assert.NotSame(t, e1, feat, "different branches must get distinct entries")

	// Concurrent first-touch on a new branch.
	const goroutines = 16
	var wg sync.WaitGroup
	results := make([]*branchWS, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			results[idx] = state.branchEntry("concurrent")
		}(i)
	}
	wg.Wait()
	for i := 1; i < goroutines; i++ {
		assert.Same(t, results[0], results[i], "concurrent first-touch must agree on the entry pointer")
	}
}

// TestLoadBranchWS_LazyLoad: a cold entry resolves the WS from
// doltDB on first call; the post-load wsHash matches HashOf of the
// loaded WS; subsequent calls hit the cached entry.
func TestLoadBranchWS_LazyLoad(t *testing.T) {
	ctx := context.Background()
	state := newTestDbState(t)

	e := state.branchEntry(defaultBranch)
	e.mu.RLock()
	require.Nil(t, e.ws, "entry must start cold")
	e.mu.RUnlock()

	ws1, err := state.loadBranchWS(ctx, defaultBranch)
	require.NoError(t, err)
	require.NotNil(t, ws1)

	e.mu.RLock()
	assert.Same(t, ws1, e.ws, "entry.ws must point at the loaded WS")
	loadedHash := e.wsHash
	e.mu.RUnlock()

	directHash, err := ws1.HashOf()
	require.NoError(t, err)
	assert.Equal(t, directHash, loadedHash, "entry.wsHash must equal HashOf of the loaded WS")

	ws2, err := state.loadBranchWS(ctx, defaultBranch)
	require.NoError(t, err)
	assert.Same(t, ws1, ws2, "second call must hit the cache and return the same WS pointer")
}

// makeBranch creates a branch named name from the current HEAD of
// defaultBranch. Used by the contention test so each branch has an
// on-disk working_set ref.
func makeBranch(ctx context.Context, state *dbState, name string) error {
	// The dolt branch-create path goes through DumboDBBranch on the
	// backend, but for unit tests we can construct the WS ref directly
	// off HEAD: ResolveWorkingSet for the default branch, then write
	// the same WS under the new branch's ref.
	mainRef := doltref.NewWorkingSetRef("heads/" + defaultBranch)
	mainWS, err := state.doltDB.ResolveWorkingSet(ctx, mainRef)
	if err != nil {
		return err
	}
	newRef := doltref.NewWorkingSetRef("heads/" + name)
	newWS := doltdb.EmptyWorkingSet(newRef).WithWorkingRoot(mainWS.WorkingRoot()).WithStagedRoot(mainWS.StagedRoot())
	return initializeWorkingSet(ctx, state.doltDB, newWS, name)
}
