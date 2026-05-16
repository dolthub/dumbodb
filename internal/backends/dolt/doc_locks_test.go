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
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// idH builds a deterministic hash.Hash from a single byte. Plenty of
// collision-free space for tests.
func idH(b byte) hash.Hash {
	var h hash.Hash
	h[0] = b
	return h
}

// TestAcquireSucceedsForNewIds verifies the basic acquire path: a fresh
// owner taking a fresh set of ids on a fresh collection records every
// requested lock.
func TestAcquireSucceedsForNewIds(t *testing.T) {
	m := NewDocLockManager()
	ids := []hash.Hash{idH(1), idH(2), idH(3)}

	require.NoError(t, m.Acquire("ownerA", "col", ids))
	for _, id := range ids {
		assert.True(t, m.Holds("ownerA", "col", id), "ownerA should hold lock on id %v", id)
	}
}

// TestAcquireBlocksConflictingOwner is parity P4's contract: a second
// owner trying to lock an id held by a different owner gets
// ErrWriteConflict immediately, and no state changes (the second owner
// does not partial-acquire any other id in the request).
func TestAcquireBlocksConflictingOwner(t *testing.T) {
	m := NewDocLockManager()

	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1)}))

	err := m.Acquire("ownerB", "col", []hash.Hash{idH(1), idH(2)})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWriteConflict))

	// ownerB must NOT have acquired id(2) even though it was uncontested
	// in the request -- Acquire is all-or-nothing.
	assert.False(t, m.Holds("ownerB", "col", idH(2)),
		"all-or-nothing: ownerB should not hold id(2) when id(1) conflicted")
}

// TestAcquireNonConflictingIsAllowed verifies that two owners writing to
// different ids in the same collection both succeed. This is parity P5.
func TestAcquireNonConflictingIsAllowed(t *testing.T) {
	m := NewDocLockManager()

	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1)}))
	require.NoError(t, m.Acquire("ownerB", "col", []hash.Hash{idH(2)}))
	assert.True(t, m.Holds("ownerA", "col", idH(1)))
	assert.True(t, m.Holds("ownerB", "col", idH(2)))
}

// TestAcquireIsIdempotentForSameOwner: re-acquiring a lock the owner
// already holds is a no-op (not an error). This matters because a
// transaction may write to the same doc more than once -- each successive
// Acquire should not conflict with itself.
func TestAcquireIsIdempotentForSameOwner(t *testing.T) {
	m := NewDocLockManager()

	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1), idH(2)}))
	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1), idH(2), idH(3)}))
	assert.True(t, m.Holds("ownerA", "col", idH(1)))
	assert.True(t, m.Holds("ownerA", "col", idH(2)))
	assert.True(t, m.Holds("ownerA", "col", idH(3)))
}

// TestReleaseDropsAllOwnedLocks verifies Release covers every collection,
// not just one, and only the owner's own locks (other owners' locks
// remain).
func TestReleaseDropsAllOwnedLocks(t *testing.T) {
	m := NewDocLockManager()

	require.NoError(t, m.Acquire("ownerA", "colX", []hash.Hash{idH(1)}))
	require.NoError(t, m.Acquire("ownerA", "colY", []hash.Hash{idH(2)}))
	require.NoError(t, m.Acquire("ownerB", "colX", []hash.Hash{idH(3)}))

	m.Release("ownerA")
	assert.False(t, m.Holds("ownerA", "colX", idH(1)))
	assert.False(t, m.Holds("ownerA", "colY", idH(2)))
	// ownerB unaffected.
	assert.True(t, m.Holds("ownerB", "colX", idH(3)))

	// After release, ownerB can pick up the formerly-A-held id.
	require.NoError(t, m.Acquire("ownerB", "colX", []hash.Hash{idH(1)}))
	assert.True(t, m.Holds("ownerB", "colX", idH(1)))
}

// TestReleaseNoopForUnknownOwner is a defensive check: releasing an owner
// that holds nothing must not crash.
func TestReleaseNoopForUnknownOwner(t *testing.T) {
	m := NewDocLockManager()
	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1)}))

	m.Release("ownerZ") // never acquired anything
	assert.True(t, m.Holds("ownerA", "col", idH(1)))
}

// TestEmptyOwnerRejected -- the empty string is a "no owner" sentinel that
// Release would otherwise indistinguishably match; Acquire rejects it.
func TestEmptyOwnerRejected(t *testing.T) {
	m := NewDocLockManager()
	err := m.Acquire("", "col", []hash.Hash{idH(1)})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrWriteConflict, "empty owner is a programmer error, not a runtime conflict")
}

// TestAcquireEmptyIdsIsNoop -- a transaction with no writes (e.g. a
// readonly txn) should not error on an empty Acquire.
func TestAcquireEmptyIdsIsNoop(t *testing.T) {
	m := NewDocLockManager()
	require.NoError(t, m.Acquire("ownerA", "col", nil))
	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{}))
}
