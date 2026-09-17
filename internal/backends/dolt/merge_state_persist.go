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
	"io"
	"sort"
	"strings"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"
	"github.com/dolthub/go-mysql-server/sql"
)

// An in-progress merge, cherry-pick, revert, or rebase lives in the branch
// working set's Dolt MergeState/RebaseState. The working set carries both the
// conflicted root value -- whose DTBLs hold the conflict artifacts -- and the
// metadata describing the operation, and one UpdateWorkingSet publishes them
// together, so the two can never disagree after a crash.
//
// Working set slots:
//
//	intoBranch              the working set's own branch
//	resolvedAM              working root (and staged, so the tree reads clean)
//	premergeAM              MergeState.PreMergeWorkingRoot
//	fromHash / pickHash /
//	  rebaseBranchHash      MergeState.Commit, the content being applied
//	fromBranch / ontoBranch MergeState.CommitSpecStr
//	intoHash                MergeState.PreMergeHeadCommit; during a rebase that
//	                        slot holds rebaseBranchHash and intoHash is the
//	                        branch HEAD the replay has advanced to
//	rebaseOntoHash          RebaseState.OntoCommit
//	rebaseCommitsReplayed   RebaseState.LastAttemptedStep
//	conflicts               ArtifactMaps inside the conflicting DTBLs
//	viewConflicts,
//	  metaConflicts         MergeState.TablesWithSchemaConflicts -- namespace
//	                        entries no document-level merge can settle
//
// A rebase puts its PRE-REBASE TIP in MergeState.Commit rather than the pick it
// paused on, because that slot and MergeState.PreMergeWorkingRoot are the only
// two a working set exposes to the chunk walker (see SerialMessage.WalkAddrs).
// The replay moves the branch HEAD off the original commits, so pinning the tip
// is what keeps the whole run -- the paused pick and every pick still queued
// behind it -- reachable through a GC. The pick it paused on is not stored: the
// plan is recomputed with findCommitsToReplay over the same two commits it was
// computed from at the start, and RebaseState.LastAttemptedStep says how far
// into that plan the replay got.
//
// The rest is derived when the state is read back: originalMsg from the picked
// commit, and a revert's fromHash from that commit's first parent.

// sourceHash is the commit stored as the merge state's "from" commit: the
// commit whose content the operation is applying. For a rebase that is the
// pre-rebase tip, which keeps the whole run of picks reachable.
func (m *mergeInProgress) sourceHash() hash.Hash {
	switch {
	case m.isRebase:
		return m.rebaseBranchHash
	case m.isCherryPick, m.isRevert:
		return m.pickHash
	default:
		return m.fromHash
	}
}

// preOpHeadHash is the branch HEAD as it stood before the operation started.
func (m *mergeInProgress) preOpHeadHash() hash.Hash {
	if m.isRebase {
		return m.rebaseBranchHash
	}
	return m.intoHash
}

// sourceSpec is the refspec the operation names its source by.
func (m *mergeInProgress) sourceSpec() string {
	if m.isRebase {
		return m.ontoBranch
	}
	return m.fromBranch
}

// sideDescriptions renders the "ours" and "theirs" labels used in conflict
// reason messages. It reproduces the labels the operation passed to
// mergeAddressMapsWithConflicts, so reason text is stable across a restart.
func (m *mergeInProgress) sideDescriptions(ctx context.Context, state *dbState) (ours, theirs string) {
	switch {
	case m.isRebase:
		return fmt.Sprintf("commit '%s' (ours)", m.rebaseCurrentPick.String()),
			fmt.Sprintf("branch '%s' (theirs)", m.ontoBranch)
	case m.isCherryPick, m.isRevert:
		return fmt.Sprintf("branch '%s' (ours)", m.intoBranch),
			fmt.Sprintf("commit '%s' (theirs)", m.pickHash.String())
	default:
		return fmt.Sprintf("branch '%s' (ours)", m.intoBranch),
			fmt.Sprintf("%s (theirs)", refLabel(ctx, state, m.fromBranch))
	}
}

