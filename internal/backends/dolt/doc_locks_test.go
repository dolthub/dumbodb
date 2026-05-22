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
	"testing"
	"time"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func idH(b byte) hash.Hash {
	var h hash.Hash
	h[0] = b
	return h
}

func TestAcquireSucceedsForNewIds(t *testing.T) {
	m := NewDocLockManager()
	ids := []hash.Hash{idH(1), idH(2), idH(3)}

	require.NoError(t, m.Acquire("ownerA", "col", ids, LockKindUpdate))
	for _, id := range ids {
		assert.True(t, m.Holds("ownerA", "col", id), "ownerA should hold lock on id %v", id)
	}
}

func TestAcquireBlocksConflictingOwner(t *testing.T) {
	m := NewDocLockManager()

	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1)}, LockKindUpdate))

	err := m.Acquire("ownerB", "col", []hash.Hash{idH(1), idH(2)}, LockKindUpdate)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWriteConflict))

	assert.False(t, m.Holds("ownerB", "col", idH(2)),
		"all-or-nothing: ownerB should not hold id(2) when id(1) conflicted")
}

func TestAcquireNonConflictingIsAllowed(t *testing.T) {
	m := NewDocLockManager()

	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1)}, LockKindUpdate))
	require.NoError(t, m.Acquire("ownerB", "col", []hash.Hash{idH(2)}, LockKindUpdate))
	assert.True(t, m.Holds("ownerA", "col", idH(1)))
	assert.True(t, m.Holds("ownerB", "col", idH(2)))
}

func TestAcquireIsIdempotentForSameOwner(t *testing.T) {
	m := NewDocLockManager()

	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1), idH(2)}, LockKindUpdate))
	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1), idH(2), idH(3)}, LockKindUpdate))
	assert.True(t, m.Holds("ownerA", "col", idH(1)))
	assert.True(t, m.Holds("ownerA", "col", idH(2)))
	assert.True(t, m.Holds("ownerA", "col", idH(3)))
}

func TestReleaseDropsAllOwnedLocks(t *testing.T) {
	m := NewDocLockManager()

	require.NoError(t, m.Acquire("ownerA", "colX", []hash.Hash{idH(1)}, LockKindUpdate))
	require.NoError(t, m.Acquire("ownerA", "colY", []hash.Hash{idH(2)}, LockKindUpdate))
	require.NoError(t, m.Acquire("ownerB", "colX", []hash.Hash{idH(3)}, LockKindUpdate))

	m.Release("ownerA")
	assert.False(t, m.Holds("ownerA", "colX", idH(1)))
	assert.False(t, m.Holds("ownerA", "colY", idH(2)))
	assert.True(t, m.Holds("ownerB", "colX", idH(3)))

	require.NoError(t, m.Acquire("ownerB", "colX", []hash.Hash{idH(1)}, LockKindUpdate))
	assert.True(t, m.Holds("ownerB", "colX", idH(1)))
}

func TestReleaseNoopForUnknownOwner(t *testing.T) {
	m := NewDocLockManager()
	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1)}, LockKindUpdate))

	m.Release("ownerZ")
	assert.True(t, m.Holds("ownerA", "col", idH(1)))
}

func TestEmptyOwnerRejected(t *testing.T) {
	m := NewDocLockManager()
	err := m.Acquire("", "col", []hash.Hash{idH(1)}, LockKindUpdate)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrWriteConflict, "empty owner is a programmer error, not a runtime conflict")
}

func TestAcquireEmptyIdsIsNoop(t *testing.T) {
	m := NewDocLockManager()
	require.NoError(t, m.Acquire("ownerA", "col", nil, LockKindUpdate))
	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{}, LockKindUpdate))
}

func TestAcquireInsertVsTxnInsertConflicts(t *testing.T) {
	// Two concurrent transactions inserting the same _id must conflict at the
	// lock manager (matches MongoDB's WriteConflict / TransientTransactionError
	// for txn-vs-txn duplicate-key races), even though insert-kind locks are
	// invisible to non-txn waiters.
	m := NewDocLockManager()
	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1)}, LockKindInsert))
	err := m.Acquire("ownerB", "col", []hash.Hash{idH(1)}, LockKindInsert)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWriteConflict))
}

func TestWaitForReleaseReturnsImmediatelyWhenUnheld(t *testing.T) {
	m := NewDocLockManager()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	start := time.Now()
	require.NoError(t, m.WaitForRelease(ctx, "col", []hash.Hash{idH(1)}))
	assert.Less(t, time.Since(start), 50*time.Millisecond)
}

func TestWaitForReleaseBlocksUntilReleased(t *testing.T) {
	m := NewDocLockManager()
	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1)}, LockKindUpdate))

	go func() {
		time.Sleep(100 * time.Millisecond)
		m.Release("ownerA")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	require.NoError(t, m.WaitForRelease(ctx, "col", []hash.Hash{idH(1)}))
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 80*time.Millisecond, "should have waited for the release")
	assert.Less(t, elapsed, 500*time.Millisecond, "should have unblocked promptly after release")
}

func TestWaitForReleaseRespectsContextCancellation(t *testing.T) {
	m := NewDocLockManager()
	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1)}, LockKindUpdate))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := m.WaitForRelease(ctx, "col", []hash.Hash{idH(1)})
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.GreaterOrEqual(t, elapsed, 80*time.Millisecond)
	assert.Less(t, elapsed, 500*time.Millisecond)
}

func TestWaitForReleaseEmptyIdsIsNoop(t *testing.T) {
	m := NewDocLockManager()
	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1)}, LockKindUpdate))
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	require.NoError(t, m.WaitForRelease(ctx, "col", nil))
	require.NoError(t, m.WaitForRelease(ctx, "col", []hash.Hash{}))
}

func TestWaitForReleaseSkipsInsertKindLocks(t *testing.T) {
	// Insert-kind locks model in-flight uncommitted inserts. Outside readers
	// in MongoDB do not see uncommitted inserts under default read concern,
	// so a non-txn waiter races past the insert rather than blocking on it.
	m := NewDocLockManager()
	require.NoError(t, m.Acquire("ownerA", "col", []hash.Hash{idH(1)}, LockKindInsert))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	require.NoError(t, m.WaitForRelease(ctx, "col", []hash.Hash{idH(1)}))
	assert.Less(t, time.Since(start), 50*time.Millisecond, "should not have waited on an insert-kind lock")
}
