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
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stubFactory() SessionFactory {
	return func(lsid string) (*dsess.DoltSession, error) {
		return NewSession(stubProvider{}, nil), nil
	}
}

func TestSessionRegistry_Connect_CreatesAndCaches(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())

	first, err := r.Connect("lsid-A")
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.True(t, first.Active())
	assert.Equal(t, 1, r.Len())

	got, ok := r.Get("lsid-A")
	require.True(t, ok)
	assert.Same(t, first, got)
}

func TestSessionRegistry_Connect_InvalidatesPriorShadow(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())

	first, err := r.Connect("lsid-A")
	require.NoError(t, err)
	require.True(t, first.Active())

	second, err := r.Connect("lsid-A")
	require.NoError(t, err)
	require.True(t, second.Active())

	assert.NotSame(t, first, second)
	assert.False(t, first.Active())
	assert.Equal(t, 1, r.Len())
}

func TestSessionRegistry_Connect_NewShadowInheritsLastUsed(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())

	clock := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r.WithClock(func() time.Time { return clock })

	first, err := r.Connect("lsid-A")
	require.NoError(t, err)
	usedAt := clock.Add(5 * time.Minute)
	require.NoError(t, first.Use(usedAt, func(*dsess.DoltSession) error { return nil }))
	assert.Equal(t, usedAt.UnixNano(), first.LastUsed().UnixNano())

	clock = clock.Add(5 * time.Minute)
	second, err := r.Connect("lsid-A")
	require.NoError(t, err)
	assert.Equal(t, usedAt.UnixNano(), second.LastUsed().UnixNano())
}

func TestSessionRegistry_Connect_NewShadowReusesSession(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())

	first, err := r.Connect("lsid-A")
	require.NoError(t, err)

	second, err := r.Connect("lsid-A")
	require.NoError(t, err)

	assert.Same(t, first.Session(), second.Session())
}

func TestSessionRegistry_FactoryErrorNoEntryStored(t *testing.T) {
	wantErr := errors.New("factory boom")
	r := NewSessionRegistry(time.Hour, func(lsid string) (*dsess.DoltSession, error) {
		return nil, wantErr
	})

	_, err := r.Connect("lsid-A")
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, 0, r.Len())

	_, ok := r.Get("lsid-A")
	assert.False(t, ok)
}

func TestSessionRegistry_Race_UseVsConnect(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())

	first, err := r.Connect("lsid-A")
	require.NoError(t, err)

	supersedeReady := make(chan struct{})
	supersedeDone := make(chan struct{})

	go func() {
		<-supersedeReady
		_, _ = r.Connect("lsid-A")
		close(supersedeDone)
	}()

	var preSupersedeUses, postSupersedeErrs int32
	close(supersedeReady)

	for i := 0; i < 200; i++ {
		err := first.Use(time.Now(), func(*dsess.DoltSession) error { return nil })
		if err == nil {
			atomic.AddInt32(&preSupersedeUses, 1)
		}
		if errors.Is(err, ErrShadowInvalidated) {
			atomic.AddInt32(&postSupersedeErrs, 1)
		}
	}

	<-supersedeDone

	for i := 0; i < 50; i++ {
		err := first.Use(time.Now(), func(*dsess.DoltSession) error { return nil })
		require.ErrorIs(t, err, ErrShadowInvalidated)
	}

	t.Logf("pre-supersede Use successes: %d, post-supersede errors: %d", preSupersedeUses, postSupersedeErrs)
}

func TestSessionRegistry_Concurrent_Connect_SameLsid(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())

	const workers = 32
	shadows := make([]*Shadow, workers)
	var wg sync.WaitGroup
	wg.Add(workers)

	gate := make(chan struct{})
	for i := 0; i < workers; i++ {
		idx := i
		go func() {
			defer wg.Done()
			<-gate
			s, err := r.Connect("lsid-A")
			if err != nil {
				t.Errorf("Connect: %v", err)
				return
			}
			shadows[idx] = s
		}()
	}
	close(gate)
	wg.Wait()

	active := 0
	for _, s := range shadows {
		if s.Active() {
			active++
		}
	}
	assert.Equal(t, 1, active)
	assert.Equal(t, 1, r.Len())
}

