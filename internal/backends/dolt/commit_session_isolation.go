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
	if !sok || sessState.WorkingSet() == nil {
		return nil, backends.ErrEmptyCommit
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	// CommitWorkingSet clears the session's tx, and the next EnsureTxn
	// will StartTransaction which calls clear() and wipes ALL branchStates.
	// Save the overlays for other branches before committing so we can
	// restore them after.
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

	// CommitWorkingSet writes the merged WS to disk but does NOT update
	// the session's branchState (commitBranchState only does that on the
	// DoltCommit path), so we reload the merged WS from the ref below.
	if err := sess.CommitWorkingSet(sqlCtx, qualified, tx); err != nil {
		// dsess wraps ErrRetryTransaction in sql.ErrLockDeadlock without
		// %w, so detect by message and re-surface as a data-conflict.
		if strings.Contains(err.Error(), "this transaction conflicts with a committed transaction") {
			return nil, fmt.Errorf("DumboDBCommit: data conflict during merge: %w", err)
		}
		return nil, fmt.Errorf("DumboDBCommit: merging working set: %w", err)
	}

	merged, err := db.doltDB.ResolveWorkingSet(ctx, doltref.NewWorkingSetRef("heads/"+branch))
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: reloading merged working set for %q: %w", branch, err)
	}

	db.workingSets[branch] = merged

	mergedAM, err := amFromWorkingRoot(ctx, merged.WorkingRoot(), db.ns)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: deriving merged AM: %w", err)
	}

	// dsess.DoltCommit would advance HEAD in one shot but needs a
	// PendingCommit constructed from internals; the AM helper preserves
	// the existing user-message/author/ts contract.
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

	sqlCtx.SetTransaction(nil)

	// Restore preserved overlays in a fresh tx so SetWorkingSet has
	// branchStates to write into; the new tx's dbStartPoints snapshots
	// the post-commit noms root for the preserved branches.
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
