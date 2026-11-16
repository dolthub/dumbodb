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

	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/go-mysql-server/sql"
)

// A command is identified by its session, or by a commandScope for work that
// runs outside any session or is tracked apart from its session's command.
// While it runs, each database it reaches counts it as a user, and a drop of
// that database waits for its count to reach zero. Commits and most reads use
// a store without db.mu, so this is what keeps a drop from closing a store
// under them.

// commandScope identifies one run of background work, such as a capped
// collection cleanup tick, or one transaction start. It is not zero-sized:
// pointers to distinct zero-sized values may be equal, and each scope must be
// a distinct key.
type commandScope struct{ _ byte }

type commandScopeKey struct{}

// commandUses is the databases one running command counts as a user of.
type commandUses struct {
	mu  sync.Mutex
	dbs []*dbState
}

// commandKey returns the command ctx runs in, or nil outside any command.
func commandKey(ctx context.Context) any {
	if scope, ok := ctx.Value(commandScopeKey{}).(*commandScope); ok {
		return scope
	}
	if sqlCtx, ok := ctx.(*sql.Context); ok {
		if sess, ok := sqlCtx.Session.(*dsess.DoltSession); ok {
			return sess
		}
	}
	if sess := sessionFromContext(ctx); sess != nil {
		return sess
	}
	return nil
}

// withCommandScope begins a scope of its own for work done with the returned
// context, and returns the function that ends it.
func (b *Backend) withCommandScope(ctx context.Context) (context.Context, func()) {
	scope := &commandScope{}
	b.beginCommand(scope)
	return context.WithValue(ctx, commandScopeKey{}, scope), func() { b.endCommand(scope) }
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

// beginCommand starts counting the command key as a user of the databases it
// reaches.
func (b *Backend) beginCommand(key any) {
	b.commands.Store(key, &commandUses{})
}

// endCommand stops counting the command key as a user of any database.
func (b *Backend) endCommand(key any) {
	if uses, ok := b.commands.LoadAndDelete(key); ok {
		uses.(*commandUses).release()
	}
}

// releaseUses stops counting the running command key as a user of the
// databases it has reached so far; one it reaches again counts it again. A
// drop calls it before waiting, so it never waits on its own command and two
// commands that each drop a database the other used do not wait on each other.
func (b *Backend) releaseUses(key any) {
	if uses, ok := b.commands.Load(key); ok {
		uses.(*commandUses).release()
	}
}

func (u *commandUses) release() {
	u.mu.Lock()
	dbs := u.dbs
	u.dbs = nil
	u.mu.Unlock()
	for _, db := range dbs {
		db.removeUser()
	}
}

// noteUser counts the command key as a user of db, once per command. It
// reports false, counting nothing, when db is being dropped. A key outside
// any command counts nothing.
func (b *Backend) noteUser(key any, db *dbState) bool {
	if db.detached.Load() {
		return false
	}
	if key == nil {
		return true
	}
	value, running := b.commands.Load(key)
	if !running {
		return true
	}
	uses := value.(*commandUses)
	uses.mu.Lock()
	defer uses.mu.Unlock()
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
