// Copyright 2021 FerretDB Inc.
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

package cursor

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/exp/maps"

	"github.com/dolthub/dumbodb/internal/types"
)

// Global last cursor ID.
var lastCursorID atomic.Uint32

func init() {
	lastCursorID.Store(rand.Uint32())
}

// Registry stores cursors.
//
//nolint:vet // for readability
type Registry struct {
	rw sync.RWMutex
	m  map[int64]*Cursor

	l  *slog.Logger
	wg sync.WaitGroup
}

func NewRegistry(l *slog.Logger) *Registry {
	return &Registry{
		m: map[int64]*Cursor{},
		l: l,
	}
}

// Close waits for all cursors to be closed and removed from the registry.
func (r *Registry) Close() {
	r.wg.Wait()
}

type NewParams struct {
	// Data stored, but not used by this package.
	// Used to pass *handler.findCursorData between `find` and `getMore` command implementations.
	// Stored as any to avoid dependency cycle.
	Data any

	// those fields are used for limited authorization checks
	// before we implement proper authz and/or sessions
	DB         string
	Collection string
	Username   string

	Type         Type
	ShowRecordID bool

	_ struct{} // prevent unkeyed literals
}

// NewCursor creates and stores a new cursor.
//
// The cursor of any type will be closed automatically when a given context is canceled,
// even if the cursor is not being used at that time.
func (r *Registry) NewCursor(ctx context.Context, iter types.DocumentsIterator, params *NewParams) *Cursor {
	r.rw.Lock()
	defer r.rw.Unlock()

	// use global, sequential, positive, short cursor IDs to make debugging easier
	var id int64
	for id == 0 || r.m[id] != nil {
		id = int64(lastCursorID.Add(1))
	}

	r.l.DebugContext(
		ctx,
		"Creating cursor",
		slog.Int64("id", id),
		slog.String("type", params.Type.String()),
		slog.String("db", params.DB),
		slog.String("collection", params.Collection),
		slog.String("username", params.Username),
	)

	c := newCursor(id, iter, params, r)
	r.m[id] = c

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		select {
		case <-ctx.Done():
			r.CloseAndRemove(c)
		case <-c.removed: // for c.Close() and normal cursors
		}

		<-c.removed
	}()

	return c
}

// Get returns stored cursor by ID, or nil.
func (r *Registry) Get(id int64) *Cursor {
	r.rw.RLock()
	defer r.rw.RUnlock()

	return r.m[id]
}

// All returns a shallow copy of all stored cursors.
func (r *Registry) All() []*Cursor {
	r.rw.RLock()
	defer r.rw.RUnlock()

	return maps.Values(r.m)
}

func (r *Registry) CloseAndRemove(c *Cursor) {
	c.Close()

	r.rw.Lock()
	defer r.rw.Unlock()

	if r.m[c.ID] == nil {
		return
	}

	d := time.Since(c.created)
	r.l.Debug(
		"Removing cursor",
		slog.Int64("id", c.ID),
		slog.String("type", c.Type.String()),
		slog.Int("total", len(r.m)),
		slog.Duration("duration", d),
	)

	delete(r.m, c.ID)
	close(c.removed)
}
