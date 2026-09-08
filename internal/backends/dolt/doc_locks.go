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
	"errors"
	"fmt"
	"sync"

	"github.com/dolthub/dolt/go/store/hash"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/types"
)

var ErrWriteConflict = errors.New("write conflict: document locked by another transaction")

// LockKind distinguishes between locks taken for an insert (a write that
// creates a previously-nonexistent document) and locks taken for an
// update or delete (a write to a document that exists in committed state).
// The distinction matters for non-transactional waiters: under MongoDB's
// default read concern an uncommitted insert is not visible to outside
// readers, so a non-txn writer races past an insert-kind lock; whereas
// an update/delete on a committed document does block the non-txn
// writer until the holding transaction ends.
type LockKind int

const (
	LockKindUpdate LockKind = iota
	LockKindInsert
)

type lockEntry struct {
	owner string
	kind  LockKind
}

type DocLockManager struct {
	mu    sync.Mutex
	cond  *sync.Cond
	locks map[string]map[hash.Hash]lockEntry
}

func NewDocLockManager() *DocLockManager {
	m := &DocLockManager{
		locks: map[string]map[hash.Hash]lockEntry{},
	}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func (m *DocLockManager) Acquire(owner string, collection string, ids []hash.Hash, kind LockKind) error {
	if owner == "" {
		// Empty owner means upstream lost the lsid/conn-id; reject so the bug
		// surfaces rather than locking under a sentinel value.
		return errors.New("DocLockManager.Acquire: owner must not be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	collLocks, ok := m.locks[collection]
	if ok {
		for _, id := range ids {
			if existing, held := collLocks[id]; held && existing.owner != owner {
				return ErrWriteConflict
			}
		}
	}

	if collLocks == nil {
		collLocks = map[hash.Hash]lockEntry{}
		m.locks[collection] = collLocks
	}
	for _, id := range ids {
		collLocks[id] = lockEntry{owner: owner, kind: kind}
	}
	return nil
}

func (m *DocLockManager) Release(owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for coll, collLocks := range m.locks {
		for id, entry := range collLocks {
			if entry.owner == owner {
				delete(collLocks, id)
			}
		}
		if len(collLocks) == 0 {
			delete(m.locks, coll)
		}
	}
	m.cond.Broadcast()
}

// WaitForRelease blocks until none of the given ids in collection are held
// by any owner, or until ctx is canceled. Non-transactional writes use this
// to honour MongoDB's semantics that a non-txn write contending with a
// document held by an open multi-doc transaction waits for that transaction
// to commit or abort, rather than racing past the lock.
func (m *DocLockManager) WaitForRelease(ctx context.Context, collection string, ids []hash.Hash) error {
	if len(ids) == 0 {
		return nil
	}

	stopWatcher := make(chan struct{})
	defer close(stopWatcher)
	go func() {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			m.cond.Broadcast()
			m.mu.Unlock()
		case <-stopWatcher:
		}
	}()

	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		if !m.heldLocked(collection, ids) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		m.cond.Wait()
	}
}

// heldLocked reports whether any of ids in collection are held by an
// update-kind lock. Insert-kind locks are deliberately invisible to
// non-transactional waiters because MongoDB's default read concern does
// not expose uncommitted inserts; a contending non-txn write therefore
// races past an in-flight insert (matching MongoDB) rather than blocking.
// Caller must hold m.mu.
func (m *DocLockManager) heldLocked(collection string, ids []hash.Hash) bool {
	collLocks, ok := m.locks[collection]
	if !ok {
		return false
	}
	for _, id := range ids {
		if entry, has := collLocks[id]; has && entry.kind != LockKindInsert {
			return true
		}
	}
	return false
}

// ownerForDocLocks reports the lock owner for a write inside a transaction the
// client opened itself, and whether there is one. Distinct from ownerForTxn,
// which asks whether any session transaction is live: every write forks, so
// that answer is yes for an ordinary write too.
func ownerForDocLocks(ctx context.Context) (string, bool) {
	ci := conninfo.GetIfPresent(ctx)
	if ci == nil {
		return "", false
	}
	if ci.InTransaction() {
		return ci.Owner(), true
	}
	return "", false
}

// Document locks belong to client transactions and to nothing else. A write in
// one Acquires, and fails fast with a WriteConflict when another transaction
// already holds the document, which is how MongoDB reports transaction
// contention.
//
// An ordinary write takes no lock, in any mode. It pins a BASE, accumulates in
// the session overlay, and reconciles at its boundary, where the collection's
// merge mode decides whether two writes to one document agree. A lock would
// decide that first, at write time, and the mode would never run.
// --session-isolation changes when the boundary falls, not what happens at it.
func (b *Backend) acquireTxnLocks(ctx context.Context, db, branch, collection string, ids []hash.Hash, kind LockKind) error {
	if len(ids) == 0 {
		return nil
	}
	owner, inClientTxn := ownerForDocLocks(ctx)
	if !inClientTxn {
		return nil
	}
	if err := b.docLockManager(db, branch).Acquire(owner, collection, ids, kind); err != nil {
		if errors.Is(err, ErrWriteConflict) {
			return backends.NewError(backends.ErrorCodeWriteConflict, err)
		}
		return err
	}
	return nil
}

func idsFromDocs(docs []*types.Document) ([]hash.Hash, error) {
	out := make([]hash.Hash, 0, len(docs))
	for _, d := range docs {
		idVal, err := d.Get("_id")
		if err != nil {
			return nil, fmt.Errorf("document missing _id: %w", err)
		}
		h, err := hashID(idVal)
		if err != nil {
			return nil, fmt.Errorf("hashing _id: %w", err)
		}
		out = append(out, hashFromArray(h))
	}
	return out, nil
}

func idsFromValues(idVals []any) ([]hash.Hash, error) {
	out := make([]hash.Hash, 0, len(idVals))
	for _, v := range idVals {
		h, err := hashID(v)
		if err != nil {
			return nil, fmt.Errorf("hashing _id: %w", err)
		}
		out = append(out, hashFromArray(h))
	}
	return out, nil
}

func (c *collection) acquireInsertLocks(ctx context.Context, docs []*types.Document) error {
	ids, err := idsFromDocs(docs)
	if err != nil {
		return err
	}
	return c.db.backend.acquireTxnLocks(ctx, c.db.name, c.db.rootish, c.name, ids, LockKindInsert)
}

func (c *collection) acquireUpdateLocks(ctx context.Context, docs []*types.Document) error {
	ids, err := idsFromDocs(docs)
	if err != nil {
		return err
	}
	return c.db.backend.acquireTxnLocks(ctx, c.db.name, c.db.rootish, c.name, ids, LockKindUpdate)
}

func (c *collection) acquireDeleteLocks(ctx context.Context, idVals []any) error {
	ids, err := idsFromValues(idVals)
	if err != nil {
		return err
	}
	return c.db.backend.acquireTxnLocks(ctx, c.db.name, c.db.rootish, c.name, ids, LockKindUpdate)
}

func (m *DocLockManager) Holds(owner string, collection string, id hash.Hash) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	collLocks, ok := m.locks[collection]
	if !ok {
		return false
	}
	entry, has := collLocks[id]
	return has && entry.owner == owner
}
