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
	assert.Same(t, first, got, "Get must return the same shadow as Connect just produced")
}

func TestSessionRegistry_Connect_InvalidatesPriorShadow(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())

	first, err := r.Connect("lsid-A")
	require.NoError(t, err)
	require.True(t, first.Active())

	second, err := r.Connect("lsid-A")
	require.NoError(t, err)
	require.True(t, second.Active())

	assert.NotSame(t, first, second, "reconnect must return a new shadow")
	assert.False(t, first.Active(), "the prior shadow's latch must be off after reconnect")
	assert.Equal(t, 1, r.Len(), "the entry count must not grow on reconnect")
}

func TestSessionRegistry_Connect_NewShadowInheritsLastUsed(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())

	clock := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r.WithClock(func() time.Time { return clock })

	first, err := r.Connect("lsid-A")
	require.NoError(t, err)
	// Simulate activity at T=5min via Use.
	usedAt := clock.Add(5 * time.Minute)
	require.NoError(t, first.Use(usedAt, func(*dsess.DoltSession) error { return nil }))
	assert.Equal(t, usedAt.UnixNano(), first.LastUsed().UnixNano())

	// Reconnect at T=10min. The new shadow's lastUsed must equal the old
	// shadow's lastUsed at the moment of supersession (T=5min, not the
	// reconnect time of T=10min).
	clock = clock.Add(5 * time.Minute) // T = 10min
	second, err := r.Connect("lsid-A")
	require.NoError(t, err)
	assert.Equal(t, usedAt.UnixNano(), second.LastUsed().UnixNano(),
		"reconnect must inherit lastUsed; reconnect itself is not session activity")
}

func TestSessionRegistry_Connect_NewShadowReusesSession(t *testing.T) {
	r := NewSessionRegistry(time.Hour, stubFactory())

	first, err := r.Connect("lsid-A")
	require.NoError(t, err)

	second, err := r.Connect("lsid-A")
	require.NoError(t, err)

	assert.Same(t, first.Session(), second.Session(),
		"reconnect must reuse the underlying *dsess.DoltSession")
}

func TestSessionRegistry_FactoryErrorNoEntryStored(t *testing.T) {
	wantErr := errors.New("factory boom")
	r := NewSessionRegistry(time.Hour, func(lsid string) (*dsess.DoltSession, error) {
		return nil, wantErr
	})

	_, err := r.Connect("lsid-A")
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, 0, r.Len(), "failed creation must not leave a stub entry")

	_, ok := r.Get("lsid-A")
	assert.False(t, ok)
}

// Use loop on shadow_A while another goroutine supersedes via Connect.
// After supersession, every subsequent Use on shadow_A must return
// ErrShadowInvalidated. Run under -race to validate the lock-free hot
// path against the registry mutex / writeMu interplay.
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

	// Drive Use on the first shadow before, during, and after supersede.
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

	// After supersede is done, every Use must error.
	for i := 0; i < 50; i++ {
		err := first.Use(time.Now(), func(*dsess.DoltSession) error { return nil })
		require.ErrorIs(t, err, ErrShadowInvalidated)
	}

	t.Logf("pre-supersede Use successes: %d, post-supersede errors: %d", preSupersedeUses, postSupersedeErrs)
}

// 32 goroutines Connect to one lsid. Connect is serialised by r.mu, so
// they run sequentially; each call supersedes the prior. Exactly one
// shadow ends up active at the end, and all others are invalidated.
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
	assert.Equal(t, 1, active, "exactly one shadow must remain Active after concurrent supersedes")
	assert.Equal(t, 1, r.Len(), "exactly one entry in the registry")
}

// Many distinct lsids in parallel each get their own session.
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

	assert.Equal(t, workers, r.Len(), "each distinct lsid must produce its own entry")
}

// T=0 Connect, T=4min Connect again (inherits lastUsed=0), T=8min check
// with a 10-minute idle window: the entry should still be there with
// lastUsed near 0. This documents that the reconnect inherits and does
// not refresh. Sweep itself arrives in .6.4.3; here we just verify
// LastUsed on the new shadow.
func TestSessionRegistry_LastUsed_AcrossSupersession(t *testing.T) {
	r := NewSessionRegistry(10*time.Minute, stubFactory())
	clock := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r.WithClock(func() time.Time { return clock })

	first, err := r.Connect("lsid-A")
	require.NoError(t, err)
	originalLastUsed := first.LastUsed()

	// Reconnect at T+4min.
	clock = clock.Add(4 * time.Minute)
	second, err := r.Connect("lsid-A")
	require.NoError(t, err)

	assert.Equal(t, originalLastUsed.UnixNano(), second.LastUsed().UnixNano(),
		"new shadow's lastUsed must equal the original lastUsed, not the reconnect time")

	// And the registry still has the one entry.
	assert.Equal(t, 1, r.Len())
}