// unsettledNamespaceEntries lists the collections and views whose conflict is
// at the namespace level rather than the document level, and which the user has
// not resolved yet.
func unsettledNamespaceEntries(ms *mergeInProgress) []string {
	names := make([]string, 0, len(ms.viewConflicts)+len(ms.metaConflicts))
	for name, vce := range ms.viewConflicts {
		if !vce.resolved {
			names = append(names, name)
		}
	}
	for name, mce := range ms.metaConflicts {
		if !mce.resolved {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func commitForHash(ctx context.Context, state *dbState, h hash.Hash) (*doltdb.Commit, error) {
	if h.IsEmpty() {
		return nil, nil
	}
	optional, err := state.doltDB.ReadCommit(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("reading commit %q: %w", h.String(), err)
	}
	commit, ok := optional.ToCommit()
	if !ok {
		return nil, fmt.Errorf("commit %q is not present", h.String())
	}
	return commit, nil
}

// withOperationState stamps ms onto ws. The Start* helpers snapshot the working
// root as the pre-operation root, so ws must arrive holding premergeRV.
func withOperationState(ctx context.Context, state *dbState, ws *doltdb.WorkingSet, ms *mergeInProgress, premergeRV doltdb.RootValue) (*doltdb.WorkingSet, error) {
	head, err := commitForHash(ctx, state, ms.preOpHeadHash())
	if err != nil {
		return nil, err
	}
	source, err := commitForHash(ctx, state, ms.sourceHash())
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, fmt.Errorf("operation on branch %q has no source commit", ms.intoBranch)
	}

	switch {
	case ms.isCherryPick:
		ws = ws.StartCherryPick(head, source, ms.sourceSpec())
	case ms.isRevert:
		ws = ws.StartRevert(head, source, ms.sourceSpec(), nil)
	default:
		ws = ws.StartMerge(head, source, ms.sourceSpec())
	}
	ws = ws.WithUnmergableTables(doltdb.ToTableNames(unsettledNamespaceEntries(ms), doltdb.DefaultSchemaName))

	if !ms.isRebase {
		return ws.ClearRebase(), nil
	}

	onto, err := commitForHash(ctx, state, ms.rebaseOntoHash)
	if err != nil {
		return nil, err
	}
	if onto == nil {
		return nil, fmt.Errorf("rebase on branch %q has no onto commit", ms.intoBranch)
	}
	ws, err = ws.StartRebase(sql.NewContext(ctx), onto, ms.intoBranch, premergeRV,
		doltdb.ErrorOnEmptyCommit, doltdb.ErrorOnEmptyCommit, false)
	if err != nil {
		return nil, fmt.Errorf("recording rebase state for %q: %w", ms.intoBranch, err)
	}
	return ws.WithRebaseState(
		ws.RebaseState().WithRebasingStarted(true).WithLastAttemptedStep(float32(ms.rebaseCommitsReplayed)),
	), nil
}

// persistConflictState publishes the paused operation: the partially merged
// root (carrying the conflict artifacts in its DTBLs) and the operation
// metadata go out in a single working set update. The caller must hold
// state.mu (write lock).
func persistConflictState(ctx context.Context, state *dbState, ms *mergeInProgress) error {
	premergeRV, err := amToRootValue(ctx, state, ms.premergeAM)
	if err != nil {
		return fmt.Errorf("building pre-operation root value: %w", err)
	}
	conflictRV, err := amToRootValue(ctx, state, ms.resolvedAM)
	if err != nil {
		return fmt.Errorf("building conflict root value: %w", err)
	}

	var newWS *doltdb.WorkingSet
	if err := state.updateBranchWS(ctx, ms.intoBranch, func(cur *doltdb.WorkingSet) (*doltdb.WorkingSet, error) {
		seeded := cur.WithWorkingRoot(premergeRV).WithStagedRoot(premergeRV)
		stamped, stampErr := withOperationState(ctx, state, seeded, ms, premergeRV)
		if stampErr != nil {
			return nil, stampErr
		}
		// Working and staged both carry the conflicted root so `dolt checkout`
		// sees no uncommitted diff; the artifacts ride inside its DTBLs.
		newWS = stamped.WithWorkingRoot(conflictRV).WithStagedRoot(conflictRV)
		return newWS, nil
	}); err != nil {
		return fmt.Errorf("persisting merge state for %q: %w", ms.intoBranch, err)
	}
	state.pushWSToSession(ctx, ms.intoBranch, newWS)
	return nil
}

// settleMergeState points branch's working set at finalAM and drops the
// operation metadata in one update, so a completed operation cannot leave a
// conflicted root or stale metadata behind. The caller must hold state.mu.
func settleMergeState(ctx context.Context, state *dbState, branch string, finalAM prolly.AddressMap) error {
	finalRV, err := amToRootValue(ctx, state, finalAM)
	if err != nil {
		return fmt.Errorf("building settled root value: %w", err)
	}
	var newWS *doltdb.WorkingSet
	if err := state.updateBranchWS(ctx, branch, func(cur *doltdb.WorkingSet) (*doltdb.WorkingSet, error) {
		newWS = cur.WithWorkingRoot(finalRV).WithStagedRoot(finalRV).ClearMerge().ClearRebase()
		return newWS, nil
	}); err != nil {
		return fmt.Errorf("clearing merge state for %q: %w", branch, err)
	}
	state.pushWSToSession(ctx, branch, newWS)
	return nil
}

// clearMergeState drops any operation metadata from branch's working set,
// leaving its roots alone. The caller must hold state.mu.
func clearMergeState(ctx context.Context, state *dbState, branch string) error {
	var newWS *doltdb.WorkingSet
	if err := state.updateBranchWS(ctx, branch, func(cur *doltdb.WorkingSet) (*doltdb.WorkingSet, error) {
		newWS = cur.ClearMerge().ClearRebase()
		return newWS, nil
	}); err != nil {
		return fmt.Errorf("clearing merge state for %q: %w", branch, err)
	}
	state.pushWSToSession(ctx, branch, newWS)
	return nil
}

// abortMergeState restores the pre-operation root and drops the operation
// metadata in one update. The caller must hold state.mu.
func abortMergeState(ctx context.Context, state *dbState, ms *mergeInProgress) error {
	return settleMergeState(ctx, state, ms.intoBranch, ms.premergeAM)
}

// loadMergeState rebuilds the operation paused on any branch of state, or
// returns (nil, nil) when no branch has one. Called from getOrOpenDB before
// state.mu is taken.
func loadMergeState(ctx context.Context, state *dbState) (*mergeInProgress, error) {
	dsMap, err := state.datasDB.Datasets(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing datasets: %w", err)
	}

	var branches []string
	if iterErr := dsMap.IterAll(ctx, func(id string, _ hash.Hash) error {
		if strings.HasPrefix(id, branchRefPrefix) {
			branches = append(branches, strings.TrimPrefix(id, branchRefPrefix))
		}
		return nil
	}); iterErr != nil {
		return nil, fmt.Errorf("iterating datasets: %w", iterErr)
	}
	sort.Strings(branches)

	for _, branch := range branches {
		// Resolved straight from disk rather than through loadBranchWS: opening
		// a database must leave the branch working set cache cold, so paths
		// that advance a working set out of band (clone, fetch) are not read
		// back through a snapshot taken here.
		ws, wsErr := state.doltDB.ResolveWorkingSet(ctx, doltref.NewWorkingSetRef("heads/"+branch))
		if wsErr != nil || ws == nil || !ws.MergeActive() {
			continue
		}
		ms, msErr := mergeStateFromWS(ctx, state, branch, ws)
		if msErr != nil {
			return nil, fmt.Errorf("restoring operation on branch %q: %w", branch, msErr)
		}
		return ms, nil
	}
	return nil, nil
}

func mergeStateFromWS(ctx context.Context, state *dbState, branch string, ws *doltdb.WorkingSet) (*mergeInProgress, error) {
	stored := ws.MergeState()

	premergeAM, err := amFromWorkingRoot(ctx, stored.PreMergeWorkingRoot(), state.ns)
	if err != nil {
		return nil, fmt.Errorf("reading pre-operation AM: %w", err)
	}
	resolvedAM, err := amFromWorkingRoot(ctx, ws.WorkingRoot(), state.ns)
	if err != nil {
		return nil, fmt.Errorf("reading working AM: %w", err)
	}
	sourceHash, err := stored.Commit().HashOf()
	if err != nil {
		return nil, fmt.Errorf("hashing source commit: %w", err)
	}
	var preOpHead hash.Hash
	if head := stored.PreMergeHeadCommit(); head != nil {
		if preOpHead, err = head.HashOf(); err != nil {
			return nil, fmt.Errorf("hashing pre-operation head commit: %w", err)
		}
	}

	ms := &mergeInProgress{
		intoBranch:   branch,
		premergeAM:   premergeAM,
		resolvedAM:   resolvedAM,
		intoHash:     preOpHead,
		isCherryPick: stored.IsCherryPick(),
		isRevert:     stored.IsRevert(),
		isRebase:     ws.RebaseActive(),
	}

	switch {
	case ms.isRebase:
		ms.ontoBranch = stored.CommitSpecStr()
		ms.rebaseBranchHash = sourceHash
		ms.rebaseCommitsReplayed = int(ws.RebaseState().LastAttemptedStep())
		if ms.rebaseOntoHash, err = ws.RebaseState().OntoCommit().HashOf(); err != nil {
			return nil, fmt.Errorf("hashing rebase onto commit: %w", err)
		}
		// The replay has moved the branch HEAD; that tip is the "into" side of
		// the paused pick.
		if ms.intoHash, err = branchHeadHash(ctx, state, branch); err != nil {
			return nil, err
		}
		if err = restoreRebasePlan(ctx, state, ms); err != nil {
			return nil, err
		}
	case ms.isCherryPick:
		ms.pickHash = sourceHash
		if ms.originalMsg, err = commitDescription(ctx, state, sourceHash); err != nil {
			return nil, err
		}
	case ms.isRevert:
		ms.pickHash = sourceHash
		if ms.originalMsg, err = commitDescription(ctx, state, sourceHash); err != nil {
			return nil, err
		}
		// "Theirs" for a revert is the state being restored: the parent of the
		// reverted commit.
		if ms.fromHash, err = firstParentHash(ctx, state, sourceHash); err != nil {
			return nil, err
		}
	default:
		ms.fromBranch = stored.CommitSpecStr()
		ms.fromHash = sourceHash
	}

	oursDesc, theirsDesc := ms.sideDescriptions(ctx, state)

	ms.conflicts, err = loadDocumentConflicts(ctx, state, resolvedAM, oursDesc, theirsDesc)
	if err != nil {
		return nil, err
	}
	if err := loadNamespaceConflicts(ctx, state, ms, doltdb.FlattenTableNames(stored.TablesWithSchemaConflicts()), oursDesc, theirsDesc); err != nil {
		return nil, err
	}
	return ms, nil
}

func branchHeadHash(ctx context.Context, state *dbState, branch string) (hash.Hash, error) {
	ds, err := state.datasDB.GetDataset(ctx, branchRefPrefix+branch)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("resolving branch %q: %w", branch, err)
	}
	h, ok := ds.MaybeHeadAddr()
	if !ok {
		return hash.Hash{}, fmt.Errorf("branch %q has no head address", branch)
	}
	return h, nil
}

func commitDescription(ctx context.Context, state *dbState, h hash.Hash) (string, error) {
	commit, err := datas.LoadCommitAddr(ctx, state.vs, h)
	if err != nil {
		return "", fmt.Errorf("loading commit %q: %w", h.String(), err)
	}
	meta, err := datas.GetCommitMeta(ctx, commit.NomsValue())
	if err != nil {
		return "", fmt.Errorf("reading meta for commit %q: %w", h.String(), err)
	}
	return meta.Description, nil
}

// firstParentHash returns the commit's first parent, or the zero hash for a
// root commit.
func firstParentHash(ctx context.Context, state *dbState, h hash.Hash) (hash.Hash, error) {
	commit, err := datas.LoadCommitAddr(ctx, state.vs, h)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("loading commit %q: %w", h.String(), err)
	}
	parents, err := dolttypes.SerialCommitParentAddrs(dolttypes.Format_DOLT, commit.NomsValue().(dolttypes.SerialMessage))
	if err != nil {
		return hash.Hash{}, fmt.Errorf("reading parents of commit %q: %w", h.String(), err)
	}
	if len(parents) == 0 {
		return hash.Hash{}, nil
	}
	return parents[0], nil
}

