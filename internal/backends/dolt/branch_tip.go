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
	"strconv"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"

	"github.com/dolthub/dumbodb/internal/backends"
)

// branchTip is a branch's working set as one command read it from disk, and
// the hash it had. A version-control command writes the branch only by
// compare-and-swap against that hash, so a client write published after the
// read makes the command fail with backends.ErrWriteRaced instead of being
// overwritten by a result computed without it. Each write the command makes
// advances the tip to the working set it published.
type branchTip struct {
	branch string
	ws     *doltdb.WorkingSet
	// hash is zero while the branch has no working set on disk, which is also
	// the compare-and-swap value that creates one.
	hash hash.Hash
	// lost is set when a write landed but the branch had already moved on by
	// the time it was read back, so the hash it was published under is
	// unknown. The next write fails as a race rather than guess.
	lost bool
	// published is set once any write over this tip has landed.
	published bool
}

func (s *dbState) readBranchTip(ctx context.Context, branch string) (*branchTip, error) {
	wsRef := doltref.NewWorkingSetRef("heads/" + branch)
	ws, err := s.doltDB.ResolveWorkingSet(ctx, wsRef)
	if errors.Is(err, doltdb.ErrWorkingSetNotFound) {
		headRV, headErr := headRootValueForBranch(ctx, s, branch)
		if headErr != nil {
			return nil, fmt.Errorf("reading HEAD of %q: %w", branch, headErr)
		}
		return &branchTip{branch: branch, ws: doltdb.EmptyWorkingSet(wsRef).WithWorkingRoot(headRV).WithStagedRoot(headRV)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the working set of %q: %w", branch, err)
	}
	h, err := ws.HashOf()
	if err != nil {
		return nil, fmt.Errorf("hashing the working set of %q: %w", branch, err)
	}
	return &branchTip{branch: branch, ws: ws, hash: h}, nil
}

func (t *branchTip) workingAM(ctx context.Context, s *dbState) (prolly.AddressMap, error) {
	return amFromWorkingRoot(ctx, t.ws.WorkingRoot(), s.ns)
}

func (t *branchTip) raced() error {
	return fmt.Errorf("%w: %q moved since this operation read it", backends.ErrWriteRaced, t.branch)
}

// publishOverTip replaces the working set tip holds with ws.
func (s *dbState) publishOverTip(ctx context.Context, tip *branchTip, ws *doltdb.WorkingSet) error {
	if tip.lost {
		return tip.raced()
	}
	if err := runBranchOpHook(ctx); err != nil {
		return err
	}
	wsRef := doltref.NewWorkingSetRef("heads/" + tip.branch)
	var rsc doltdb.ReplicationStatusController
	err := s.doltDB.UpdateWorkingSet(ctx, wsRef, ws, tip.hash, doltdb.TodoWorkingSetMeta(), &rsc)
	if errors.Is(err, datas.ErrOptimisticLockFailed) {
		return tip.raced()
	}
	if err != nil {
		return fmt.Errorf("publishing the working set of %q: %w", tip.branch, err)
	}
	return s.advanceTip(ctx, tip, ws)
}

// commitOverTip commits commitRV on tip's branch with exactly parents, and
// publishes the working set fn returns in the same update. fn receives tip's
// working set pointed at commitRV. The update also fails as a race if the
// branch HEAD moves while it is being made.
func (s *dbState) commitOverTip(
	ctx context.Context,
	tip *branchTip,
	commitRV doltdb.RootValue,
	parents []hash.Hash,
	cm *datas.CommitMeta,
	fn func(*doltdb.WorkingSet) (*doltdb.WorkingSet, error),
) (hash.Hash, error) {
	if tip.lost {
		return hash.Hash{}, tip.raced()
	}
	pending, err := s.pendingCommit(ctx, tip.branch, commitRV, parents, cm)
	if err != nil {
		return hash.Hash{}, err
	}
	// The parents are the operation's own; HEAD need not be among them, as
	// for a rebase, which leaves HEAD where it was until it finishes.
	pending.CommitOptions.Force = true
	newWS, err := fn(tip.ws.WithWorkingRoot(commitRV).WithStagedRoot(commitRV))
	if err != nil {
		return hash.Hash{}, err
	}

	if err := runBranchOpHook(ctx); err != nil {
		return hash.Hash{}, err
	}
	wsRef := doltref.NewWorkingSetRef("heads/" + tip.branch)
	headRef, err := wsRef.ToHeadRef()
	if err != nil {
		return hash.Hash{}, fmt.Errorf("head ref for %q: %w", tip.branch, err)
	}
	var rsc doltdb.ReplicationStatusController
	commit, err := s.doltDB.CommitWithWorkingSet(ctx, headRef, wsRef, pending, newWS, tip.hash, doltdb.TodoWorkingSetMeta(), &rsc)
	if errors.Is(err, datas.ErrOptimisticLockFailed) || errors.Is(err, datas.ErrMergeNeeded) {
		return hash.Hash{}, tip.raced()
	}
	if err != nil {
		return hash.Hash{}, fmt.Errorf("committing %q: %w", tip.branch, err)
	}
	commitHash, err := commit.HashOf()
	if err != nil {
		return hash.Hash{}, fmt.Errorf("hashing the new commit on %q: %w", tip.branch, err)
	}
	return commitHash, s.advanceTip(ctx, tip, newWS)
}

// setHeadOverTip moves tip's branch HEAD to target. With resetWorkingSet the
// same swap points the working set at target's root with no operation state;
// otherwise the working set is left as it is. The move fails as a race unless
// the branch still holds the working set tip read and the HEAD it had when
// this was called.
func (s *dbState) setHeadOverTip(ctx context.Context, tip *branchTip, target hash.Hash, resetWorkingSet bool) error {
	if tip.lost {
		return tip.raced()
	}
	branchDS, err := s.datasDB.GetDataset(ctx, branchRefPrefix+tip.branch)
	if err != nil {
		return fmt.Errorf("resolving branch %q: %w", tip.branch, err)
	}
	head, _ := branchDS.MaybeHeadAddr()
	if err := runBranchOpHook(ctx); err != nil {
		return err
	}
	wsPath := doltref.NewWorkingSetRef("heads/" + tip.branch).String()
	unmoved := func(ctx context.Context, datasets prolly.AddressMap, targetID string) error {
		ws, err := datasets.Get(ctx, wsPath)
		if err != nil {
			return err
		}
		cur, err := datasets.Get(ctx, targetID)
		if err != nil {
			return err
		}
		if ws != tip.hash || cur != head {
			return datas.ErrOptimisticLockFailed
		}
		return nil
	}
	resetPath := ""
	if resetWorkingSet {
		resetPath = wsPath
	}
	if _, err := s.datasDB.SetHead(ctx, branchDS, target, resetPath, unmoved); err != nil {
		if errors.Is(err, datas.ErrOptimisticLockFailed) {
			return tip.raced()
		}
		return err
	}
	tip.published = true
	if resetWorkingSet {
		// The swap wrote a working set this command never built, so there is
		// no further write it could make over it.
		tip.lost = true
		if err := s.reloadBranchWSFromDisk(ctx, tip.branch); err != nil {
			return err
		}
		if ws, err := s.loadBranchWS(ctx, tip.branch); err == nil {
			s.pushWSToSession(ctx, tip.branch, ws)
		}
	}
	return nil
}

// pendingCommit builds the commit for commitRV with exactly parents.
func (s *dbState) pendingCommit(ctx context.Context, branch string, commitRV doltdb.RootValue, parents []hash.Hash, cm *datas.CommitMeta) (*doltdb.PendingCommit, error) {
	headRoot, err := headRootValueForBranch(ctx, s, branch)
	if err != nil {
		return nil, fmt.Errorf("reading HEAD root for %q: %w", branch, err)
	}
	parentCommits := make([]*doltdb.Commit, 0, len(parents))
	for _, p := range parents {
		c, cErr := commitForHash(ctx, s, p)
		if cErr != nil {
			return nil, cErr
		}
		if c != nil {
			parentCommits = append(parentCommits, c)
		}
	}
	pending, err := s.doltDB.NewPendingCommit(ctx, doltdb.Roots{Head: headRoot, Working: commitRV, Staged: commitRV}, parentCommits, hash.Hash{}, cm)
	if err != nil {
		return nil, fmt.Errorf("pending commit for %q: %w", branch, err)
	}
	return pending, nil
}

// commitDangling writes a commit of commitRV with exactly parents, referenced
// by no branch.
func (s *dbState) commitDangling(ctx context.Context, branch string, commitRV doltdb.RootValue, parents []hash.Hash, cm *datas.CommitMeta) (hash.Hash, error) {
	pending, err := s.pendingCommit(ctx, branch, commitRV, parents, cm)
	if err != nil {
		return hash.Hash{}, err
	}
	commit, err := s.doltDB.CommitDangling(ctx, pending.Val, pending.CommitOptions)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("writing a commit for %q: %w", branch, err)
	}
	return commit.HashOf()
}

