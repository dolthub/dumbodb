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

// ErrShadowInvalidated is returned by Shadow.Use and Shadow.Commit when the
// shadow's latch has been flipped (by reconnect supersession or by
// PurgeNow / Sweep). The connection holding this shadow has been
// superseded; the wire dispatcher surfaces this as MongoDB error code 225
// (TransactionTooOld) so drivers treat the session as terminal.
var ErrShadowInvalidated = errors.New("session shadow invalidated by reconnect or sweep")

// Shadow is the data-path reference to a *dsess.DoltSession. It carries an
// atomic latch (active) and an atomic activity timestamp (lastUsed). The
// connection that obtains a Shadow from the registry owns it; the registry
// flips the latch when the lsid is reused by another connection or when an
// idle timeout reaps the entry.
//
// writeMu fences the latch flip against an in-flight commit. The
// registry's supersede / sweep / end paths acquire writeMu before flipping
// active to false, so a commit currently in flight runs to completion
// before any cancellation takes effect. Reads and non-commit writes do not
// touch writeMu -- they live entirely on the atomic latch check.
type Shadow struct {
	sess     *dsess.DoltSession
	lastUsed atomic.Int64 // unix nanoseconds
	active   atomic.Bool  // false once superseded

	writeMu sync.Mutex
}

// NewShadow returns an active Shadow over sess with lastUsed set to now.
func NewShadow(sess *dsess.DoltSession, now time.Time) *Shadow {
	s := &Shadow{sess: sess}
	s.lastUsed.Store(now.UnixNano())
	s.active.Store(true)
	return s
}

// Session returns the underlying *dsess.DoltSession. The pointer remains
// valid for the lifetime of the *Shadow even after the latch is flipped
// (the registry detaches the entry from its map without freeing the
// session), but operations on the session after invalidation are undefined
// and callers should not perform them.
func (s *Shadow) Session() *dsess.DoltSession { return s.sess }

// LastUsed returns the most recent atomically-recorded activity time.
func (s *Shadow) LastUsed() time.Time { return time.Unix(0, s.lastUsed.Load()) }

// Active reports whether the shadow's latch is still set. False after
// supersession, sweep, or end.
func (s *Shadow) Active() bool { return s.active.Load() }

// invalidate flips the latch atomically. Called by the registry's
// supersede / purge paths while holding writeMu (which fences against
// in-flight commits).
func (s *Shadow) invalidate() { s.active.Store(false) }

// Use is the read / non-commit-write hot path. It atomically checks the
// latch, returns ErrShadowInvalidated if the shadow has been superseded,
// records activity in lastUsed, brackets fn with sess.CommandBegin /
// CommandEnd so the GC safepoint controller sees the session as
// in-flight only during fn, and forwards fn's return value.
//
// Use does not acquire writeMu. A concurrent supersede may flip the latch
// while fn is running; that is the design's intent. The supersede only
// becomes visible on the next call.
func (s *Shadow) Use(now time.Time, fn func(*dsess.DoltSession) error) error {
	if !s.active.Load() {
		return ErrShadowInvalidated
	}
	s.lastUsed.Store(now.UnixNano())

	if err := s.sess.CommandBegin(); err != nil {
		return fmt.Errorf("Shadow.Use: CommandBegin: %w", err)
	}
	defer s.sess.CommandEnd()

	return fn(s.sess)
}

// Commit is the durable-write hot path. Like Use, it checks the latch,
// records activity, and brackets fn with CommandBegin / CommandEnd. It
// additionally holds writeMu for the duration of fn so a concurrent
// supersede / sweep / end will wait for the commit to complete before
// flipping the latch. This guarantees that fsync-bearing operations are
// not cancelled mid-flight.
//
// On return the latch may have been flipped while writeMu was held by
// this commit (a reconnect that arrived during the commit blocked on
// writeMu, then ran immediately after); callers that need to chain work
// after the commit should check Active() afterward.
func (s *Shadow) Commit(now time.Time, fn func(*dsess.DoltSession) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if !s.active.Load() {
		return ErrShadowInvalidated
	}
	s.lastUsed.Store(now.UnixNano())

	if err := s.sess.CommandBegin(); err != nil {
		return fmt.Errorf("Shadow.Commit: CommandBegin: %w", err)
	}
	defer s.sess.CommandEnd()

	return fn(s.sess)
}
