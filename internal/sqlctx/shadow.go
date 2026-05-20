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

// Shadow's writeMu is held by Commit and acquired by the registry's
// invalidate paths so a commit cannot be cancelled mid-fsync. purged
// distinguishes a Sweep / End teardown (the session was reaped) from a
// supersede (another connection took over the lsid); callers map the
// two to wire codes 251 (NoSuchTransaction) and 225 (TransactionTooOld)
// respectively.
type Shadow struct {
	sess     *dsess.DoltSession
	lastUsed atomic.Int64
	active   atomic.Bool
	purged   atomic.Bool

	writeMu sync.Mutex
}

func NewShadow(sess *dsess.DoltSession, now time.Time) *Shadow {
	s := &Shadow{sess: sess}
	s.lastUsed.Store(now.UnixNano())
	s.active.Store(true)
	return s
}

func (s *Shadow) Session() *dsess.DoltSession { return s.sess }

func (s *Shadow) LastUsed() time.Time { return time.Unix(0, s.lastUsed.Load()) }

func (s *Shadow) Active() bool { return s.active.Load() }

// Purged reports whether the shadow's invalidation was caused by a
// Sweep or End teardown (rather than a supersede on the same lsid).
// Meaningful only after Active() returns false.
func (s *Shadow) Purged() bool { return s.purged.Load() }

// invalidate must be called with writeMu held. Used by the Connect
// supersede path; the shadow's invalidation is "a newer connection
// took over." See purge for the teardown variant.
func (s *Shadow) invalidate() { s.active.Store(false) }

// purge must be called with writeMu held. Used by PurgeNow (Sweep,
// End); the shadow's invalidation is "the session is gone."
func (s *Shadow) purge() {
	s.purged.Store(true)
	s.active.Store(false)
}

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

// Commit holds writeMu for fn so invalidate paths block until durable
// writes complete.
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
