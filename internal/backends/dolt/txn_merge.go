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
	"fmt"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/merge"
	"github.com/dolthub/dolt/go/libraries/doltcore/table/editor"
)

// mergePendingIntoCommitted produces the post-commit working set for a
// transaction by three-way merging the txn's pending changes (ours) against
// the latest committed state (theirs), using the txn's start-of-life snapshot
// (base) as the common ancestor. Fast paths short-circuit when one side made
// no changes; otherwise dolt's merge.MergeRoots performs a row-level merge.
func mergePendingIntoCommitted(
	sqlCtx *sql.Context,
	resolver doltdb.TableResolver,
	base, ours, theirs *doltdb.WorkingSet,
) (*doltdb.WorkingSet, error) {
	baseHash, err := base.WorkingRoot().HashOf()
	if err != nil {
		return nil, fmt.Errorf("merge: hashing base root: %w", err)
	}
	oursHash, err := ours.WorkingRoot().HashOf()
	if err != nil {
		return nil, fmt.Errorf("merge: hashing ours root: %w", err)
	}
	theirsHash, err := theirs.WorkingRoot().HashOf()
	if err != nil {
		return nil, fmt.Errorf("merge: hashing theirs root: %w", err)
	}

	if baseHash == oursHash {
		return theirs, nil
	}
	if baseHash == theirsHash {
		return ours, nil
	}
	if oursHash == theirsHash {
		return ours, nil
	}

	result, err := merge.MergeRoots(
		sqlCtx,
		resolver,
		ours.WorkingRoot(), theirs.WorkingRoot(), base.WorkingRoot(),
		theirs, base,
		editor.Options{},
		merge.MergeOpts{},
	)
	if err != nil {
		return nil, fmt.Errorf("merge: %w", err)
	}
	if result.HasSchemaConflicts() {
		return nil, fmt.Errorf("merge: schema conflict on transaction commit")
	}
	for tbl, stats := range result.Stats {
		if stats.DataConflicts > 0 {
			return nil, fmt.Errorf("merge: data conflict on %q (%d row(s)); commit rejected", tbl.Name, stats.DataConflicts)
		}
	}

	return ours.WithWorkingRoot(result.Root).WithStagedRoot(result.Root), nil
}
