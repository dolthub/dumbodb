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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
)

// SessionFactory produces a fresh *dsess.DoltSession for a previously
// unknown lsid. Called by Connect on first sight; never invoked on
// reconnect (the existing entry's underlying session is reused).
//
// Factories used in production must construct sessions wired to the
// process's gcctx.GCSafepointController so VisitGCRoots is meaningful.
type SessionFactory func(lsid string) (*dsess.DoltSession, error)

// sessionEntry is the per-lsid registry record. sess is permanent for the
// entry's lifetime; only shadow is swapped on supersession.
type sessionEntry struct {
	sess   *dsess.DoltSession
	shadow atomic.Pointer[Shadow]
}

// SessionRegistry maps an externally-managed session id (a MongoDB lsid)
// to a *dsess.DoltSession via a Shadow handle. See
// docs/design/session-registry.md for the full model.
//
// Locking: r.mu serialises Connect, PurgeNow (subsequent bead), Sweep,
// End, Get, Len. The hot path (Shadow.Use, Shadow.Commit) does not touch
// r.mu. Lock order is r.mu first, then shadow.writeMu second; the commit
// path takes only writeMu, so there is no inversion.
type SessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*sessionEntry
	timeout  time.Duration
	factory  SessionFactory
	nowFn    func() time.Time
}

// NewSessionRegistry returns a registry that creates sessions via factory
// and considers idle entries older than timeout eligible for Sweep
// (Sweep itself arrives in .6.4.3).
func NewSessionRegistry(timeout time.Duration, factory SessionFactory) *SessionRegistry {
	return &SessionRegistry{
		sessions: make(map[string]*sessionEntry),
		timeout:  timeout,
		factory:  factory,
		nowFn:    time.Now,
	}
}

// WithClock overrides the registry's notion of "now". Tests use this to
// advance time deterministically.
func (r *SessionRegistry) WithClock(now func() time.Time) *SessionRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nowFn = now
	return r
}

// Connect returns the active shadow for lsid. On first sight, the factory
// mints a fresh *dsess.DoltSession, the registry registers it with the GC
// safepoint controller (via a Begin/End pair on CommandBegin), and a new
// Shadow is created and stored. On reconnect, the existing shadow's latch
// is flipped to false (fenced against any in-flight Commit via the
// shadow's writeMu) and a new Shadow is minted carrying forward lastUsed,
// so the idle-timeout window does not reset just because a TCP connection
// dropped and a new one arrived.
//
// The new shadow's lastUsed inherits the old shadow's lastUsed: the
// reconnect itself is routing, not session activity. The first Use or
// Commit on the new shadow bumps lastUsed to the actual operation time.
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

	// Register the new session with the GC safepoint controller (no-op
	// when the session was built without a controller, e.g. in unit
	// tests). The Begin auto-registers the session in the controller's
	// map; the immediate End marks it quiesced so VisitGCRoots can be
	// called during GC events.
	if err := sess.CommandBegin(); err != nil {
		return nil, fmt.Errorf("SessionRegistry.Connect: CommandBegin for new session %q: %w", lsid, err)
	}
	sess.CommandEnd()

	shadow := NewShadow(sess, r.nowFn())
	entry := &sessionEntry{sess: sess}
	entry.shadow.Store(shadow)
	r.sessions[lsid] = entry
	return shadow, nil
}

// PurgeNow forcibly invalidates and removes the entry for lsid. The
// shadow's latch is flipped (fenced against any in-flight Commit via
// shadow.writeMu), the underlying session is deregistered from the GC
// safepoint controller via sess.SessionEnd(), and the entry is deleted
// from the map. Returns false if lsid is unknown.
//
// PurgeNow is the engine of both Sweep (idle reap) and explicit
// teardown. The advisory End (subsequent bead) does not call PurgeNow
// directly; it merely marks the entry so the next Sweep tick picks it
// up.
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

// Sweep removes every entry whose shadow.lastUsed is older than
// asOf - timeout. Returns the number of sessions removed. Callers
// schedule Sweep on a timer (a goroutine in Backend arrives in .6.4.9);
// the registry does not run its own goroutine.
//
// Two-phase to avoid holding r.mu across a writeMu acquire that may
// block on an in-flight Commit:
//
//  1. Under r.mu, collect lsids whose lastUsed predates the cutoff.
//  2. Release r.mu and call PurgeNow on each collected lsid. PurgeNow
//     re-takes r.mu; an entry that was deleted by another path
//     between phases is safely returned-false.
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

// End is the advisory implementation of Mongo's endSessions command.
// Per live-Mongo probes (see docs/design/session-registry.md), endSessions
// is best-effort: it neither cancels in-flight work nor immediately
// removes session state. We match that semantic by marking the entry's
// shadow.lastUsed = 0 so the next Sweep tick picks it up; the latch
// stays on and active connections keep working until the sweep arrives.
//
// Returns true if lsid was known. Mongo always returns ok:1 even on
// unknown lsids; the boolean here is for tests and metrics.
//
// End does NOT acquire shadow.writeMu, so it returns immediately even
// while a Commit is in flight. The marked entry will be purged by Sweep
// after the commit completes.
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

// Get returns the current active shadow for lsid without altering its
// state. Intended for tests and observation; callers that intend to do
// work must use Connect followed by Shadow.Use / Shadow.Commit.
func (r *SessionRegistry) Get(lsid string) (*Shadow, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.sessions[lsid]
	if !ok {
		return nil, false
	}
	return entry.shadow.Load(), true
}

// Len reports the number of registered lsids.
func (r *SessionRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}