func TestSessionRegistry_Concurrent_Connect_DifferentLsids(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)

	gate := make(chan struct{})
	for i := 0; i < workers; i++ {
		lsid := fmt.Sprintf("lsid-%d", i)
		go func() {
			defer wg.Done()
			<-gate
			_, err := r.Connect(lsid)
			if err != nil {
				t.Errorf("Connect %s: %v", lsid, err)
			}
		}()
	}
	close(gate)
	wg.Wait()

	assert.Equal(t, workers, r.Len())
}

func TestSessionRegistry_PurgeNow_InvalidatesShadow(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())

	s, err := r.Connect("lsid-A")
	require.NoError(t, err)
	require.True(t, s.Active())

	assert.True(t, r.PurgeNow("lsid-A"))
	assert.False(t, s.Active())
}

func TestSessionRegistry_PurgeNow_DeletesEntry(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())

	_, err := r.Connect("lsid-A")
	require.NoError(t, err)
	assert.Equal(t, 1, r.Len())

	assert.True(t, r.PurgeNow("lsid-A"))
	assert.Equal(t, 0, r.Len())

	_, ok := r.Get("lsid-A")
	assert.False(t, ok)
}

func TestSessionRegistry_PurgeNow_OnUnknownReturnsFalse(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())
	assert.False(t, r.PurgeNow("never-existed"))
}

// stubProvider sessions have a nil gcSafepointController so SessionEnd
// is a no-op; the GC integration is exercised in the dolt-backend
// integration tests.
func TestSessionRegistry_PurgeNow_CallsSessionEnd(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())
	_, err := r.Connect("lsid-A")
	require.NoError(t, err)
	require.True(t, r.PurgeNow("lsid-A"))
}

func TestSessionRegistry_Sweep_RemovesIdle(t *testing.T) {
	r := NewSessionRegistry(5*time.Minute, stubFactory())
	clock := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r.WithClock(func() time.Time { return clock })

	s, err := r.Connect("lsid-A")
	require.NoError(t, err)
	require.True(t, s.Active())

	clock = clock.Add(10 * time.Minute)
	removed := r.Sweep(clock)
	assert.Equal(t, 1, removed)
	assert.Equal(t, 0, r.Len())
	assert.False(t, s.Active())
}

func TestSessionRegistry_Sweep_KeepsActive(t *testing.T) {
	r := NewSessionRegistry(10*time.Minute, stubFactory())
	clock := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r.WithClock(func() time.Time { return clock })

	s, err := r.Connect("lsid-A")
	require.NoError(t, err)

	for step := 0; step < 4; step++ {
		clock = clock.Add(2 * time.Minute)
		require.NoError(t, s.Use(clock, func(*dsess.DoltSession) error { return nil }))
		assert.Equal(t, 0, r.Sweep(clock))
	}

	clock = clock.Add(11 * time.Minute)
	assert.Equal(t, 1, r.Sweep(clock))
}

func TestSessionRegistry_Race_UseVsSweep(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())
	s, err := r.Connect("lsid-A")
	require.NoError(t, err)

	stop := make(chan struct{})
	useDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				close(useDone)
				return
			default:
				err := s.Use(time.Now(), func(*dsess.DoltSession) error { return nil })
				assert.NoError(t, err)
			}
		}
	}()

	for i := 0; i < 100; i++ {
		removed := r.Sweep(time.Now())
		assert.Equal(t, 0, removed)
	}

	close(stop)
	<-useDone
}

