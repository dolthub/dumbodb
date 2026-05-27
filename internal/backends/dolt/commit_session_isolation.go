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

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
)

// doltCommitSessionIsolation is the --session-isolation variant of
// DumboDBCommit. The per-branch overlay lives on the calling session's
// branchState (see workspace-qsc.2); CommitWorkingSet runs dsess's
// three-way merge against current HEAD, the explicit dolt commit on top
// preserves the user-supplied message/author/timestamp.
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

	// Confirm the session's branchState for this (db, branch) has
	// uncommitted changes; otherwise there is nothing to commit.
	sessState, sok, err := sess.LookupDbState(sqlCtx, qualified)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: LookupDbState for %q: %w", qualified, err)
	}
	if !sok || sessState.WorkingSet() == nil {
		return nil, backends.ErrEmptyCommit
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	// dsess.commitBranchState ends the session's transaction (ctx.SetTransaction(nil))
	// and the next command's EnsureTxn will call StartTransaction, which
	// calls clear() and wipes ALL branchStates. That destroys uncommitted
	// overlays on branches other than the one being committed -- breaking
	// dumbodb's per-(session, branch) isolation. Save those overlays
	// before the commit and restore them after.
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

	// dsess.CommitWorkingSet runs the three-way merge (base from
	// tx.dbStartPoints, ours from branchState, theirs from current HEAD)
	// and writes the merged working set to disk. It does NOT update the
	// session's branchState (commitBranchState only writes back on a
	// DoltCommit path), so we reload the merged WS from the working_set
	// ref to feed the dolt commit below.
	if err := sess.CommitWorkingSet(sqlCtx, qualified, tx); err != nil {
		// dsess wraps ErrRetryTransaction in sql.ErrLockDeadlock (no %w),
		// so detect it by message rather than errors.Is. Surface as a
		// data-conflict error so callers (and the session-isolation
		// parity tests) see the familiar wording.
		if strings.Contains(err.Error(), "this transaction conflicts with a committed transaction") {
			return nil, fmt.Errorf("DumboDBCommit: data conflict during merge: %w", err)
		}
		return nil, fmt.Errorf("DumboDBCommit: merging working set: %w", err)
	}

	merged, err := db.doltDB.ResolveWorkingSet(ctx, doltref.NewWorkingSetRef("heads/"+branch))
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: reloading merged working set for %q: %w", branch, err)
	}

	// Mirror back into dbState.workingSets for non-session readers (still
	// present until workspace-qsc.6).
	db.workingSets[branch] = merged

	mergedAM, err := amFromWorkingRoot(ctx, merged.WorkingRoot(), db.ns)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: deriving merged AM: %w", err)
	}

	// Advance HEAD with an explicit dolt commit carrying the user-supplied
	// message/author/timestamp. dsess.DoltCommit would do this in one shot
	// but requires a PendingCommit constructed from internals; the
	// AM-based commit helper keeps the existing semantics intact.
	mainDS, err := db.datasDB.GetDataset(ctx, "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: resolving branch dataset %q: %w", branch, err)
	}
	newDS, _, err := commitCollectionsAMAs(ctx, db.datasDB, mainDS, mergedAM, message, params.Author, ts)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: persisting commit on %q: %w", branch, err)
	}

	if err := db.persistAM(ctx, branch, mergedAM); err != nil {
		return nil, fmt.Errorf("DumboDBCommit: updating working set for %q: %w", branch, err)
	}

	// Clear the session's transaction; the next write starts a fresh dsess
	// txn with a fresh dbStartPoints snapshot covering the new HEAD.
	sqlCtx.SetTransaction(nil)

	// Restore the preserved overlays from before the commit. Start a fresh
	// dsess transaction here (rather than waiting for the next command)
	// so SetWorkingSet has a branchState to write into; the new txn's
	// dbStartPoints reflects the post-commit noms root so merge bases for
	// the preserved branches stay current.
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
		Message:            message,
		Author:             params.Author,
		Timestamp:          ts.UnixMilli(),
		Committer:          params.Author,
		CommitterTimestamp: ts.UnixMilli(),
	}, nil
}