// restoreRebasePlan recomputes the run of commits the rebase set out to replay
// and splits it at the step the replay reached, recovering the paused pick and
// the picks still queued behind it.
func restoreRebasePlan(ctx context.Context, state *dbState, ms *mergeInProgress) error {
	plan, err := findCommitsToReplay(ctx, state, ms.rebaseBranchHash, ms.rebaseOntoHash)
	if err != nil {
		return fmt.Errorf("recomputing the rebase plan: %w", err)
	}
	if ms.rebaseCommitsReplayed < 0 || ms.rebaseCommitsReplayed >= len(plan) {
		return fmt.Errorf("rebase on %q is paused at step %d of a %d-commit plan",
			ms.intoBranch, ms.rebaseCommitsReplayed, len(plan))
	}
	ms.rebaseCurrentPick = plan[ms.rebaseCommitsReplayed]
	ms.rebaseRemainingHashes = plan[ms.rebaseCommitsReplayed+1:]
	return nil
}

// loadDocumentConflicts rebuilds the per-collection conflict entries from the
// ArtifactMaps embedded in the working root's DTBLs.
func loadDocumentConflicts(ctx context.Context, state *dbState, workingAM prolly.AddressMap, oursDesc, theirsDesc string) (map[string][]*conflictEntry, error) {
	conflicts := make(map[string][]*conflictEntry)
	names, err := collectionsWithArtifacts(ctx, state, workingAM)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		entries, entryErr := loadConflictEntriesFromArtifacts(ctx, state, workingAM, name, oursDesc, theirsDesc)
		if entryErr != nil {
			return nil, fmt.Errorf("loading conflicts for %q from artifacts: %w", name, entryErr)
		}
		if len(entries) > 0 {
			conflicts[name] = entries
		}
	}
	return conflicts, nil
}

