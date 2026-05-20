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
	"sync"
	"sync/atomic"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
)

type SessionFactory func(lsid string) (*dsess.DoltSession, error)

type sessionEntry struct {
	sess   *dsess.DoltSession
	shadow atomic.Pointer[Shadow]
}

// Lock order is r.mu then shadow.writeMu. Shadow.Commit takes only
// writeMu.
type SessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*sessionEntry
	timeout  time.Duration
	factory  SessionFactory
	nowFn    func() time.Time
}

func NewSessionRegistry(timeout time.Duration, factory SessionFactory) *SessionRegistry {
	return &SessionRegistry{
		sessions: make(map[string]*sessionEntry),
		timeout:  timeout,
		factory:  factory,
		nowFn:    time.Now,
	}
}

func (r *SessionRegistry) WithClock(now func() time.Time) *SessionRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nowFn = now
	return r
}

// Connect supersedes any existing shadow for lsid. The new shadow
// carries forward lastUsed so reconnection does not reset the idle
// window -- the first Use/Commit on the returned shadow records actual
// activity.
func (r *SessionRegistry) Connect(lsid string) (*Shadow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if entry, ok := r.sessions[lsid]; ok {
		oldShadow := entry.shadow.Load()
		oldShadow.writeMu.Lock()
		oldShadow.invalidate()
		carried := oldShadow.lastUsed.Load()
		oldShadow.writeMu.Unlock()

		newShadow := &Shadow{sess: entry.sess}
		newShadow.lastUsed.Store(carried)
		newShadow.active.Store(true)
		entry.shadow.Store(newShadow)
		return newShadow, nil
	}

	sess, err := r.factory(lsid)
	if err != nil {
		return nil, err
	}

	shadow := NewShadow(sess, r.nowFn())
	entry := &sessionEntry{sess: sess}
	entry.shadow.Store(shadow)
	r.sessions[lsid] = entry
	return shadow, nil
}

func (r *SessionRegistry) PurgeNow(lsid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.sessions[lsid]
	if !ok {
		return false
	}

	shadow := entry.shadow.Load()
	shadow.writeMu.Lock()
	shadow.invalidate()
	shadow.writeMu.Unlock()

	entry.sess.SessionEnd()
	delete(r.sessions, lsid)
	return true
}

// Sweep is two-phase: collect eligible lsids under r.mu, then drop the
// lock before calling PurgeNow, which acquires shadow.writeMu and may
// block on an in-flight Commit.
func (r *SessionRegistry) Sweep(asOf time.Time) int {
	cutoffNanos := asOf.Add(-r.timeout).UnixNano()

	var eligible []string
	r.mu.Lock()
	for lsid, entry := range r.sessions {
		if entry.shadow.Load().lastUsed.Load() < cutoffNanos {
			eligible = append(eligible, lsid)
		}
	}
	r.mu.Unlock()

	removed := 0
	for _, lsid := range eligible {
		if r.PurgeNow(lsid) {
			removed++
		}
	}
	return removed
}

// End is advisory: matches MongoDB endSessions, which neither cancels
// in-flight work nor removes session state. We mark lastUsed=0 so the
// next Sweep reaps; the latch stays on until then.
func (r *SessionRegistry) End(lsid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[lsid]
	if !ok {
		return false
	}
	entry.shadow.Load().lastUsed.Store(0)
	return true
}

func (r *SessionRegistry) Get(lsid string) (*Shadow, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[lsid]
	if !ok {
		return nil, false
	}
	return entry.shadow.Load(), true
}

func (r *SessionRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}