// advanceTip moves tip to written, the working set just published over it.
// UpdateWorkingSet does not return the hash it wrote, so the branch is read
// back; when its roots are no longer the ones written, another write has
// already replaced them and tip is marked lost.
func (s *dbState) advanceTip(ctx context.Context, tip *branchTip, written *doltdb.WorkingSet) error {
	tip.published = true
	if err := s.reloadBranchWSFromDisk(ctx, tip.branch); err != nil {
		return err
	}
	current, err := s.loadBranchWS(ctx, tip.branch)
	if err != nil {
		return err
	}
	s.pushWSToSession(ctx, tip.branch, current)
	same, err := sameRoots(current, written)
	if err != nil {
		return err
	}
	if !same {
		tip.lost = true
		return nil
	}
	h, err := current.HashOf()
	if err != nil {
		return fmt.Errorf("hashing the working set of %q: %w", tip.branch, err)
	}
	tip.ws, tip.hash = current, h
	return nil
}

func sameRoots(a, b *doltdb.WorkingSet) (bool, error) {
	for _, pair := range [][2]doltdb.RootValue{{a.WorkingRoot(), b.WorkingRoot()}, {a.StagedRoot(), b.StagedRoot()}} {
		ha, err := pair[0].HashOf()
		if err != nil {
			return false, err
		}
		hb, err := pair[1].HashOf()
		if err != nil {
			return false, err
		}
		if ha != hb {
			return false, nil
		}
	}
	return a.MergeActive() == b.MergeActive() && a.RebaseActive() == b.RebaseActive(), nil
}

