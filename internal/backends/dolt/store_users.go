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

	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/go-mysql-server/sql"
)

// A command is identified by its session, or by a commandScope for work that
// runs outside any session or is tracked apart from its session's command. While it runs, every database it reaches is
// recorded as one of its stores in use, and a drop of that database waits for
// it to end. Commits and most reads use a store without db.mu, so this is what
// keeps a drop from closing a store under them.

// commandScope identifies one run of background work, such as a capped
// collection cleanup tick, or one transaction start. It is not zero-sized:
// pointers to distinct zero-sized values may be equal, and each scope must be
// a distinct key.
type commandScope struct{ _ byte }

type commandScopeKey struct{}

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
// open database; recording them all as used for the rest of the command would
// make every drop wait for every write.
func (p *dumbodbProvider) ScopeTxnStart(ctx *sql.Context) (*sql.Context, func()) {
	if p.withCommandScope == nil {
		return ctx, func() {}
	}
	scoped, end := p.withCommandScope(ctx.Context)
	return ctx.WithContext(scoped), end
}

// beginCommand starts recording the stores the command key uses.
func (b *Backend) beginCommand(key any) {
	b.commandsMu.Lock()
	defer b.commandsMu.Unlock()
	b.commands[key] = map[*dbState]struct{}{}
}

// endCommand releases every store the command key used.
func (b *Backend) endCommand(key any) {
	b.commandsMu.Lock()
	used := b.commands[key]
	delete(b.commands, key)
	b.commandsMu.Unlock()

	for db := range used {
		db.releaseUser(key)
	}
}

// releaseUses releases every store the running command key has used so far;
// a store it reaches again is recorded again. A drop calls it before waiting,
// so two commands that each drop a database the other used do not wait on
// each other.
func (b *Backend) releaseUses(key any) {
	b.commandsMu.Lock()
	used, running := b.commands[key]
	if running {
		b.commands[key] = map[*dbState]struct{}{}
	}
	b.commandsMu.Unlock()

	for db := range used {
		db.releaseUser(key)
	}
}

// noteUserLocked records that the command key uses db. A key outside any
// command records nothing. The caller holds b.mu, so a drop cannot detach db
// between the lookup and this record.
func (b *Backend) noteUserLocked(key any, db *dbState) {
	if key == nil || db == nil {
		return
	}
	b.commandsMu.Lock()
	defer b.commandsMu.Unlock()

	used, running := b.commands[key]
	if !running {
		return
	}
	if _, ok := used[db]; ok {
		return
	}
	used[db] = struct{}{}
	db.addUser(key)
}

func (s *dbState) addUser(key any) {
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	if s.users == nil {
		s.users = map[any]struct{}{}
	}
	s.users[key] = struct{}{}
}

func (s *dbState) releaseUser(key any) {
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	delete(s.users, key)
	if s.usersChanged != nil {
		close(s.usersChanged)
		s.usersChanged = nil
	}
}

// waitForOtherUsers returns once no command but self uses the store, or ctx's
// error when ctx ends first.
func (s *dbState) waitForOtherUsers(ctx context.Context, self any) error {
	for {
		s.usersMu.Lock()
		busy := false
		for key := range s.users {
			if key != self {
				busy = true
				break
			}
		}
		if !busy {
			s.usersMu.Unlock()
			return nil
		}
		if s.usersChanged == nil {
			s.usersChanged = make(chan struct{})
		}
		changed := s.usersChanged
		s.usersMu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
