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
	"time"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
)

// doltCommitSessionIsolation is the --session-isolation variant of
// DumboDBCommit. It merges the calling connection's per-branch overlay back
// into the committed working set via three-way merge and, on success,
// advances HEAD with a new dolt commit.
func (b *Backend) doltCommitSessionIsolation(ctx context.Context, params *backends.CommitParams, db *dbState, branch, message string, ts time.Time) (*backends.CommitResult, error) {
	ci := conninfo.GetIfPresent(ctx)
	if ci == nil {
		return nil, fmt.Errorf("DumboDBCommit: no connection info in context")
	}
	owner := ci.Owner()

	db.mu.Lock()
	defer db.mu.Unlock()

	entry, ok := db.pendingWS[pendingWSKey{owner, branch}]
	if !ok {
		return nil, backends.ErrEmptyCommit
	}

	committed, ok := db.workingSets[branch]
	if !ok {
		committed = entry.base
	}

	sess := b.NewSession()
	sqlCtx := sqlctx.Wrap(ctx, sess)

	sqlDB, sqlOk, err := b.provider.getOrBuildSqleDatabase(sqlCtx, params.DBName)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: resolving sqle.Database for %q: %w", params.DBName, err)
	}
	if !sqlOk {
		return nil, fmt.Errorf("DumboDBCommit: sqle.Database not found for %q", params.DBName)
	}

	merged, err := mergePendingIntoCommitted(sqlCtx, sqlDB.GetTableResolver(), entry.base, entry.current, committed)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: %w", err)
	}

	db.workingSets[branch] = merged

	mergedAM, err := amFromWorkingRoot(ctx, merged.WorkingRoot(), db.ns)
	if err != nil {
		return nil, fmt.Errorf("DumboDBCommit: deriving merged AM: %w", err)
	}
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

	delete(db.pendingWS, pendingWSKey{owner, branch})

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
