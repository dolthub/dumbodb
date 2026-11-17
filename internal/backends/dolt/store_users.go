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

package dolt

import (
	"context"
	"strings"
	"sync"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
)

// While a command runs, each database it reaches counts it as a user, and a
// drop of that database waits for its count to reach zero. Commits and most
// reads use a store without db.mu, so this is what keeps a drop from closing a
// store under them. A command keeps the databases it counted in a
// commandUses: a session's command on its shadow (see Shadow.CommandData), and
// work outside any session, or tracked apart from its session's command, on a
// commandScope carried by its context.

// commandScope tracks one run of background work, such as a capped
// collection cleanup tick, or one transaction start.
type commandScope struct {
	uses commandUses
}

type commandScopeKey struct{}

// commandUses is the databases one running command, or one open
// transaction, counts as a user of.
type commandUses struct {
	mu    sync.Mutex
	dbs   []*dbState
	ended bool
	// cancel stops the command, for a drop of a database it uses; nil for a
	// transaction's uses and for work outside any command.
	cancel context.CancelCauseFunc
}

func (u *commandUses) commandCancel() context.CancelCauseFunc {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cancel
}

func (u *commandUses) setCancel(cancel context.CancelCauseFunc) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.cancel = cancel
}

// usesOf returns the commandUses of the command ctx runs in, or nil outside
// any command. A command in a client transaction counts as a user on behalf
// of the transaction, which keeps the databases it reached until it commits or
// aborts: like mongod, a drop waits for the transaction rather than discarding
// the writes it is about to commit.
func usesOf(ctx context.Context) *commandUses {
	if scope, ok := ctx.Value(commandScopeKey{}).(*commandScope); ok {
		return &scope.uses
	}
	if ci := conninfo.GetIfPresent(ctx); ci != nil {
		if shadow, _ := ci.CachedShadow(); shadow != nil {
			if ci.InTransaction() {
				if txn := transactionUsesOfShadow(shadow); txn != nil {
					return txn
				}
			}
			uses := usesOfShadow(shadow)
			if uses != nil {
				uses.setCancel(ci.CommandCancel())
			}
			return uses
		}
	}
	return nil
}

// transactionUsesOfShadow returns the commandUses the session's client
// transaction holds, or nil for a session the registry did not build.
func transactionUsesOfShadow(shadow *sqlctx.Shadow) *commandUses {
	uses, _ := shadow.SessionData().(*commandUses)
	return uses
}

// Release implements sqlctx.SessionData: a session that ends releases what
// its transaction held.
func (u *commandUses) Release() {
	u.release()
}

// find returns the database named name, compared case-blind as dsess does,
// that the command already counts as a user of, or nil.
func (u *commandUses) find(name string) *dbState {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, db := range u.dbs {
		if strings.EqualFold(db.name, name) {
			return db
		}
	}
	return nil
}

func usesOfShadow(shadow *sqlctx.Shadow) *commandUses {
	uses, _ := shadow.CommandData().(*commandUses)
	return uses
}

// withCommandScope begins a scope of its own for work done with the returned
// context, and returns the function that ends it.
func (b *Backend) withCommandScope(ctx context.Context) (context.Context, func()) {
	scope := &commandScope{}
	return context.WithValue(ctx, commandScopeKey{}, scope), scope.uses.end
}

// ScopeTxnStart implements sqlctx.TxnStartScoper. StartTransaction reads every
// open database; counting it as a user of all of them for the rest of the
// command would make every drop wait for every write.
func (p *dumbodbProvider) ScopeTxnStart(ctx *sql.Context) (*sql.Context, func()) {
	if p.withCommandScope == nil {
		return ctx, func() {}
	}
	scoped, end := p.withCommandScope(ctx.Context)
	return ctx.WithContext(scoped), end
}

// end stops counting the command as a user of any database, for good.
func (u *commandUses) end() {
	u.mu.Lock()
	u.ended = true
	u.mu.Unlock()
	u.release()
}

// release stops counting the command as a user of the databases it has
// reached so far; one it reaches again counts it again. A drop calls it
// before waiting, so it never waits on its own command and two commands that
// each drop a database the other used do not wait on each other.
func (u *commandUses) release() {
	if u == nil {
		return
	}
	u.mu.Lock()
	dbs := u.dbs
	u.dbs = nil
	u.mu.Unlock()
	for _, db := range dbs {
		db.removeLease(u)
	}
}

// noteUser counts the command with uses as a user of db, once per command. It
// reports false, counting nothing, when db is being dropped. Outside any
// command, uses is nil and nothing is counted.
func noteUser(uses *commandUses, db *dbState) bool {
	if uses == nil {
		return !db.isDetached()
	}
	uses.mu.Lock()
	defer uses.mu.Unlock()
	// A command that already counts keeps the database even while it is being
	// dropped: the drop is waiting for it.
	for _, used := range uses.dbs {
		if used == db {
			return true
		}
	}
	if uses.ended {
		return !db.isDetached()
	}
	if !db.addLease(uses) {
		return false
	}
	uses.dbs = append(uses.dbs, db)
	return true
}

// addLease records uses as a user of the store, unless the database is being
// dropped.
func (s *dbState) addLease(uses *commandUses) bool {
	s.leasesMu.Lock()
	defer s.leasesMu.Unlock()
	if s.detached {
		return false
	}
	if s.leases == nil {
		s.leases = map[*commandUses]struct{}{}
	}
	s.leases[uses] = struct{}{}
	return true
}

func (s *dbState) removeLease(uses *commandUses) {
	s.leasesMu.Lock()
	defer s.leasesMu.Unlock()
	delete(s.leases, uses)
	if len(s.leases) == 0 && s.leasesGone != nil {
		close(s.leasesGone)
		s.leasesGone = nil
	}
}

func (s *dbState) isDetached() bool {
	s.leasesMu.Lock()
	defer s.leasesMu.Unlock()
	return s.detached
}

// detach marks the database as being dropped, so no new command counts as its
// user; reattach undoes it. Both run under b.mu.
func (s *dbState) detach() {
	s.leasesMu.Lock()
	s.detached = true
	s.leasesMu.Unlock()
}

func (s *dbState) reattach() {
	s.leasesMu.Lock()
	s.detached = false
	s.leasesMu.Unlock()
}

// cancelCommands cancels, with cause, every command using the store. Open
// transactions are left alone: a drop waits for them, as mongod's does.
func (s *dbState) cancelCommands(cause error) {
	s.leasesMu.Lock()
	var cancels []context.CancelCauseFunc
	for uses := range s.leases {
		if cancel := uses.commandCancel(); cancel != nil {
			cancels = append(cancels, cancel)
		}
	}
	s.leasesMu.Unlock()
	for _, cancel := range cancels {
		cancel(cause)
	}
}

// waitForUsers returns once nothing uses the detached database, or ctx's error
// when ctx ends first.
func (s *dbState) waitForUsers(ctx context.Context) error {
	for {
		s.leasesMu.Lock()
		if len(s.leases) == 0 {
			s.leasesMu.Unlock()
			return nil
		}
		if s.leasesGone == nil {
			s.leasesGone = make(chan struct{})
		}
		gone := s.leasesGone
		s.leasesMu.Unlock()
		select {
		case <-gone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