// Long-running Commit must fence PurgeNow against the latch flip.
func TestSessionRegistry_Sweep_FencesAgainstCommit(t *testing.T) {
	r := NewSessionRegistry(time.Nanosecond, stubFactory())
	clock := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r.WithClock(func() time.Time { return clock })

	s, err := r.Connect("lsid-A")
	require.NoError(t, err)

	const fnDuration = 80 * time.Millisecond
	commitStarted := make(chan struct{})
	commitReturned := make(chan time.Time, 1)

	go func() {
		err := s.Commit(clock, func(*dsess.DoltSession) error {
			close(commitStarted)
			time.Sleep(fnDuration)
			return nil
		})
		require.NoError(t, err)
		commitReturned <- time.Now()
	}()

	<-commitStarted
	sweepStarted := time.Now()
	removed := r.Sweep(clock.Add(time.Hour))
	sweepReturned := time.Now()

	commitDoneAt := <-commitReturned

	assert.Equal(t, 1, removed)
	heldFor := sweepReturned.Sub(sweepStarted)
	assert.GreaterOrEqual(t, heldFor, fnDuration-20*time.Millisecond)
	assert.GreaterOrEqual(t, sweepReturned.UnixNano(), commitDoneAt.Add(-20*time.Millisecond).UnixNano())
}

func TestSessionRegistry_End_IsAdvisoryNotImmediate(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())

	s, err := r.Connect("lsid-A")
	require.NoError(t, err)
	require.True(t, s.Active())

	assert.True(t, r.End("lsid-A"))
	assert.True(t, s.Active())
	assert.Equal(t, 1, r.Len())

	require.NoError(t, s.Use(time.Now(), func(*dsess.DoltSession) error { return nil }))
}

func TestSessionRegistry_End_OnUnknownLsidReturnsFalse(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())
	assert.False(t, r.End("never-existed"))
}

func TestSessionRegistry_End_DoesNotFenceAgainstCommit(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())
	s, err := r.Connect("lsid-A")
	require.NoError(t, err)

	const fnDuration = 100 * time.Millisecond
	commitStarted := make(chan struct{})
	commitReturned := make(chan struct{})

	go func() {
		_ = s.Commit(time.Now(), func(*dsess.DoltSession) error {
			close(commitStarted)
			time.Sleep(fnDuration)
			return nil
		})
		close(commitReturned)
	}()

	<-commitStarted
	endStart := time.Now()
	assert.True(t, r.End("lsid-A"))
	endDuration := time.Since(endStart)

	assert.Less(t, endDuration, fnDuration/2)

	<-commitReturned
}

func TestSessionRegistry_End_FollowedBySweepReaps(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())
	clock := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r.WithClock(func() time.Time { return clock })

	s, err := r.Connect("lsid-A")
	require.NoError(t, err)

	assert.True(t, r.End("lsid-A"))

	removed := r.Sweep(clock.Add(time.Second))
	assert.Equal(t, 1, removed)
	assert.False(t, s.Active())
	assert.Equal(t, 0, r.Len())
}

// Race window where End's mark is overwritten by a concurrent Use; the
// session survives until idle for real. Benign per the design.
func TestSessionRegistry_End_BumpedBackByUse(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())
	clock := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r.WithClock(func() time.Time { return clock })

	s, err := r.Connect("lsid-A")
	require.NoError(t, err)

	assert.True(t, r.End("lsid-A"))

	require.NoError(t, s.Use(clock, func(*dsess.DoltSession) error { return nil }))

	removed := r.Sweep(clock)
	assert.Equal(t, 0, removed)
	assert.True(t, s.Active())
}

func TestSessionRegistry_LastUsed_AcrossSupersession(t *testing.T) {
	r := NewSessionRegistry(10*time.Minute, stubFactory())
	clock := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r.WithClock(func() time.Time { return clock })

	first, err := r.Connect("lsid-A")
	require.NoError(t, err)
	originalLastUsed := first.LastUsed()

	clock = clock.Add(4 * time.Minute)
	second, err := r.Connect("lsid-A")
	require.NoError(t, err)

	assert.Equal(t, originalLastUsed.UnixNano(), second.LastUsed().UnixNano())
	assert.Equal(t, 1, r.Len())
}
