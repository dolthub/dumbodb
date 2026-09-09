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

package sqlctx

import (
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/gcctx"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const inflightFnDuration = 80 * time.Millisecond

// gcFactory mirrors production wiring: Backend builds sessions via
// NewSessionWithGC, so CommandBegin/CommandEnd/SessionEnd reach a real
// GCSafepointController. stubFactory does not, which is why the sibling
// Race_UseVsSweep test cannot observe a teardown-vs-command panic.
func gcFactory(gc *gcctx.GCSafepointController) SessionFactory {
	return func(lsid string) (*dsess.DoltSession, error) {
		return NewSessionWithGC(stubProvider{}, nil, gc)
	}
}

// startInflightUse opens a command on s that stays open for
// inflightFnDuration. It returns once the command is genuinely open, plus
// a channel carrying the instant the command finished.
func startInflightUse(t *testing.T, s *Shadow) <-chan time.Time {
	t.Helper()

	open := make(chan struct{})
	returned := make(chan time.Time, 1)

	go func() {
		err := s.Use(time.Now(), func(*dsess.DoltSession) error {
			close(open)
			time.Sleep(inflightFnDuration)
			return nil
		})
		assert.NoError(t, err)
		returned <- time.Now()
	}()

	<-open
	return returned
}

// recoverOf runs fn and reports whatever it panicked with.
func recoverOf(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

// A sweep tick that lands on a session running a command through
// Shadow.Use must wait for that command rather than calling SessionEnd
// underneath it. gcctx.SessionEnd panics on an outstanding command, and
// the sweeper goroutine has no recover, so the panic kills the server.
func TestSessionRegistry_Sweep_FencesAgainstInflightUse(t *testing.T) {
	r := NewSessionRegistry(time.Nanosecond, gcFactory(gcctx.NewGCSafepointController()))

	s, err := r.Connect("lsid-A")
	require.NoError(t, err)

	commandReturned := startInflightUse(t, s)

	var removed int
	sweepStarted := time.Now()
	recovered := recoverOf(func() { removed = r.Sweep(time.Now().Add(time.Hour)) })
	sweepReturned := time.Now()

	require.Nil(t, recovered, "Sweep panicked against an in-flight Shadow.Use")
	assert.Equal(t, 1, removed)
	assert.GreaterOrEqual(t, sweepReturned.Sub(sweepStarted), inflightFnDuration-20*time.Millisecond)
	assert.GreaterOrEqual(t, sweepReturned.UnixNano(), (<-commandReturned).Add(-20*time.Millisecond).UnixNano())
	assert.Equal(t, 0, r.Len())
}

// Connect's supersede path hands the new Shadow the same underlying
// DoltSession, which permits only one open command. The new shadow's
// first command must therefore queue behind one still running on the
// shadow it replaced.
func TestSessionRegistry_Connect_SupersedeSerializesCommands(t *testing.T) {
	r := NewSessionRegistry(time.Hour, gcFactory(gcctx.NewGCSafepointController()))

	first, err := r.Connect("lsid-A")
	require.NoError(t, err)

	commandReturned := startInflightUse(t, first)

	second, err := r.Connect("lsid-A")
	require.NoError(t, err)

	ran := false
	recovered := recoverOf(func() {
		assert.NoError(t, second.Use(time.Now(), func(*dsess.DoltSession) error {
			ran = true
			return nil
		}))
	})

	require.Nil(t, recovered, "superseding shadow opened a command on a session already running one")
	assert.True(t, ran)
	assert.GreaterOrEqual(t, time.Now().UnixNano(), (<-commandReturned).Add(-20*time.Millisecond).UnixNano())
}

// End zeroes lastUsed, so a disconnect arms the session for the very next
// sweep tick regardless of the idle timeout. Another connection sharing
// the lsid may still be mid-command, and that command must survive.
func TestSessionRegistry_Sweep_AfterEndFencesAgainstInflightUse(t *testing.T) {
	r := NewSessionRegistry(30*time.Minute, gcFactory(gcctx.NewGCSafepointController()))

	s, err := r.Connect("lsid-shared")
	require.NoError(t, err)

	commandReturned := startInflightUse(t, s)
	require.True(t, r.End("lsid-shared"))

	var removed int
	recovered := recoverOf(func() { removed = r.Sweep(time.Now()) })
	sweepReturned := time.Now()

	require.Nil(t, recovered, "sweep after End reaped a session with a live command")
	assert.Equal(t, 1, removed)
	assert.GreaterOrEqual(t, sweepReturned.UnixNano(), (<-commandReturned).Add(-20*time.Millisecond).UnixNano())
}

// A command that reaches the latch while a teardown holds it must not
// reopen the ended session. The latch is taken here the way teardown
// takes it, because racing a real Sweep would leave the winner of the
// latch up to the scheduler.
func TestShadow_Use_RechecksActiveUnderCommandLatch(t *testing.T) {
	r := NewSessionRegistry(time.Hour, gcFactory(gcctx.NewGCSafepointController()))

	s, err := r.Connect("lsid-A")
	require.NoError(t, err)

	s.state.cmdMu.Lock()

	result := make(chan error, 1)
	ran := make(chan struct{}, 1)
	go func() {
		result <- s.Use(time.Now(), func(*dsess.DoltSession) error {
			ran <- struct{}{}
			return nil
		})
	}()

	// Gives the command time to clear its pre-latch check and block, so
	// the recheck inside the latch is what rejects it.
	time.Sleep(20 * time.Millisecond)
	s.purge()
	s.state.cmdMu.Unlock()

	require.ErrorIs(t, <-result, ErrShadowInvalidated)
	assert.Empty(t, ran, "command ran against a session that was already ended")
}
