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

	"github.com/dolthub/dolt/go/libraries/doltcore/ref"

	"github.com/dolthub/dumbodb/internal/backends"
)

// DumboDBPull mirrors git pull: it fetches from the branch's remote (updating
// every tracking ref) and then merges the fetched commit for the current branch
// into that branch. Conflicts surface as the same *MergeConflictError as
// DumboDBMerge, leaving the branch staged for resolution.
func (b *Backend) DumboDBPull(ctx context.Context, params *backends.PullParams) (*backends.PullResult, error) {
	branch := params.Branch
	if branch == "" {
		branch = defaultBranch
	}

	remote := params.Remote
	if remote == "" {
		up, ok, err := b.getUpstream(ctx, params.DBName, branch)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("dumboPull: branch %q has no upstream; specify a remote with 'from'", branch)
		}
		remote = up.remote
	}

	// Resolve the effective pull behavior: explicit arguments override the
	// branch's stored pull policy, exactly as git command-line flags override
	// branch.<name>.rebase and pull.ff.
	policy, err := b.getPullPolicy(ctx, params.DBName, branch)
	if err != nil {
		return nil, err
	}
	rebaseMode := policy.rebase
	if params.RebaseSet {
		rebaseMode = params.Rebase
	}
	noFF, ffOnly := params.NoFF, params.FFOnly
	if !params.FFSet {
		noFF = policy.ff == "no"
		ffOnly = policy.ff == "only"
	}
	// "merges" currently performs a plain rebase; rebase-merges topology
	// preservation is a follow-up (the rebase engine lacks --rebase-merges).
	doRebase := rebaseMode == "true" || rebaseMode == "merges"

	state, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, err
	}
	branchRef := ref.NewBranchRef(branch)
	beforeCommit, err := state.doltDB.ResolveCommitRef(ctx, branchRef)
	if err != nil {
		return nil, fmt.Errorf("dumboPull: resolving branch %q: %w", branch, err)
	}
	beforeHash, err := beforeCommit.HashOf()
	if err != nil {
		return nil, err
	}
	before := beforeHash.String()

	// Fetch updates every tracking ref; take the fetched commit for this branch.
	fetchRes, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: params.DBName, Remote: remote})
	if err != nil {
		return nil, err
	}
	var fetched string
	for _, fr := range fetchRes.Branches {
		if fr.Branch == branch {
			fetched = fr.Commit
		}
	}
	if fetched == "" {
		return nil, fmt.Errorf("dumboPull: remote %q has no branch %q", remote, branch)
	}

	// Already up to date: the branch head equals the fetched commit.
	if before == fetched {
		return &backends.PullResult{
			Remote:          remote,
			Branch:          branch,
			CommitBefore:    before,
			CommitAfter:     before,
			AlreadyUpToDate: true,
		}, nil
	}

	// Rebase path: replay the branch's local commits onto the fetched commit
	// instead of merging. A *DumboDBRebaseConflictError propagates to the handler.
	if doRebase {
		rebaseRes, err := b.DumboDBRebase(ctx, &backends.RebaseParams{
			DBName: params.DBName,
			Branch: branch,
			Onto:   fetched,
		})
		if err != nil {
			return nil, err
		}
		return &backends.PullResult{
			Remote:       remote,
			Branch:       branch,
			CommitBefore: before,
			CommitAfter:  rebaseRes.NewTip,
			// A rebase with no local commits to replay lands exactly on the
			// fetched commit -- a fast-forward.
			FastForward: rebaseRes.NewTip == fetched,
			Rebased:     true,
		}, nil
	}

	// Merge the fetched commit into the branch. A commit hash resolves cleanly;
	// a *MergeConflictError propagates to the handler unchanged.
	mergeRes, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName:  params.DBName,
		Into:    branch,
		From:    fetched,
		NoFF:    noFF,
		FFOnly:  ffOnly,
		Message: params.Message,
		Author:  params.Author,
	})
	if err != nil {
		return nil, err
	}

	// A fast-forward moves the branch to exactly the fetched commit; anything
	// else is a merge commit.
	return &backends.PullResult{
		Remote:       remote,
		Branch:       branch,
		CommitBefore: before,
		CommitAfter:  mergeRes.CommitID,
		FastForward:  mergeRes.CommitID == fetched,
	}, nil
}
