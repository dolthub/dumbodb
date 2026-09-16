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
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"

	"github.com/dolthub/dumbodb/internal/backends"
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

// setBranchWS updates the cached WS for branch without writing to
// disk. entry.wsHash is unchanged (the on-disk WS has not moved),
// which keeps it correct for the next updateBranchWS's optimistic
// lock. Used by version-control flows that update the in-memory
// cache and persist via a separate path (e.g., the setAM/persistAM
// split, or merge-conflict staging that updates state in-memory
// before the saved merge-state JSON drives the disk write).
func (s *dbState) setBranchWS(branch string, ws *doltdb.WorkingSet) {
	e := s.branchEntry(branch)
	e.mu.Lock()
	e.ws = ws
	e.mu.Unlock()
}

// clearBranchWS nils the entry's ws and wsHash. Used by branch
// deletion; the entry pointer stays in the map so a subsequent
// recreate of the branch name reuses the same entry (loadBranchWS
// will resolve from disk into it).
func (s *dbState) clearBranchWS(branch string) {
	e := s.branchEntry(branch)
	e.mu.Lock()
	e.ws = nil
	e.wsHash = hash.Hash{}
	e.mu.Unlock()
}

// reloadBranchWSFromDisk re-resolves the entry from doltDB. Used
// when an out-of-band path advanced disk and the cache needs to
// catch up (e.g., after dsess.CommitWorkingSet mirrors a merged WS
// onto the session, the dbState cache must follow to keep
// non-session readers current).
func (s *dbState) reloadBranchWSFromDisk(ctx context.Context, branch string) error {
	wsRef := doltref.NewWorkingSetRef("heads/" + branch)
	ws, err := s.doltDB.ResolveWorkingSet(ctx, wsRef)
	if err != nil {
		return fmt.Errorf("reloadBranchWSFromDisk: resolving %q: %w", branch, err)
	}
	h, err := ws.HashOf()
	if err != nil {
		return fmt.Errorf("reloadBranchWSFromDisk: hashing %q: %w", branch, err)
	}
	e := s.branchEntry(branch)
	e.mu.Lock()
	e.ws = ws
	e.wsHash = h
	e.mu.Unlock()
	return nil
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

// commitBranchRoot publishes commitRV as a new commit on branch together with
// the working set fn returns it, in a single atomic update. Version-control
// operations finish through here rather than committing and then updating the
// working set separately: a crash between two such writes leaves a finished
// commit on a branch whose working set still advertises the operation, and
// neither continue nor abort can make sense of that afterwards.
//
// parents is the new commit's complete parent list. fn receives the working set
// already pointed at commitRV and returns the one to publish with the commit.
// Caller must hold state.mu.
func (s *dbState) commitBranchRoot(
	ctx context.Context,
	branch string,
	commitRV doltdb.RootValue,
	parents []hash.Hash,
	cm *datas.CommitMeta,
	fn func(*doltdb.WorkingSet) (*doltdb.WorkingSet, error),
) (hash.Hash, error) {
	commitHash, published, err := s.commitBranchRootLocked(ctx, branch, commitRV, parents, cm, fn)
	if err != nil {
		return hash.Hash{}, err
	}
	s.pushWSToSession(ctx, branch, published)
	return commitHash, nil
}

func (s *dbState) commitBranchRootLocked(
	ctx context.Context,
	branch string,
	commitRV doltdb.RootValue,
	parents []hash.Hash,
	cm *datas.CommitMeta,
	fn func(*doltdb.WorkingSet) (*doltdb.WorkingSet, error),
) (hash.Hash, *doltdb.WorkingSet, error) {
	e := s.branchEntry(branch)
	e.mu.Lock()
	defer e.mu.Unlock()

	wsRef := doltref.NewWorkingSetRef("heads/" + branch)
	if e.ws == nil {
		ws, err := s.doltDB.ResolveWorkingSet(ctx, wsRef)
		if err != nil {
			// A branch whose working set never reached disk still has to be
			// committable; an empty one anchors the optimistic lock at zero.
			e.ws = doltdb.EmptyWorkingSet(wsRef).WithWorkingRoot(commitRV).WithStagedRoot(commitRV)
			e.wsHash = hash.Hash{}
		} else {
			h, hErr := ws.HashOf()
			if hErr != nil {
				return hash.Hash{}, nil, fmt.Errorf("commitBranchRoot: hashing %q: %w", branch, hErr)
			}
			e.ws = ws
			e.wsHash = h
		}
	}

	headRoot, err := headRootValueForBranch(ctx, s, branch)
	if err != nil {
		return hash.Hash{}, nil, fmt.Errorf("commitBranchRoot: reading HEAD root for %q: %w", branch, err)
	}

	parentCommits := make([]*doltdb.Commit, 0, len(parents))
	for _, p := range parents {
		c, cErr := commitForHash(ctx, s, p)
		if cErr != nil {
			return hash.Hash{}, nil, fmt.Errorf("commitBranchRoot: %w", cErr)
		}
		if c != nil {
			parentCommits = append(parentCommits, c)
		}
	}

	pending, err := s.doltDB.NewPendingCommit(ctx, doltdb.Roots{
		Head:    headRoot,
		Working: commitRV,
		Staged:  commitRV,
	}, parentCommits, hash.Hash{}, cm)
	if err != nil {
		return hash.Hash{}, nil, fmt.Errorf("commitBranchRoot: pending commit for %q: %w", branch, err)
	}

	newWS, err := fn(e.ws.WithWorkingRoot(commitRV).WithStagedRoot(commitRV))
	if err != nil {
		return hash.Hash{}, nil, err
	}

	headRef, err := wsRef.ToHeadRef()
	if err != nil {
		return hash.Hash{}, nil, fmt.Errorf("commitBranchRoot: head ref for %q: %w", branch, err)
	}
	var rsc doltdb.ReplicationStatusController
	commit, err := s.doltDB.CommitWithWorkingSet(ctx, headRef, wsRef, pending, newWS, e.wsHash, doltdb.TodoWorkingSetMeta(), &rsc)
	if err != nil {
		return hash.Hash{}, nil, fmt.Errorf("commitBranchRoot: committing %q: %w", branch, err)
	}

	persisted, err := s.doltDB.ResolveWorkingSet(ctx, wsRef)
	if err != nil {
		return hash.Hash{}, nil, fmt.Errorf("commitBranchRoot: post-commit resolve for %q: %w", branch, err)
	}
	newHash, err := persisted.HashOf()
	if err != nil {
		return hash.Hash{}, nil, fmt.Errorf("commitBranchRoot: post-commit hash for %q: %w", branch, err)
	}
	e.ws = persisted
	e.wsHash = newHash

	commitHash, err := commit.HashOf()
	if err != nil {
		return hash.Hash{}, nil, fmt.Errorf("commitBranchRoot: hashing new commit on %q: %w", branch, err)
	}
	return commitHash, persisted, nil
}

// commitBranchWS commits branch's working root as one Dolt commit, or returns
// false without committing when the root already matches HEAD. Caller must hold
// state.mu.
// maxAutoCommitRetries bounds the re-reads below. Each pass either commits or
// discovers there is nothing left to commit, so this is a livelock brake and
// not a policy.
const maxAutoCommitRetries = 8

// commitBranchWS creates the auto-commit for a branch. Losing the race to
// another writer is not a failure and must never reach the client: that writer
// committed whatever was on the branch, which includes this write, so the
// re-read below normally finds the working set already equal to HEAD and
// reports that there was nothing to commit.
//
// The retry lives here rather than in the command replay loop because the
// document write has already been published by the time auto-commit runs.
// Replaying the operation would apply it twice.
func (s *dbState) commitBranchWS(ctx context.Context, branch, message, author string) (bool, error) {
	for attempt := 0; ; attempt++ {
		committed, err := s.tryCommitBranchWS(ctx, branch, message, author)
		if !errors.Is(err, backends.ErrWriteRaced) || attempt >= maxAutoCommitRetries {
			return committed, err
		}
	}
}

func (s *dbState) tryCommitBranchWS(ctx context.Context, branch, message, author string) (committed bool, err error) {
	e := s.branchEntry(branch)
	wsRef := doltref.NewWorkingSetRef("heads/" + branch)

	// Read the branch, and commit exactly what was read. The cached copy is a
	// read cache, not a claim on the branch: its root can be older than what
	// another writer has published while its hash has been refreshed to the
	// current state, and committing that pair writes an old root back with a
	// prevHash that cannot refuse it.
	current, err := s.doltDB.ResolveWorkingSet(ctx, wsRef)
	if err != nil {
		return false, fmt.Errorf("commitBranchWS: resolving %q: %w", branch, err)
	}
	forkPoint, err := current.HashOf()
	if err != nil {
		return false, fmt.Errorf("commitBranchWS: hashing %q: %w", branch, err)
	}

	workingRoot := current.WorkingRoot()
	headRoot, err := headRootValueForBranch(ctx, s, branch)
	if err != nil {
		return false, fmt.Errorf("commitBranchWS: reading HEAD root for %q: %w", branch, err)
	}
	workingHash, err := workingRoot.HashOf()
	if err != nil {
		return false, fmt.Errorf("commitBranchWS: hashing working root for %q: %w", branch, err)
	}
	headHash, err := headRoot.HashOf()
	if err != nil {
		return false, fmt.Errorf("commitBranchWS: hashing HEAD root for %q: %w", branch, err)
	}
	if workingHash == headHash {
		return false, nil
	}

	headRef, err := wsRef.ToHeadRef()
	if err != nil {
		return false, fmt.Errorf("commitBranchWS: head ref for %q: %w", branch, err)
	}
	if author == "" {
		author = "dumbodb <dumbodb@localhost>"
	}
	cm, err := commitMetaAC(author, "", message, time.Now())
	if err != nil {
		return false, fmt.Errorf("commitBranchWS: commit meta for %q: %w", branch, err)
	}
	pending, err := s.doltDB.NewPendingCommit(ctx, doltdb.Roots{
		Head:    headRoot,
		Working: workingRoot,
		Staged:  workingRoot,
	}, nil, hash.Hash{}, cm)
	if err != nil {
		return false, fmt.Errorf("commitBranchWS: pending commit for %q: %w", branch, err)
	}

	cleanWS := current.WithStagedRoot(workingRoot).ClearMerge()
	var rsc doltdb.ReplicationStatusController
	if _, err := s.doltDB.CommitWithWorkingSet(ctx, headRef, wsRef, pending, cleanWS, forkPoint, doltdb.TodoWorkingSetMeta(), &rsc); err != nil {
		if errors.Is(err, datas.ErrOptimisticLockFailed) {
			return false, fmt.Errorf("%w: %q moved while auto-committing it", backends.ErrWriteRaced, branch)
		}
		return false, fmt.Errorf("commitBranchWS: committing %q: %w", branch, err)
	}

	persisted, err := s.doltDB.ResolveWorkingSet(ctx, wsRef)
	if err != nil {
		return false, fmt.Errorf("commitBranchWS: post-commit resolve for %q: %w", branch, err)
	}
	newHash, err := persisted.HashOf()
	if err != nil {
		return false, fmt.Errorf("commitBranchWS: post-commit hash for %q: %w", branch, err)
	}
	// Refreshing the read cache. e.mu spans two pointer writes, never I/O and
	// never the commit above: correctness is the compare-and-swap's job.
	e.mu.Lock()
	e.ws = persisted
	e.wsHash = newHash
	e.mu.Unlock()
	return true, nil
}
