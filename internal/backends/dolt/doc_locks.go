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

// DocLockManager holds one owner per locked document, keyed by collection.
// Only a client transaction takes a lock; see acquireTxnLocks.
type DocLockManager struct {
	mu    sync.Mutex
	locks map[string]map[hash.Hash]string
}

func NewDocLockManager() *DocLockManager {
	return &DocLockManager{locks: map[string]map[hash.Hash]string{}}
}

func (m *DocLockManager) Acquire(owner string, collection string, ids []hash.Hash) error {
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
			if holder, held := collLocks[id]; held && holder != owner {
				return ErrWriteConflict
			}
		}
	}

	if collLocks == nil {
		collLocks = map[hash.Hash]string{}
		m.locks[collection] = collLocks
	}
	for _, id := range ids {
		collLocks[id] = owner
	}
	return nil
}

func (m *DocLockManager) Release(owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for coll, collLocks := range m.locks {
		for id, holder := range collLocks {
			if holder == owner {
				delete(collLocks, id)
			}
		}
		if len(collLocks) == 0 {
			delete(m.locks, coll)
		}
	}
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
func (b *Backend) acquireTxnLocks(ctx context.Context, db, branch, collection string, ids []hash.Hash) error {
	if len(ids) == 0 {
		return nil
	}
	owner, inClientTxn := ownerForDocLocks(ctx)
	if !inClientTxn {
		return nil
	}
	if err := b.docLockManager(db, branch).Acquire(owner, collection, ids); err != nil {
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
	return c.db.backend.acquireTxnLocks(ctx, c.db.name, c.db.rootish, c.name, ids)
}

func (c *collection) acquireUpdateLocks(ctx context.Context, docs []*types.Document) error {
	ids, err := idsFromDocs(docs)
	if err != nil {
		return err
	}
	return c.db.backend.acquireTxnLocks(ctx, c.db.name, c.db.rootish, c.name, ids)
}

func (c *collection) acquireDeleteLocks(ctx context.Context, idVals []any) error {
	ids, err := idsFromValues(idVals)
	if err != nil {
		return err
	}
	return c.db.backend.acquireTxnLocks(ctx, c.db.name, c.db.rootish, c.name, ids)
}

func (m *DocLockManager) Holds(owner string, collection string, id hash.Hash) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	collLocks, ok := m.locks[collection]
	if !ok {
		return false
	}
	holder, has := collLocks[id]
	return has && holder == owner
}