// keepOperationStateConsistent keeps db.mergeState in step with the branch
// when a command fails. A command that failed before publishing anything goes
// back to saved, the state snapshotted when it started: it may have changed
// the state in memory, but none of that reached the branch. One that failed
// after publishing takes the state from the branch, as a restart would. A
// conflict report is not a failure here: the paused state it describes has
// been published, or was never more than in memory. Deferred as
//
//	var tip *branchTip
//	defer db.keepOperationStateConsistent(db.mergeState.clone(), &tip, &err)
func (s *dbState) keepOperationStateConsistent(saved *mergeInProgress, tipp **branchTip, errp *error) {
	if *errp == nil || isConflictReport(*errp) {
		return
	}
	tip := *tipp
	if tip == nil || !tip.published {
		s.mergeState = saved
		return
	}
	// The command's context may be the reason it failed.
	ctx := context.Background()
	ws, err := s.doltDB.ResolveWorkingSet(ctx, doltref.NewWorkingSetRef("heads/"+tip.branch))
	if err != nil || !ws.MergeActive() {
		s.mergeState = nil
		return
	}
	ms, err := mergeStateFromWS(ctx, s, tip.branch, ws)
	if err != nil {
		s.mergeState = nil
		*errp = fmt.Errorf("%w (and reloading the paused operation on %q failed: %w)", *errp, tip.branch, err)
		return
	}
	s.mergeState = ms
}

func isConflictReport(err error) bool {
	var merge *backends.MergeConflictError
	var pick *backends.DumboDBCherryPickConflictError
	var revert *backends.DumboDBRevertConflictError
	var rebase *backends.DumboDBRebaseConflictError
	return errors.As(err, &merge) || errors.As(err, &pick) || errors.As(err, &revert) || errors.As(err, &rebase)
}

// requireNoUncommittedChanges refuses an operation that is starting while
// tip's branch has uncommitted changes. The working set is shared by every
// session, so unlike git the operation cannot carry them through: they could
// be another client's.
func (s *dbState) requireNoUncommittedChanges(ctx context.Context, tip *branchTip) error {
	headAM, err := headRootAMForBranch(ctx, s, tip.branch)
	if err != nil {
		return fmt.Errorf("reading HEAD of %q: %w", tip.branch, err)
	}
	workingAM, err := tip.workingAM(ctx, s)
	if err != nil {
		return fmt.Errorf("reading the working root of %q: %w", tip.branch, err)
	}
	if headAM.HashOf() == workingAM.HashOf() {
		return nil
	}

	var changed []string
	var collections []string
	err = eachCollectionChange(ctx, s, headAM, workingAM, func(c collectionChange) error {
		added, modified, deleted, countErr := countCollectionMapDiffs(ctx, c.AMap, c.BMap)
		if countErr != nil {
			return fmt.Errorf("counting changes to %q: %w", c.Name, countErr)
		}
		if added != 0 || modified != 0 || deleted != 0 || c.surfacesWithoutDocChange() {
			collections = append(collections, strconv.Quote(c.Name))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(collections) > 0 {
		changed = append(changed, "collections "+strings.Join(collections, ", "))
	}
	viewChanges, err := diffViewEntries(ctx, s.cs, s.ns, headAM, workingAM)
	if err != nil {
		return err
	}
	var views []string
	for _, vc := range viewChanges {
		views = append(views, strconv.Quote(vc.Name))
	}
	if len(views) > 0 {
		changed = append(changed, "views "+strings.Join(views, ", "))
	}
	if len(changed) == 0 {
		return nil
	}
	return fmt.Errorf("%w on %q (%s); commit or reset them first",
		backends.ErrUncommittedChanges, tip.branch, strings.Join(changed, "; "))
}

// branchOpHookKey makes a write racing a version-control operation
// deterministic in backend tests: the hook runs after the operation has read
// the branch and before each compare-and-swap it makes. A hook returning an
// error fails the command there.
type branchOpHookKey struct{}

func runBranchOpHook(ctx context.Context) error {
	switch hook := ctx.Value(branchOpHookKey{}).(type) {
	case func():
		hook()
	case func() error:
		return hook()
	}
	return nil
}
