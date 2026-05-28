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
	"sync/atomic"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/gcctx"
	"github.com/dolthub/dolt/go/store/hash"

	"github.com/stretchr/testify/require"
)

// TestRunUnderGCSafepointKeeper_GCWaitsForBracket: a GC safepoint
// Waiter started while RunUnderGCSafepointKeeper is in flight must
// not complete its Wait until the bracket releases. The bracket's
// VisitGCRoots is invoked at safepoint resolution.
func TestRunUnderGCSafepointKeeper_GCWaitsForBracket(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	require.NotNil(t, be.gcController, "newBackend must wire gcController")
	require.NotNil(t, be.backgroundRP, "newBackend must wire backgroundRP")

	// Start the bracket FIRST so backgroundRP is registered with the
	// controller before we create the Waiter. The bracket holds via
	// bracketReleased until the test releases it.
	bracketEntered := make(chan struct{})
	bracketReleased := make(chan struct{})
	bracketDone := make(chan struct{})
	go func() {
		defer close(bracketDone)
		err := be.RunUnderGCSafepointKeeper(context.Background(), func() error {
			close(bracketEntered)
			<-bracketReleased
			return nil
		})
		require.NoError(t, err)
	}()
	<-bracketEntered

	// Now create a Waiter from an OUTSIDE GCRootsProvider (this is
	// what GC itself does -- the callSession is excluded from the
	// waited set). With the bracket in flight, the Waiter must defer
	// its visit of backgroundRP until SessionCommandEnd fires.
	outside := &outsideGCRootsProvider{}
	var visitedBackground atomic.Bool
	waiter := be.gcController.Waiter(context.Background(), outside, func(_ context.Context, s gcctx.GCRootsProvider) error {
		if s == gcctx.GCRootsProvider(be.backgroundRP) {
			visitedBackground.Store(true)
		}
		return nil
	})

	// Spin off the Wait on its own goroutine so we can assert it does
	// NOT return before the bracket releases.
	waitErrCh := make(chan error, 1)
	go func() {
		waitErrCh <- waiter.Wait(context.Background())
	}()

	select {
	case <-waitErrCh:
		t.Fatal("waiter.Wait returned before the safepoint bracket released")
	case <-time.After(100 * time.Millisecond):
		// Expected: Wait is blocked.
	}

	close(bracketReleased)
	<-bracketDone

	select {
	case err := <-waitErrCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("waiter.Wait did not return after the bracket released")
	}

	require.True(t, visitedBackground.Load(), "Waiter must invoke visit on backgroundRP at safepoint")
}

// TestRunUnderGCSafepointKeeper_NilControllerFallsThrough: a backend
// constructed without a gcController (test helpers) must still run fn
// without panic.
func TestRunUnderGCSafepointKeeper_NilControllerFallsThrough(t *testing.T) {
	be := &Backend{}
	called := false
	err := be.RunUnderGCSafepointKeeper(context.Background(), func() error {
		called = true
		return nil
	})
	require.NoError(t, err)
	require.True(t, called)
}

// outsideGCRootsProvider stands in for the GC callSession in unit
// tests. Its VisitGCRoots is never invoked by the test paths.
type outsideGCRootsProvider struct{}

func (*outsideGCRootsProvider) VisitGCRoots(_ context.Context, _ string, _ func(hash.Hash) bool) error {
	return nil
}
