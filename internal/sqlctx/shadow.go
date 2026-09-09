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
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
)

var ErrShadowInvalidated = errors.New("session shadow invalidated by reconnect or sweep")

// sessionState is the lsid's DoltSession plus the latch serializing
// commands on it. dsess.DoltSession tolerates exactly one outstanding
// command: gcctx panics on a second CommandBegin, and on SessionEnd while
// a command is open. Shadows for one lsid share the session across a
// supersede, so the latch lives here and not on an individual Shadow.
type sessionState struct {
	sess  *dsess.DoltSession
	cmdMu sync.Mutex
}

// Shadow is a connection's handle on a registry session. purged
// distinguishes a Sweep / End teardown (the session was reaped) from a
// supersede (another connection took over the lsid); callers map the two
// to wire codes 251 (NoSuchTransaction) and 225 (TransactionTooOld)
// respectively.
type Shadow struct {
	state    *sessionState
	lastUsed atomic.Int64
	active   atomic.Bool
	purged   atomic.Bool
}

func NewShadow(sess *dsess.DoltSession, now time.Time) *Shadow {
	return newShadow(&sessionState{sess: sess}, now.UnixNano())
}

func newShadow(state *sessionState, lastUsedNanos int64) *Shadow {
	s := &Shadow{state: state}
	s.lastUsed.Store(lastUsedNanos)
	s.active.Store(true)
	return s
}

func (s *Shadow) Session() *dsess.DoltSession { return s.state.sess }

func (s *Shadow) LastUsed() time.Time { return time.Unix(0, s.lastUsed.Load()) }

func (s *Shadow) Active() bool { return s.active.Load() }

// Purged reports whether the shadow's invalidation was caused by a
// Sweep or End teardown (rather than a supersede on the same lsid).
// Meaningful only after Active() returns false.
func (s *Shadow) Purged() bool { return s.purged.Load() }

// invalidate is the Connect supersede path: "a newer connection took
// over." Flag-only, so it does not need the command latch -- an
// in-flight command runs to completion, and the shared latch already
// keeps the superseding shadow's first command from overlapping it.
func (s *Shadow) invalidate() { s.active.Store(false) }

// purge must be called with the command latch held. Used by PurgeNow
// (Sweep, End); the shadow's invalidation is "the session is gone."
func (s *Shadow) purge() {
	s.purged.Store(true)
	s.active.Store(false)
}

func (s *Shadow) Use(now time.Time, fn func(*dsess.DoltSession) error) error {
	return s.run(now, fn)
}

// Commit is Use under the same latch. The handler's Durable commands name
// it explicitly; it no longer differs from Use in fencing.
func (s *Shadow) Commit(now time.Time, fn func(*dsess.DoltSession) error) error {
	return s.run(now, fn)
}

// run holds the command latch across the whole CommandBegin/fn/CommandEnd
// bracket, which is what keeps a concurrent teardown from calling
// SessionEnd underneath a live command.
func (s *Shadow) run(now time.Time, fn func(*dsess.DoltSession) error) error {
	if !s.active.Load() {
		return ErrShadowInvalidated
	}

	s.state.cmdMu.Lock()
	defer s.state.cmdMu.Unlock()

	// Authoritative recheck: purge runs under this latch, so a shadow
	// reaped while this command queued must not reopen the ended session.
	if !s.active.Load() {
		return ErrShadowInvalidated
	}
	s.lastUsed.Store(now.UnixNano())

	if err := s.state.sess.CommandBegin(); err != nil {
		return fmt.Errorf("Shadow.run: CommandBegin: %w", err)
	}
	defer s.state.sess.CommandEnd()

	return fn(s.state.sess)
}
