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
	"sync"
	"time"

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

// commandUses is the databases one running command counts as a user of.
type commandUses struct {
	mu    sync.Mutex
	dbs   []*dbState
	ended bool
}

// usesOf returns the commandUses of the command ctx runs in, or nil outside
// any command.
func usesOf(ctx context.Context) *commandUses {
	if scope, ok := ctx.Value(commandScopeKey{}).(*commandScope); ok {
		return &scope.uses
	}
	if ci := conninfo.GetIfPresent(ctx); ci != nil {
		if shadow, _ := ci.CachedShadow(); shadow != nil {
			return usesOfShadow(shadow)
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
		db.removeUser()
	}
}

// noteUser counts the command with uses as a user of db, once per command. It
// reports false, counting nothing, when db is being dropped. Outside any
// command, uses is nil and nothing is counted.
func noteUser(uses *commandUses, db *dbState) bool {
	if db.detached.Load() {
		return false
	}
	if uses == nil {
		return true
	}
	uses.mu.Lock()
	defer uses.mu.Unlock()
	if uses.ended {
		return true
	}
	for _, used := range uses.dbs {
		if used == db {
			return true
		}
	}
	// Count first, then check: the drop marks db detached first, then reads
	// the count, so at least one of the two sees the other.
	db.users.Add(1)
	if db.detached.Load() {
		db.removeUser()
		return false
	}
	uses.dbs = append(uses.dbs, db)
	return true
}

func (s *dbState) removeUser() {
	if s.users.Add(-1) == 0 && s.detached.Load() {
		select {
		case s.usersGone <- struct{}{}:
		default:
		}
	}
}

// detach marks the database as being dropped, so no new command counts as its
// user; reattach undoes it. Both run under b.mu.
func (s *dbState) detach() {
	s.detached.Store(true)
}

func (s *dbState) reattach() {
	s.detached.Store(false)
}

// waitForUsers returns once no command counts as a user of the detached
// database, or ctx's error when ctx ends first.
func (s *dbState) waitForUsers(ctx context.Context) error {
	for s.users.Load() > 0 {
		select {
		case <-s.usersGone:
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
