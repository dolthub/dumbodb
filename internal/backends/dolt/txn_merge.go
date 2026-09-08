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

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/store/prolly"

	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dumbodb/internal/backends"
)

// rootValueFromAM wraps a collections AddressMap in the RTVL chunk dolt
// expects wherever a root value is stored.
func rootValueFromAM(ctx context.Context, state *dbState, am prolly.AddressMap) (doltdb.RootValue, error) {
	msg := dolttypes.SerialMessage(buildRootValueFlatbuffer(am))
	return doltdb.NewRootValue(ctx, state.doltDB.ValueReadWriter(), state.doltDB.NodeStore(), msg)
}

// reconcileWorkingSets produces the post-boundary working set for a write by
// three-way merging the writer's pending changes (ours) against the branch's
// current state (theirs), with the working set the write started from as the
// base.
//
// This runs dumbodb's own address-map merge -- the same merge a branch merge,
// cherry-pick, revert or rebase runs -- so every collection's merge mode
// decides what counts as a conflict. There is one merge in the product and one
// policy, and a write reaches it whatever boundary it reconciles at.
//
// A refusal comes back as a typed MergeConflictError together with the
// unresolved conflicts, so a boundary that can offer resolution -- dumboCommit
// -- is able to install them, and one that cannot just reports the error.
func (state *dbState) reconcileWorkingSets(ctx context.Context, branch string, base, ours, theirs *doltdb.WorkingSet) (*doltdb.WorkingSet, *mergeInProgress, error) {
	baseHash, err := base.WorkingRoot().HashOf()
	if err != nil {
		return nil, nil, fmt.Errorf("hashing base root: %w", err)
	}
	oursHash, err := ours.WorkingRoot().HashOf()
	if err != nil {
		return nil, nil, fmt.Errorf("hashing ours root: %w", err)
	}
	theirsHash, err := theirs.WorkingRoot().HashOf()
	if err != nil {
		return nil, nil, fmt.Errorf("hashing theirs root: %w", err)
	}

	// One side changed nothing, so there is nothing to reconcile.
	if baseHash == oursHash {
		return theirs, nil, nil
	}
	if baseHash == theirsHash {
		return ours, nil, nil
	}
	// Deliberately no fast path for ours == theirs. Two sides reaching the
	// identical result is the convergent edit, which is the case the Touched
	// modes exist to refuse: two compare-and-swap increments from the same
	// base both produce v=n+1, and short-circuiting on the equal hash would
	// accept both and lose one. Only the merge mode may decide it.

	oursAM, err := amFromWorkingRoot(ctx, ours.WorkingRoot(), state.ns)
	if err != nil {
		return nil, nil, fmt.Errorf("deriving ours AM: %w", err)
	}
	theirsAM, err := amFromWorkingRoot(ctx, theirs.WorkingRoot(), state.ns)
	if err != nil {
		return nil, nil, fmt.Errorf("deriving theirs AM: %w", err)
	}
	baseAM, err := amFromWorkingRoot(ctx, base.WorkingRoot(), state.ns)
	if err != nil {
		return nil, nil, fmt.Errorf("deriving base AM: %w", err)
	}

	mergedAM, conflicts, viewConflicts, metaConflicts, err := mergeAddressMapsWithConflicts(
		ctx, state, oursAM, theirsAM, baseAM, theirsHash, baseHash,
		"your write (ours)", "the branch (theirs)")
	if err != nil {
		return nil, nil, err
	}

	if len(conflicts) > 0 || len(viewConflicts) > 0 || len(metaConflicts) > 0 {
		unresolved := &mergeInProgress{
			intoBranch:    branch,
			fromBranch:    branch,
			premergeAM:    oursAM,
			intoHash:      theirsHash,
			fromHash:      theirsHash,
			conflicts:     conflicts,
			viewConflicts: viewConflicts,
			metaConflicts: metaConflicts,
			resolvedAM:    mergedAM,
		}
		return nil, unresolved, &backends.MergeConflictError{Conflicts: unresolved.summaries()}
	}

	mergedRV, err := rootValueFromAM(ctx, state, mergedAM)
	if err != nil {
		return nil, nil, fmt.Errorf("building merged root: %w", err)
	}
	return ours.WithWorkingRoot(mergedRV).WithStagedRoot(mergedRV), nil, nil
}
