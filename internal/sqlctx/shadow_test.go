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

	"github.com/dolthub/dolt/go/libraries/doltcore/dsess"
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
	require.NoError(t, s.Use(want, func(*dsess.DoltSession) error { return nil }))

	got := s.LastUsed()
	assert.Equal(t, want.UnixNano(), got.UnixNano())
	assert.NotEqual(t, before.UnixNano(), got.UnixNano())
}

func TestShadow_Use_RunsFnAgainstSession(t *testing.T) {
	s := newShadowForTest(t)

	var captured *dsess.DoltSession
	require.NoError(t, s.Use(time.Now(), func(sess *dsess.DoltSession) error {
		captured = sess
		return nil
	}))
	assert.Same(t, s.Session(), captured)
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
	assert.False(t, called)
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
	require.NoError(t, s.Commit(want, func(sess *dsess.DoltSession) error { captured = sess; return nil }))

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

// External writeMu acquire must block until fn returns; this is what
// the registry's invalidate paths rely on to fence a commit.
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
	acquireStarted := time.Now()
	s.writeMu.Lock()
	acquired := time.Now()
	s.writeMu.Unlock()

	commitDoneAt := <-commitReturned

	heldFor := acquired.Sub(acquireStarted)
	assert.GreaterOrEqual(t, heldFor, fnDuration-20*time.Millisecond)
	assert.GreaterOrEqual(t, acquired.UnixNano(), commitDoneAt.Add(-20*time.Millisecond).UnixNano())
}

func TestShadow_Active_LastUsed_Session_ReflectState(t *testing.T) {
	sess := NewSession(stubProvider{}, nil)
	created := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	s := NewShadow(sess, created)

	assert.Same(t, sess, s.Session())
	assert.Equal(t, created.UnixNano(), s.LastUsed().UnixNano())
	assert.True(t, s.Active())

	s.invalidate()
	assert.False(t, s.Active())
	assert.Same(t, sess, s.Session())
}

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
	require.Equal(t, int32(2*workers), done.Load())
}
