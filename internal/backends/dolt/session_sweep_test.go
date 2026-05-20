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
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBackendWithSweepPeriod constructs a Backend exactly like
// newBackend but overrides the session-sweep tick interval. The
// sweeper goroutine has not yet been started when this returns; the
// caller is responsible for starting it (or recreating the backend
// via newBackend if they want default-period production behavior).
//
// Used by tests in this file to drive a much faster ticker than the
// 1-minute production default.
func newBackendForSweepTest(t *testing.T, period time.Duration) *Backend {
	t.Helper()
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	require.NoError(t, err)
	// Stop the production-period sweeper and start a new one with the
	// test period. The production sweeper hasn't run yet (it's blocked
	// on the 1-minute ticker), so this is race-free.
	close(be.sweeperStop)
	<-be.sweeperDone
	be.sweeperStop = make(chan struct{})
	be.sweeperDone = make(chan struct{})
	be.sweeperPeriod = period
	go be.sessionSweepLoop()
	return be
}

// .6.4.9 acceptance:
//   - ticker fires Sweep on schedule
//   - ticker stops cleanly on Backend.Close
//   - empty registry sweep is a no-op (no panic)

func TestBackend_SessionSweep_FiresPeriodically(t *testing.T) {
	be := newBackendForSweepTest(t, 20*time.Millisecond)
	defer be.Close() //nolint:errcheck

	// Connect a session with the timeout already elapsed by spinning
	// up a tiny-timeout registry view: register a session, then
	// manipulate its lastUsed so the next sweep tick will reap it.
	shadow, err := be.SessionRegistry().Connect("test-lsid")
	require.NoError(t, err)
	require.True(t, shadow.Active())
	// Setting lastUsed to 0 is the same trick End uses to mark for sweep.
	be.SessionRegistry().End("test-lsid") // also returns ok

	// Wait up to 500ms for the ticker to reap the entry.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if be.SessionRegistry().Len() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.Equal(t, 0, be.SessionRegistry().Len(), "Sweep must have reaped the marked entry within 500ms")
	assert.False(t, shadow.Active(), "the swept shadow's latch must be off")
}

func TestBackend_SessionSweep_StopsOnClose(t *testing.T) {
	be := newBackendForSweepTest(t, 10*time.Millisecond)

	// Close must complete promptly: the sweeper goroutine observes
	// sweeperStop and exits within at most one tick.
	closeStart := time.Now()
	be.Close() //nolint:errcheck
	closeDuration := time.Since(closeStart)
	assert.Less(t, closeDuration, 200*time.Millisecond,
		"Close must drain the sweeper within ~one tick; took %v", closeDuration)

	// Verify sweeperDone is closed by ranging through it briefly. If
	// the goroutine were leaked we'd hang here; the test's t.Timeout
	// would catch it but the direct receive proves intent.
	select {
	case <-be.sweeperDone:
		// good
	default:
		t.Fatal("sweeperDone must be closed after Backend.Close returns")
	}
}

func TestBackend_SessionSweep_EmptyRegistryNoPanic(t *testing.T) {
	be := newBackendForSweepTest(t, 5*time.Millisecond)
	defer be.Close() //nolint:errcheck

	// Let many ticks fire against an empty registry. If Sweep
	// dereferenced something it shouldn't, this would panic.
	time.Sleep(60 * time.Millisecond)
	assert.Equal(t, 0, be.SessionRegistry().Len())
}
