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
	"fmt"
	"sync"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/hash"
)

// branchWS is the singleton entry for one branch's working-set pointer.
// Entries are lazily created by branchEntry on first access and never
// removed for the life of the dbState. Branch deletion clears ws and
// wsHash but leaves the entry in place so a subsequent re-create of
// the branch name reuses the same entry pointer.
//
// wsHash is the on-disk hash of ws -- the optimistic-lock value for
// the next UpdateWorkingSet call. (*doltdb.WorkingSet).HashOf() only
// works for WSes loaded from disk; for in-memory WSes built via
// WithWorkingRoot it returns an error. Tracking the hash explicitly
// avoids a per-write ResolveWorkingSet round trip just to get prevHash.
type branchWS struct {
	mu     sync.RWMutex
	ws     *doltdb.WorkingSet
	wsHash hash.Hash
}

// branchEntry returns the singleton entry for branch, lazily creating
// it on first call. The returned pointer is stable for the life of
// the dbState; concurrent callers see the same pointer.
func (s *dbState) branchEntry(branch string) *branchWS {
	s.branchWSMu.RLock()
	e, ok := s.branchWS[branch]
	s.branchWSMu.RUnlock()
	if ok {
		return e
	}

	s.branchWSMu.Lock()
	defer s.branchWSMu.Unlock()
	if e, ok = s.branchWS[branch]; ok {
		return e
	}
	e = &branchWS{}
	s.branchWS[branch] = e
	return e
}

// loadBranchWS returns the cached working set for branch, resolving
// from disk on first access (or after the entry was cleared by
// branch deletion). The post-load entry has both ws and wsHash
// populated, so subsequent reads hit the entry.RLock fast path.
func (s *dbState) loadBranchWS(ctx context.Context, branch string) (*doltdb.WorkingSet, error) {
	e := s.branchEntry(branch)

	e.mu.RLock()
	if e.ws != nil {
		ws := e.ws
		e.mu.RUnlock()
		return ws, nil
	}
	e.mu.RUnlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ws != nil {
		return e.ws, nil
	}

	wsRef := doltref.NewWorkingSetRef("heads/" + branch)
	ws, err := s.doltDB.ResolveWorkingSet(ctx, wsRef)
	if err != nil {
		return nil, fmt.Errorf("loadBranchWS: resolving %q: %w", branch, err)
	}
	h, err := ws.HashOf()
	if err != nil {
		return nil, fmt.Errorf("loadBranchWS: hashing %q: %w", branch, err)
	}
	e.ws = ws
	e.wsHash = h
	return ws, nil
}

// updateBranchWS atomically reads, mutates, and persists the working
// set for branch. It holds entry.mu.Lock across the whole sequence so
// two writers on the same branch serialize.
//
// Steps:
//  1. Lazy-load the entry if cold.
//  2. fn(currentWS) computes the new WS.
//  3. ddb.UpdateWorkingSet writes to disk using entry.wsHash as the
//     optimistic-lock prevHash. Returns ErrOptimisticLockFailed if
//     disk moved (e.g., another process committed); the caller
//     decides whether to retry.
//  4. On success, refresh entry.wsHash by re-resolving from disk
//     (the WS chunk is in NBS memtable; the round trip is in-memory).
//
// fn must not retain references to the WorkingSet pointer it receives
// beyond its own return; the entry takes ownership of the new WS.
func (s *dbState) updateBranchWS(
	ctx context.Context,
	branch string,
	fn func(*doltdb.WorkingSet) (*doltdb.WorkingSet, error),
) error {
	e := s.branchEntry(branch)

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.ws == nil {
		wsRef := doltref.NewWorkingSetRef("heads/" + branch)
		ws, err := s.doltDB.ResolveWorkingSet(ctx, wsRef)
		if err != nil {
			return fmt.Errorf("updateBranchWS: resolving %q: %w", branch, err)
		}
		h, herr := ws.HashOf()
		if herr != nil {
			return fmt.Errorf("updateBranchWS: hashing %q: %w", branch, herr)
		}
		e.ws = ws
		e.wsHash = h
	}

	newWS, err := fn(e.ws)
	if err != nil {
		return err
	}
	if newWS == nil {
		return fmt.Errorf("updateBranchWS: fn returned nil WorkingSet for %q", branch)
	}

	wsRef := doltref.NewWorkingSetRef("heads/" + branch)
	meta := doltdb.TodoWorkingSetMeta()
	var rsc doltdb.ReplicationStatusController
	if err := s.doltDB.UpdateWorkingSet(ctx, wsRef, newWS, e.wsHash, meta, &rsc); err != nil {
		return fmt.Errorf("updateBranchWS: UpdateWorkingSet for %q: %w", branch, err)
	}

	// Refresh wsHash from disk so subsequent updates have a fresh
	// optimistic-lock value. The chunk is in NBS memtable; this is
	// an in-memory round trip.
	persisted, err := s.doltDB.ResolveWorkingSet(ctx, wsRef)
	if err != nil {
		return fmt.Errorf("updateBranchWS: post-write resolve for %q: %w", branch, err)
	}
	newHash, err := persisted.HashOf()
	if err != nil {
		return fmt.Errorf("updateBranchWS: post-write hash for %q: %w", branch, err)
	}
	e.ws = persisted
	e.wsHash = newHash
	return nil
}
