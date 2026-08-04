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
	"strings"
	"time"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/store/prolly"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
)

// doltCommitSessionIsolation is the --session-isolation variant of
// DumboDBCommit. Per-branch overlays live on the session's branchState;
// CommitWorkingSet runs dsess's three-way merge against HEAD, then the
// explicit dolt commit on top preserves the user message/author/ts.
func (b *Backend) doltCommitSessionIsolation(ctx context.Context, params *backends.CommitParams, db *dbState, branch, message string, ts time.Time) (*backends.CommitResult, error) {
	ci := conninfo.GetIfPresent(ctx)
	if ci == nil {
		return nil, fmt.Errorf("DumboDBCommit: no connection info in context")
	}
	owner := ci.Owner()

	sess := b.sessionForOwner(owner)
	if sess == nil {
		return nil, backends.ErrEmptyCommit
	}
	tx := sess.GetTransaction()
	if tx == nil {
		return nil, backends.ErrEmptyCommit
	}
	sqlCtx := sqlctx.Wrap(ctx, sess)
	qualified := qualifiedDbName(params.DBName, branch)

	sessState, sok, err := sess.LookupDbState(sqlCtx, qualified)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: LookupDbState for %q: %w", qualified, err)
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	// commitAndReset commits am as a single-parent commit on branch's current
	// HEAD, resets the session's working set for this branch to clean, and
	// restores the other branches' overlays across the transaction clear (the
	// next StartTransaction wipes all branchStates; the committed branch reloads
	// clean from disk via persistAM).
	commitAndReset := func(am prolly.AddressMap, msg, author string) (*backends.CommitResult, error) {
		preservedWSs := make(map[string]*doltdb.WorkingSet)
		for _, q := range sess.DirtyBranchRevisions() {
			if strings.EqualFold(q, qualified) {
				continue
			}
			ss, ok, lerr := sess.LookupDbState(sqlCtx, q)
			if lerr != nil || !ok {
				continue
			}
			if ws := ss.WorkingSet(); ws != nil {
				preservedWSs[q] = ws
			}
		}

		mainDS, err := db.datasDB.GetDataset(ctx, "refs/heads/"+branch)
		if err != nil {
			return nil, fmt.Errorf("DumboDBCommit: resolving branch dataset %q: %w", branch, err)
		}
		newDS, _, err := commitCollectionsAMAs(ctx, db.datasDB, mainDS, am, msg, author, ts)
		if err != nil {
			return nil, fmt.Errorf("DumboDBCommit: persisting commit on %q: %w", branch, err)
		}
		if err := db.persistAM(ctx, branch, am); err != nil {
			return nil, fmt.Errorf("DumboDBCommit: updating working set for %q: %w", branch, err)
		}

		sqlCtx.SetTransaction(nil)
		if len(preservedWSs) > 0 {
			if _, err := sess.StartTransaction(sqlCtx, sql.ReadWrite); err != nil {
				return nil, fmt.Errorf("DumboDBCommit: restarting tx for preserved overlays: %w", err)
			}
			for q, ws := range preservedWSs {
				if err := sess.SetWorkingSet(sqlCtx, q, ws); err != nil {
					return nil, fmt.Errorf("DumboDBCommit: restoring overlay for %q: %w", q, err)
				}
			}
		}

		headHash, ok := newDS.MaybeHeadAddr()
		if !ok {
			return nil, fmt.Errorf("DumboDBCommit: no head after commit on %q", branch)
		}
		return &backends.CommitResult{
			CommitID:           headHash.String(),
			Branch:             branch,
			Message:            msg,
			Author:             author,
			Timestamp:          ts.UnixMilli(),
			Committer:          author,
			CommitterTimestamp: ts.UnixMilli(),
		}, nil
	}

	// Finalize an in-progress session commit once its conflicts are resolved:
	// commit the resolved working set on top of the current HEAD.
	if ms := db.mergeState; ms != nil && ms.isSessionCommit {
		if ms.hasUnresolvedConflicts() {
			return nil, &backends.MergeConflictError{Conflicts: ms.summaries()}
		}
		_ = clearConflictArtifacts(ctx, db, ms) // best-effort
		res, err := commitAndReset(ms.resolvedAM, message, params.Author)
		if err != nil {
			return nil, err
		}
		db.mergeState = nil
		_ = clearMergeState(db) // best-effort
		return res, nil
	}

	if !sok || sessState.WorkingSet() == nil {
		return nil, backends.ErrEmptyCommit
	}

	// A --session-isolation commit is a 3-way merge of the session's working set
	// against the current branch HEAD, run through the SAME conflict machinery as
	// doltMerge (base = the session's pinned fork-point HEAD; ours = the session
	// working set; theirs = the advanced on-disk HEAD). This replaces dsess's
	// CommitWorkingSet, which hard-errored on any conflict with no resolution path.
	baseCommit, err := sess.GetHeadCommit(sqlCtx, qualified)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: resolving session base commit: %w", err)
	}
	baseHash, err := baseCommit.HashOf()
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: hashing base commit: %w", err)
	}
	baseAM, err := amFromCommitHash(ctx, db, baseHash.String())
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: loading base AM: %w", err)
	}
	oursAM, err := amFromWorkingRoot(ctx, sessState.WorkingSet().WorkingRoot(), db.ns)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: deriving session AM: %w", err)
	}
	theirsDS, err := db.datasDB.GetDataset(ctx, "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: resolving branch dataset %q: %w", branch, err)
	}
	theirHash, ok := theirsDS.MaybeHeadAddr()
	if !ok {
		return nil, fmt.Errorf("DumboDBCommit: no HEAD on %q", branch)
	}
	theirsAM, err := amFromCommitHash(ctx, db, theirHash.String())
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: loading HEAD AM: %w", err)
	}

	mergedAM, conflicts, viewConflicts, metaConflicts, err := mergeAddressMapsWithConflicts(
		ctx, db, oursAM, theirsAM, baseAM, theirHash, baseHash,
		"your session (ours)", "the branch (theirs)")
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: merging session working set: %w", err)
	}

	if len(conflicts) > 0 || len(viewConflicts) > 0 || len(metaConflicts) > 0 {
		db.mergeState = &mergeInProgress{
			intoBranch:      branch,
			fromBranch:      branch,
			premergeAM:      oursAM,
			intoHash:        theirHash,
			fromHash:        theirHash,
			conflicts:       conflicts,
			viewConflicts:   viewConflicts,
			metaConflicts:   metaConflicts,
			resolvedAM:      mergedAM,
			isSessionCommit: true,
		}
		// Unlike doltMerge, a session commit must NOT touch the shared branch AM
		// or working set: other sessions read those and would see this session's
		// in-progress merge. The conflict/resolution state lives entirely in
		// db.mergeState (in memory) until finalize commits it.
		return nil, &backends.MergeConflictError{Conflicts: db.mergeState.summaries()}
	}

	// Clean merge: commit the merged working set on top of HEAD.
	return commitAndReset(mergedAM, message, params.Author)
}
