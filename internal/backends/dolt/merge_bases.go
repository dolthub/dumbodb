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

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/merge"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
)

// mergeBases returns every merge base of |ours| and |theirs|, newest committer date first, as dolt_merge_bases()
// lists them.
func (db *dbState) mergeBases(ctx context.Context, ours, theirs hash.Hash) ([]hash.Hash, error) {
	oursCommit, err := db.doltCommit(ctx, ours)
	if err != nil {
		return nil, err
	}
	theirsCommit, err := db.doltCommit(ctx, theirs)
	if err != nil {
		return nil, err
	}
	return merge.MergeBases(ctx, oursCommit, theirsCommit)
}

// mergeBase returns the merge base that a merge of |ours| and |theirs| uses, and false when they have none. When
// they have several, it is the one datas.FindCommonAncestor picks.
func (db *dbState) mergeBase(ctx context.Context, ours, theirs hash.Hash) (hash.Hash, bool, error) {
	oursCommit, err := datas.LoadCommitAddr(ctx, db.vs, ours)
	if err != nil {
		return hash.Hash{}, false, err
	}
	theirsCommit, err := datas.LoadCommitAddr(ctx, db.vs, theirs)
	if err != nil {
		return hash.Hash{}, false, err
	}
	return datas.FindCommonAncestor(ctx, oursCommit, theirsCommit, db.vs, db.vs, db.ns, db.ns)
}

func (db *dbState) doltCommit(ctx context.Context, h hash.Hash) (*doltdb.Commit, error) {
	optCmt, err := db.doltDB.ReadCommit(ctx, h)
	if err != nil {
		return nil, err
	}
	commit, ok := optCmt.ToCommit()
	if !ok {
		return nil, doltdb.ErrGhostCommitEncountered
	}
	return commit, nil
}
