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
	"crypto/rand"
	"encoding/binary"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/exp/maps"

	"github.com/dolthub/dumbodb/internal/types"
)

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

	// Only Owner may use or kill the cursor; DB and Collection must also match.
	DB         string
	Collection string
	Owner      string // conninfo.SessionPrincipal of the creator, empty when unauthenticated

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

	// IDs are random so one client cannot guess another's cursors.
	var id int64
	for id == 0 || r.m[id] != nil {
		id = randomCursorID()
	}

	r.l.DebugContext(
		ctx,
		"Creating cursor",
		slog.Int64("id", id),
		slog.String("type", params.Type.String()),
		slog.String("db", params.DB),
		slog.String("collection", params.Collection),
		slog.String("owner", params.Owner),
	)

	c := newCursor(id, iter, params, r)
	r.m[id] = c

	r.wg.Add(1)
	c.stopWatch = context.AfterFunc(ctx, func() { r.CloseAndRemove(c) })

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
	c.stopWatch()
	r.wg.Done()
}

// randomCursorID returns a random positive 63-bit cursor ID.
func randomCursorID() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.LittleEndian.Uint64(b[:]) &^ (1 << 63))
}
