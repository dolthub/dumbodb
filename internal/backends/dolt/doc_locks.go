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
	"errors"
	"sync"

	"github.com/dolthub/dolt/go/store/hash"
)

// ErrWriteConflict is the sentinel returned by DocLockManager.Acquire when
// any of the requested docIDs is held by a different owner. msg_transaction
// translates it to MongoDB wire error code 112 ("WriteConflict").
//
// Lock policy is currently no-wait: a conflict is reported immediately.
// Parity test P4 in dumbodb-parity-testing pins the eventual semantics
// (no-wait vs bounded-wait); start simple and adjust if Mongo's behavior
// diverges.
var ErrWriteConflict = errors.New("write conflict: document locked by another transaction")

// DocLockManager provides pessimistic per-document locks for default-mode
// MongoDB transactions. One instance is held per (database, branch) by the
// Backend; the design treats document-level locking as Mongo-specific
// semantics layered above dsess (which is optimistic-merge-on-commit).
//
// Concurrency:
//   - Acquire is all-or-nothing. If any requested id collides with another
//     owner, the call returns ErrWriteConflict and no locks are taken.
//   - Release drops every lock held by the given owner across every
//     collection in this manager.
//   - Acquire is also idempotent for the same owner: an owner re-acquiring
//     a lock it already holds is a no-op.
//
// Owner is a string -- typically the MongoDB lsid for default-mode txns,
// or a per-connection synthetic id when no lsid is present.
type DocLockManager struct {
	mu sync.Mutex
	// locks: collection name -> docID hash -> owner string.
	locks map[string]map[hash.Hash]string
}

// NewDocLockManager constructs an empty manager.
func NewDocLockManager() *DocLockManager {
	return &DocLockManager{
		locks: map[string]map[hash.Hash]string{},
	}
}

// Acquire attempts to lock all of |ids| in |collection| for |owner|. On any
// collision with a different owner the call returns ErrWriteConflict and
// the manager state is unchanged (no partial locks are recorded). Locks
// the owner already holds count as success.
func (m *DocLockManager) Acquire(owner string, collection string, ids []hash.Hash) error {
	if owner == "" {
		// Empty owner would be indistinguishable from "no owner" on
		// release; reject it rather than silently leak.
		return errors.New("DocLockManager.Acquire: owner must not be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	collLocks, ok := m.locks[collection]
	if ok {
		// Probe for any collision first, so we don't write partial state.
		for _, id := range ids {
			if existing, held := collLocks[id]; held && existing != owner {
				return ErrWriteConflict
			}
		}
	}

	// All clear -- record (or refresh) every lock.
	if collLocks == nil {
		collLocks = map[hash.Hash]string{}
		m.locks[collection] = collLocks
	}
	for _, id := range ids {
		collLocks[id] = owner
	}
	return nil
}

// Release drops every lock held by |owner| across every collection. Safe
// to call for an owner that holds no locks.
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

// Holds reports whether |owner| currently holds a lock on |id| in
// |collection|. Used in tests to verify acquire/release behavior.
func (m *DocLockManager) Holds(owner string, collection string, id hash.Hash) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	collLocks, ok := m.locks[collection]
	if !ok {
		return false
	}
	return collLocks[id] == owner
}