// collectionsWithArtifacts lists the collections in am whose DTBL carries a
// non-empty ArtifactMap.
func collectionsWithArtifacts(ctx context.Context, state *dbState, am prolly.AddressMap) ([]string, error) {
	var names []string
	if err := am.IterAll(ctx, func(name string, h hash.Hash) error {
		// Catalog divergence is a metadata conflict, surfaced on the owning
		// collection; the internal name must never reach ms.conflicts.
		if h.IsEmpty() || name == reservedCatalogName {
			return nil
		}
		chunk, err := state.cs.Get(ctx, h)
		if err != nil {
			return fmt.Errorf("reading chunk for %q: %w", name, err)
		}
		if serial.GetFileID(chunk.Data()) != serial.TableFileID {
			return nil
		}
		tbl, err := serial.TryGetRootAsTable(chunk.Data(), serial.MessagePrefixSz)
		if err != nil {
			return fmt.Errorf("parsing DTBL for %q: %w", name, err)
		}
		if !hash.New(tbl.ArtifactsBytes()).IsEmpty() {
			names = append(names, name)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// loadNamespaceConflicts rebuilds the view and collection-metadata conflicts
// named in the merge state by re-reading the three sides of the operation.
func loadNamespaceConflicts(ctx context.Context, state *dbState, ms *mergeInProgress, names []string, oursDesc, theirsDesc string) error {
	if len(names) == 0 {
		return nil
	}
	intoAM, fromAM, baseAM, err := operationSides(ctx, state, ms)
	if err != nil {
		return err
	}

	for _, name := range names {
		intoH, err := intoAM.Get(ctx, name)
		if err != nil {
			return fmt.Errorf("reading ours entry for %q: %w", name, err)
		}
		fromH, err := fromAM.Get(ctx, name)
		if err != nil {
			return fmt.Errorf("reading theirs entry for %q: %w", name, err)
		}
		baseH, err := baseAM.Get(ctx, name)
		if err != nil {
			return fmt.Errorf("reading base entry for %q: %w", name, err)
		}

		isView, err := anyIsViewEntry(ctx, state, intoH, fromH, baseH)
		if err != nil {
			return err
		}
		if isView {
			vce, vErr := buildViewConflict(ctx, state, name, intoH, fromH, baseH, ms.theirHash())
			if vErr != nil {
				return vErr
			}
			if ms.viewConflicts == nil {
				ms.viewConflicts = map[string]*viewConflictEntry{}
			}
			ms.viewConflicts[name] = vce
			continue
		}

		mce, mErr := buildMetaConflict(ctx, state, name, intoAM, fromAM, baseAM, ms.theirHash(), oursDesc, theirsDesc)
		if mErr != nil {
			return mErr
		}
		if mce == nil {
			continue
		}
		if ms.metaConflicts == nil {
			ms.metaConflicts = map[string]*metaConflictEntry{}
		}
		ms.metaConflicts[name] = mce
	}
	return nil
}

// operationSides returns the three AddressMaps the operation merged, in the
// same roles it gave them: ours, theirs, and the common base. A rebase swaps
// the first two, presenting the replayed commit as ours.
func operationSides(ctx context.Context, state *dbState, ms *mergeInProgress) (intoAM, fromAM, baseAM prolly.AddressMap, err error) {
	amFor := func(h hash.Hash) (prolly.AddressMap, error) {
		if h.IsEmpty() {
			return prolly.NewEmptyAddressMap(state.ns)
		}
		return amFromCommitHash(ctx, state, h.String())
	}

	var oursHash, theirsHash, baseHash hash.Hash
	switch {
	case ms.isRebase:
		oursHash = ms.rebaseCurrentPick
		theirsHash = ms.intoHash
		if baseHash, err = firstParentHash(ctx, state, ms.rebaseCurrentPick); err != nil {
			return
		}
	case ms.isCherryPick:
		oursHash = ms.intoHash
		theirsHash = ms.pickHash
		if baseHash, err = firstParentHash(ctx, state, ms.pickHash); err != nil {
			return
		}
	case ms.isRevert:
		oursHash = ms.intoHash
		theirsHash = ms.fromHash
		baseHash = ms.pickHash
	default:
		oursHash = ms.intoHash
		theirsHash = ms.fromHash
		oursCommit, cErr := datas.LoadCommitAddr(ctx, state.vs, oursHash)
		if cErr != nil {
			err = fmt.Errorf("loading ours commit: %w", cErr)
			return
		}
		theirsCommit, cErr := datas.LoadCommitAddr(ctx, state.vs, theirsHash)
		if cErr != nil {
			err = fmt.Errorf("loading theirs commit: %w", cErr)
			return
		}
		var hasBase bool
		baseHash, hasBase, err = datas.FindCommonAncestor(ctx, oursCommit, theirsCommit, state.vs, state.vs, state.ns, state.ns)
		if err != nil {
			err = fmt.Errorf("finding common ancestor: %w", err)
			return
		}
		if !hasBase {
			err = fmt.Errorf("no common ancestor for the paused merge on %q", ms.intoBranch)
			return
		}
	}

	if intoAM, err = amFor(oursHash); err != nil {
		return
	}
	if fromAM, err = amFor(theirsHash); err != nil {
		return
	}
	baseAM, err = amFor(baseHash)
	return
}

func anyIsViewEntry(ctx context.Context, state *dbState, hashes ...hash.Hash) (bool, error) {
	for _, h := range hashes {
		isView, err := isViewEntry(ctx, state.cs, h)
		if err != nil {
			return false, err
		}
		if isView {
			return true, nil
		}
	}
	return false, nil
}

// buildMetaConflict reconstructs the collection-metadata conflict for coll from
// the catalog document each side holds.
func buildMetaConflict(ctx context.Context, state *dbState, coll string, intoAM, fromAM, baseAM prolly.AddressMap, theirHash hash.Hash, oursDesc, theirsDesc string) (*metaConflictEntry, error) {
	key, err := catalogKey(coll)
	if err != nil {
		return nil, err
	}

	sideValue := func(am prolly.AddressMap) (val.Tuple, error) {
		catMap, mapErr := catalogMapFromAM(ctx, state, am)
		if mapErr != nil {
			return nil, mapErr
		}
		var found val.Tuple
		if getErr := catMap.Get(ctx, key, func(_, v val.Tuple) error {
			found = v
			return nil
		}); getErr != nil {
			return nil, getErr
		}
		return found, nil
	}

	baseVal, err := sideValue(baseAM)
	if err != nil {
		return nil, fmt.Errorf("reading base metadata for %q: %w", coll, err)
	}
	oursVal, err := sideValue(intoAM)
	if err != nil {
		return nil, fmt.Errorf("reading ours metadata for %q: %w", coll, err)
	}
	theirsVal, err := sideValue(fromAM)
	if err != nil {
		return nil, fmt.Errorf("reading theirs metadata for %q: %w", coll, err)
	}

	entry := &conflictEntry{
		id:            conflictID(reservedCatalogName, key, theirHash),
		rawKey:        key,
		baseRawVal:    baseVal,
		oursRawVal:    oursVal,
		theirsRawVal:  theirsVal,
		ourDiffType:   computeDiffType(baseVal, oursVal),
		theirDiffType: computeDiffType(baseVal, theirsVal),
	}
	conflicts, err := metaConflictsFromCatalog(ctx, state, []*conflictEntry{entry}, oursDesc, theirsDesc)
	if err != nil {
		return nil, fmt.Errorf("rebuilding metadata conflict for %q: %w", coll, err)
	}
	return conflicts[coll], nil
}

// loadConflictEntriesFromArtifacts reads collName's ArtifactMap from am and
// rebuilds its conflict entries, looking base and theirs values up in the
// commits the artifacts name.
func loadConflictEntriesFromArtifacts(ctx context.Context, state *dbState, am prolly.AddressMap, collName, oursDesc, theirsDesc string) ([]*conflictEntry, error) {
	artMap, err := openCollectionArtifacts(ctx, state, am, collName)
	if err != nil {
		return nil, fmt.Errorf("opening artifacts for %q: %w", collName, err)
	}

	cnt, err := artMap.Count()
	if err != nil || cnt == 0 {
		return nil, nil
	}

	oursMap, err := collectionMapFromAM(ctx, state, am, collName)
	if err != nil {
		return nil, fmt.Errorf("opening ours collection map for %q: %w", collName, err)
	}

	iter, err := artMap.IterAllConflicts(ctx)
	if err != nil {
		return nil, fmt.Errorf("iterating conflict artifacts for %q: %w", collName, err)
	}

	var entries []*conflictEntry
	for {
		ca, iterErr := iter.Next(ctx)
		if iterErr == io.EOF {
			break
		}
		if iterErr != nil {
			return nil, fmt.Errorf("iterating conflicts for %q: %w", collName, iterErr)
		}

		theirH := ca.TheirRootIsh
		baseH := ca.Metadata.BaseRootIsh
		rawKey := val.Tuple(ca.Key)

		var oursVal val.Tuple
		_ = oursMap.Get(ctx, rawKey, func(k, v val.Tuple) error {
			oursVal = v
			return nil
		})

		theirsVal, err := valueAtCommit(ctx, state, theirH, collName, rawKey)
		if err != nil {
			return nil, err
		}
		baseVal, err := valueAtCommit(ctx, state, baseH, collName, rawKey)
		if err != nil {
			return nil, err
		}

		entry := &conflictEntry{
			id:            conflictID(collName, rawKey, theirH),
			typ:           "documentEdit",
			rawKey:        rawKey,
			baseRawVal:    baseVal,
			oursRawVal:    oursVal,
			theirsRawVal:  theirsVal,
			ourDiffType:   computeDiffType(baseVal, oursVal),
			theirDiffType: computeDiffType(baseVal, theirsVal),
		}
		entry.reasonCode = documentEditReasonCode(entry.ourDiffType, entry.theirDiffType)
		entry.reasonMessage = documentEditReasonMessage(entry.reasonCode, conflictDocID(ctx, state, entry), oursDesc, theirsDesc)
		entries = append(entries, entry)
	}

	return entries, nil
}

// valueAtCommit reads the value stored under rawKey in collName as of commit h.
// A missing commit, collection, or key reads as absent: the side deleted the
// document, or never had it.
func valueAtCommit(ctx context.Context, state *dbState, h hash.Hash, collName string, rawKey val.Tuple) (val.Tuple, error) {
	if h.IsEmpty() {
		return nil, nil
	}
	am, err := amFromCommitHash(ctx, state, h.String())
	if err != nil {
		return nil, nil
	}
	collMap, err := collectionMapFromAM(ctx, state, am, collName)
	if err != nil {
		return nil, nil
	}
	var found val.Tuple
	_ = collMap.Get(ctx, rawKey, func(_, v val.Tuple) error {
		found = v
		return nil
	})
	return found, nil
}

// conflictDocID decodes the document _id for a conflict's reason message,
// falling back to nil when no side holds a readable document.
func conflictDocID(ctx context.Context, state *dbState, entry *conflictEntry) any {
	for _, v := range []val.Tuple{entry.oursRawVal, entry.theirsRawVal, entry.baseRawVal} {
		if v == nil {
			continue
		}
		doc, err := readDocFromValue(ctx, state.ns, v)
		if err != nil {
			continue
		}
		if id, idErr := doc.Get("_id"); idErr == nil {
			return id
		}
	}
	return nil
}

// computeDiffType returns the diff type string based on base and current values.
func computeDiffType(base, current val.Tuple) string {
	switch {
	case base == nil && current != nil:
		return "added"
	case base != nil && current == nil:
		return "deleted"
	default:
		return "modified"
	}
}

// clearConflictArtifacts rebuilds all conflicting collection DTBLs without artifacts
// and updates ms.resolvedAM. Called before committing a resolved merge/cherry-pick/rebase.
// The caller must hold state.mu (write lock).
func clearConflictArtifacts(ctx context.Context, state *dbState, ms *mergeInProgress) error {
	amEditor := ms.resolvedAM.Editor()

	for collName := range ms.conflicts {
		// Get the collection map from the resolved AM.
		collMap, err := collectionMapFromAM(ctx, state, ms.resolvedAM, collName)
		if err != nil {
			return fmt.Errorf("opening collection %q: %w", collName, err)
		}

		// Build a DTBL with no artifacts. Preserve the current per-branch
		// index AM read from the resolved AM (workspace-ife will handle
		// proper per-index merge).
		curIdxAM, idxErr := indexAMFromAM(ctx, state.cs, state.ns, ms.resolvedAM, collName)
		if idxErr != nil {
			return fmt.Errorf("reading current index AM for %q: %w", collName, idxErr)
		}
		newDTBLHash, err := state.dtblHashForCollection(ctx, collName, collMap, curIdxAM, hash.Hash{})
		if err != nil {
			return fmt.Errorf("building clean DTBL for %q: %w", collName, err)
		}

		cnt, countErr := collMap.Count()
		if countErr != nil {
			return fmt.Errorf("counting collection %q: %w", collName, countErr)
		}

		if cnt == 0 {
			if err := amEditor.Delete(ctx, collName); err != nil {
				return fmt.Errorf("deleting empty collection %q from AM: %w", collName, err)
			}
		} else {
			if err := amEditor.Update(ctx, collName, newDTBLHash); err != nil {
				return fmt.Errorf("updating AM for %q: %w", collName, err)
			}
		}
	}

	newAM, err := amEditor.Flush(ctx)
	if err != nil {
		return fmt.Errorf("flushing AM after clearing artifacts: %w", err)
	}

	ms.resolvedAM = newAM
	return nil
}
