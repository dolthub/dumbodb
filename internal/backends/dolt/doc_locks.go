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
	"github.com/dolthub/dumbodb/internal/types"
)

var ErrWriteConflict = errors.New("write conflict: document locked by another transaction")

type DocLockManager struct {
	mu    sync.Mutex
	locks map[string]map[hash.Hash]string
}

func NewDocLockManager() *DocLockManager {
	return &DocLockManager{
		locks: map[string]map[hash.Hash]string{},
	}
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
			if existing, held := collLocks[id]; held && existing != owner {
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

// No-op outside a txn: implicit single-statement writes serialize on state.mu
// without per-doc locks.
func (b *Backend) acquireTxnLocks(ctx context.Context, db, branch, collection string, ids []hash.Hash) error {
	owner, inTxn := ownerForTxn(ctx)
	if !inTxn || len(ids) == 0 {
		return nil
	}
	mgr := b.docLockManager(db, branch)
	if err := mgr.Acquire(owner, collection, ids); err != nil {
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
	if _, inTxn := ownerForTxn(ctx); !inTxn {
		return nil
	}
	ids, err := idsFromDocs(docs)
	if err != nil {
		return err
	}
	return c.db.backend.acquireTxnLocks(ctx, c.db.name, c.db.rootish, c.name, ids)
}

func (c *collection) acquireUpdateLocks(ctx context.Context, docs []*types.Document) error {
	if _, inTxn := ownerForTxn(ctx); !inTxn {
		return nil
	}
	ids, err := idsFromDocs(docs)
	if err != nil {
		return err
	}
	return c.db.backend.acquireTxnLocks(ctx, c.db.name, c.db.rootish, c.name, ids)
}

func (c *collection) acquireDeleteLocks(ctx context.Context, idVals []any) error {
	if _, inTxn := ownerForTxn(ctx); !inTxn {
		return nil
	}
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
	return collLocks[id] == owner
}
