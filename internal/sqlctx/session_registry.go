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

// A sessionEntry stays in the registry while it is being torn down, so
// the lsid is never briefly absent. terminating is guarded by r.mu; done
// closes once SessionEnd has returned and the entry has left the map.
type sessionEntry struct {
	state       *sessionState
	shadow      atomic.Pointer[Shadow]
	terminating bool
	done        chan struct{}
}

// Lock order is r.mu then sessionState.cmdMu, and the two are never held
// together: a command can call back into End (endSessions dispatches
// through Shadow.Use), so any path that waits on cmdMu releases r.mu
// first. See beginTeardown / teardown.
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

// Connect returns the shadow for lsid, creating the session on first use.
// It does NOT supersede a shadow already in use: several connections may
// hold the same lsid at once, they share one session, command latch and
// idle clock, and the latch serializes their commands.
//
// A shadow therefore goes inactive only on teardown, so a caller that
// finds one inactive is looking at a reaped session (wire code 251), not
// at a connection that lost a race.
//
// An lsid whose teardown is in progress has no usable session, and
// building a second one alongside it would defeat the per-lsid latch.
// Connect instead waits for that teardown -- with r.mu released, since
// teardown blocks on the command latch -- and then retries.
func (r *SessionRegistry) Connect(lsid string) (*Shadow, error) {
	for {
		r.mu.Lock()

		entry, ok := r.sessions[lsid]
		if !ok {
			sess, err := r.factory(lsid)
			if err != nil {
				r.mu.Unlock()
				return nil, err
			}

			state := &sessionState{sess: sess}
			state.lastUsed.Store(r.nowFn().UnixNano())
			entry = &sessionEntry{state: state, done: make(chan struct{})}
			entry.shadow.Store(newShadow(state))
			r.sessions[lsid] = entry

			shadow := entry.shadow.Load()
			r.mu.Unlock()
			return shadow, nil
		}

		if !entry.terminating {
			// The live shadow is handed back as it is. An lsid reaching a
			// second connection is ordinary -- drivers pool logical sessions
			// independently of connections -- so it must not cost the first
			// connection its session. Every shadow for an lsid shares one
			// sessionState, so the command latch already keeps the two
			// connections' commands from overlapping, which is the only
			// thing dsess requires.
			shadow := entry.shadow.Load()
			r.mu.Unlock()
			return shadow, nil
		}

		done := entry.done
		r.mu.Unlock()
		<-done
	}
}

// beginTeardown claims lsid for teardown, leaving the entry in the map as
// a tombstone. When idleBefore is non-nil the claim is refused unless the
// session is still idle as of that instant, which closes the window
// between Sweep's eligibility scan and this call.
func (r *SessionRegistry) beginTeardown(lsid string, idleBefore *int64) (*sessionEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.sessions[lsid]
	if !ok || entry.terminating {
		return nil, false
	}
	if idleBefore != nil && entry.state.lastUsed.Load() >= *idleBefore {
		return nil, false
	}

	entry.terminating = true
	return entry, true
}

// teardown ends the session under the command latch so SessionEnd never
// lands on an outstanding command, then drops the tombstone. Callers must
// hold the claim from beginTeardown and must not hold r.mu.
func (r *SessionRegistry) teardown(lsid string, entry *sessionEntry) {
	entry.state.cmdMu.Lock()
	entry.shadow.Load().purge()
	entry.state.sess.SessionEnd()
	entry.state.cmdMu.Unlock()

	r.mu.Lock()
	delete(r.sessions, lsid)
	r.mu.Unlock()

	close(entry.done)
}

func (r *SessionRegistry) PurgeNow(lsid string) bool {
	entry, ok := r.beginTeardown(lsid, nil)
	if !ok {
		return false
	}

	r.teardown(lsid, entry)
	return true
}

// Sweep is two-phase: collect eligible lsids under r.mu, then drop the
// lock before tearing each one down, since teardown waits on the command
// latch and may block on an in-flight command.
func (r *SessionRegistry) Sweep(asOf time.Time) int {
	cutoffNanos := asOf.Add(-r.timeout).UnixNano()

	var eligible []string
	r.mu.Lock()
	for lsid, entry := range r.sessions {
		if entry.terminating {
			continue
		}
		if entry.state.lastUsed.Load() < cutoffNanos {
			eligible = append(eligible, lsid)
		}
	}
	r.mu.Unlock()

	removed := 0
	for _, lsid := range eligible {
		entry, ok := r.beginTeardown(lsid, &cutoffNanos)
		if !ok {
			continue
		}
		r.teardown(lsid, entry)
		removed++
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
	if !ok || entry.terminating {
		return false
	}
	entry.state.lastUsed.Store(0)
	return true
}

func (r *SessionRegistry) Get(lsid string) (*Shadow, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[lsid]
	if !ok || entry.terminating {
		return nil, false
	}
	return entry.shadow.Load(), true
}

// Len counts live sessions; entries left as teardown tombstones are not
// usable and are excluded.
func (r *SessionRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, entry := range r.sessions {
		if !entry.terminating {
			n++
		}
	}
	return n
}

// SessionInfo describes one cached logical session for enumeration
// (e.g. $listLocalSessions).
type SessionInfo struct {
	Lsid     string
	LastUsed time.Time
}

// Snapshot returns the lsid and last-used time of every live session in
// the registry. Ended sessions (lastUsed==0, awaiting sweep) are omitted.
func (r *SessionRegistry) Snapshot() []SessionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SessionInfo, 0, len(r.sessions))
	for lsid, entry := range r.sessions {
		if entry.terminating {
			continue
		}
		lu := entry.shadow.Load().LastUsed()
		if lu.UnixNano() == 0 {
			continue
		}
		out = append(out, SessionInfo{Lsid: lsid, LastUsed: lu})
	}
	return out
}
