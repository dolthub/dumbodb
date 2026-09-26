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
)

func TestWaitUntilFree_ImmediateWhenUnlocked(t *testing.T) {
	m := NewDocLockManager()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.WaitUntilFree(ctx, "c", []hash.Hash{{1}}); err != nil {
		t.Fatalf("want nil for an unlocked document, got %v", err)
	}
}

func TestWaitUntilFree_BlocksThenReturnsAfterRelease(t *testing.T) {
	m := NewDocLockManager()
	id := hash.Hash{1}
	if err := m.Acquire("txn", "c", []hash.Hash{id}, true); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- m.WaitUntilFree(ctx, "c", []hash.Hash{id})
	}()

	select {
	case <-done:
		t.Fatal("WaitUntilFree returned before the lock was released")
	case <-time.After(150 * time.Millisecond):
	}

	m.Release("txn")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("want nil after release, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitUntilFree did not return after release")
	}
}

func TestWaitUntilFree_HonorsContextDeadline(t *testing.T) {
	m := NewDocLockManager()
	id := hash.Hash{2}
	if err := m.Acquire("txn", "c", []hash.Hash{id}, true); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := m.WaitUntilFree(ctx, "c", []hash.Hash{id})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded while the doc stays locked, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("returned before the deadline (%s)", elapsed)
	}
}

func TestWaitUntilFree_UnaffectedByOtherDocument(t *testing.T) {
	m := NewDocLockManager()
	if err := m.Acquire("txn", "c", []hash.Hash{{9}}, true); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.WaitUntilFree(ctx, "c", []hash.Hash{{1}}); err != nil {
		t.Fatalf("a lock on a different document must not block: %v", err)
	}
}

// An insert lock covers a not-yet-committed new _id, invisible to other
// connections, so an ordinary write must not wait on it -- matching MongoDB,
// where an upsert racing a transaction's insert does not block (workspace-s1s).
func TestWaitUntilFree_IgnoresNonWaitableInsertLock(t *testing.T) {
	m := NewDocLockManager()
	id := hash.Hash{3}
	// waitable=false models a transaction's insert of a new document.
	if err := m.Acquire("txn", "c", []hash.Hash{id}, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := m.WaitUntilFree(ctx, "c", []hash.Hash{id}); err != nil {
		t.Fatalf("an insert lock must not block an ordinary write, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("returned after %s; an insert lock should not cause any wait", elapsed)
	}
}
