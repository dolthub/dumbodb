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
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newShadowForTest(t *testing.T) *Shadow {
	t.Helper()
	sess := NewSession(stubProvider{}, nil)
	return NewShadow(sess, time.Now())
}

func TestShadow_Use_BumpsLastUsedAtomic(t *testing.T) {
	s := newShadowForTest(t)
	before := s.LastUsed()

	want := time.Date(2030, 1, 2, 3, 4, 5, 6, time.UTC)
	err := s.Use(want, func(*dsess.DoltSession) error { return nil })
	require.NoError(t, err)

	got := s.LastUsed()
	assert.Equal(t, want.UnixNano(), got.UnixNano(), "lastUsed must be the value passed to Use")
	assert.NotEqual(t, before.UnixNano(), got.UnixNano(), "lastUsed must change after Use")
}

func TestShadow_Use_RunsFnAgainstSession(t *testing.T) {
	s := newShadowForTest(t)

	var captured *dsess.DoltSession
	err := s.Use(time.Now(), func(sess *dsess.DoltSession) error {
		captured = sess
		return nil
	})
	require.NoError(t, err)
	assert.Same(t, s.Session(), captured, "Use must pass the same DoltSession to fn that Session() returns")
}

func TestShadow_Use_OnInvalidatedShadow_ReturnsErrShadowInvalidated(t *testing.T) {
	s := newShadowForTest(t)
	s.invalidate()

	called := false
	err := s.Use(time.Now(), func(*dsess.DoltSession) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, ErrShadowInvalidated)
	assert.False(t, called, "fn must not run when the shadow is invalidated")
}

func TestShadow_Use_ForwardsFnError(t *testing.T) {
	s := newShadowForTest(t)
	want := errors.New("handler boom")

	err := s.Use(time.Now(), func(*dsess.DoltSession) error { return want })
	require.ErrorIs(t, err, want)
}

func TestShadow_Commit_BumpsLastUsedAndRunsFn(t *testing.T) {
	s := newShadowForTest(t)

	var captured *dsess.DoltSession
	want := time.Date(2030, 1, 2, 3, 4, 5, 6, time.UTC)
	err := s.Commit(want, func(sess *dsess.DoltSession) error { captured = sess; return nil })
	require.NoError(t, err)

	assert.Equal(t, want.UnixNano(), s.LastUsed().UnixNano())
	assert.Same(t, s.Session(), captured)
}

func TestShadow_Commit_OnInvalidatedShadow_ReturnsErrShadowInvalidated(t *testing.T) {
	s := newShadowForTest(t)
	s.invalidate()

	called := false
	err := s.Commit(time.Now(), func(*dsess.DoltSession) error { called = true; return nil })
	require.ErrorIs(t, err, ErrShadowInvalidated)
	assert.False(t, called)
}

// Commit holds writeMu for the duration of fn. The registry's
// supersede / sweep / end paths acquire writeMu before flipping the
// latch; this test verifies the lock is held by spinning up a goroutine
// that tries to acquire writeMu during the commit. The goroutine must
// not acquire until fn returns.
func TestShadow_Commit_HoldsWriteMuForDurationOfFn(t *testing.T) {
	s := newShadowForTest(t)

	const fnDuration = 80 * time.Millisecond
	commitStarted := make(chan struct{})
	commitReturned := make(chan time.Time, 1)

	go func() {
		err := s.Commit(time.Now(), func(*dsess.DoltSession) error {
			close(commitStarted)
			time.Sleep(fnDuration)
			return nil
		})
		require.NoError(t, err)
		commitReturned <- time.Now()
	}()

	<-commitStarted
	// Try to acquire writeMu while the commit is in flight. This must
	// block until the commit's fn returns.
	acquireStarted := time.Now()
	s.writeMu.Lock()
	acquired := time.Now()
	s.writeMu.Unlock()

	commitDoneAt := <-commitReturned

	// The acquire must not have completed before the commit returned.
	// We allow a small fuzz for scheduling jitter; assert at minimum
	// the acquire took close to fnDuration.
	heldFor := acquired.Sub(acquireStarted)
	assert.GreaterOrEqual(t, heldFor, fnDuration-20*time.Millisecond,
		"writeMu acquire blocked for %v; expected approximately %v", heldFor, fnDuration)
	// And the acquire happened at or after the commit's return.
	assert.GreaterOrEqual(t, acquired.UnixNano(), commitDoneAt.Add(-20*time.Millisecond).UnixNano(),
		"writeMu acquire %v must be at or after commit return %v", acquired, commitDoneAt)
}

// LastUsed and Active reflect the most recent atomic state.
func TestShadow_Active_LastUsed_Session_ReflectState(t *testing.T) {
	sess := NewSession(stubProvider{}, nil)
	created := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	s := NewShadow(sess, created)

	assert.Same(t, sess, s.Session())
	assert.Equal(t, created.UnixNano(), s.LastUsed().UnixNano())
	assert.True(t, s.Active())

	s.invalidate()
	assert.False(t, s.Active())
	// Session pointer remains valid even after invalidation.
	assert.Same(t, sess, s.Session())
}

// Concurrent Use, Active, LastUsed must be race-free. The atomic field
// types (atomic.Bool, atomic.Int64) carry this guarantee but the test
// drives them through Use() to exercise the realistic call path.
func TestShadow_Race_ConcurrentUseAndActiveAreSafe(t *testing.T) {
	s := newShadowForTest(t)

	const ops = 200
	var done atomic.Int32
	workers := 4

	for i := 0; i < workers; i++ {
		go func() {
			for j := 0; j < ops; j++ {
				_ = s.Use(time.Now(), func(*dsess.DoltSession) error { return nil })
			}
			done.Add(1)
		}()
	}
	for i := 0; i < workers; i++ {
		go func() {
			for j := 0; j < ops; j++ {
				_ = s.Active()
				_ = s.LastUsed()
			}
			done.Add(1)
		}()
	}

	deadline := time.Now().Add(5 * time.Second)
	for done.Load() < int32(2*workers) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.Equal(t, int32(2*workers), done.Load(), "workers did not finish in time")
}
