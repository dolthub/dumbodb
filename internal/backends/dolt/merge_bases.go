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
	"time"

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

// docFlag marks a document of a virtual merge base whose value its merge bases did not agree on: the whole document,
// or the field paths they conflicted on. id is the document's _id.
type docFlag struct {
	id    any
	whole bool
	paths [][]string
}

// virtualBaseFlags holds the flagged documents of a virtual merge base, by collection and by document key.
type virtualBaseFlags map[string]map[string]docFlag

const (
	virtualMergeBaseName        = "dumbodb"
	virtualMergeBaseEmail       = "virtual-merge-base@dolthub.com"
	virtualMergeBaseDescription = "virtual merge base"
)

// mergeBase returns the merge base that a merge of |ours| and |theirs| uses, and false when they have none. When
// they have several, it is a virtual merge base: a commit that no branch references, which merges them as git's
// merge-ort does.
func (db *dbState) mergeBase(ctx context.Context, ours, theirs hash.Hash) (hash.Hash, bool, error) {
	bases, err := db.mergeBases(ctx, ours, theirs)
	if errors.Is(err, doltdb.ErrNoCommonAncestor) {
		return hash.Hash{}, false, nil
	}
	if err != nil {
		return hash.Hash{}, false, err
	}
	if len(bases) == 1 {
		return bases[0], true, nil
	}
	virtualBase, _, err := db.virtualMergeBase(ctx, bases)
	if err != nil {
		return hash.Hash{}, false, fmt.Errorf("building the virtual merge base: %w", err)
	}
	return virtualBase, true, nil
}

// virtualMergeBase merges |bases|, which are newest first, oldest first into a dangling commit and returns its hash
// and flags.
func (db *dbState) virtualMergeBase(ctx context.Context, bases []hash.Hash) (hash.Hash, virtualBaseFlags, error) {
	merged, err := db.doltCommit(ctx, bases[len(bases)-1])
	if err != nil {
		return hash.Hash{}, nil, err
	}
	for i := len(bases) - 2; i >= 0; i-- {
		next, err := db.doltCommit(ctx, bases[i])
		if err != nil {
			return hash.Hash{}, nil, err
		}
		if merged, err = db.mergeIntoVirtualBase(ctx, merged, next); err != nil {
			return hash.Hash{}, nil, err
		}
	}
	h, err := merged.HashOf()
	return h, nil, err
}

// mergeIntoVirtualBase merges |x| and |y| against their own merge base into a dangling commit. Its metadata is fixed
// and dated with the later of their committer dates, so that the same merge bases always give the same commit.
func (db *dbState) mergeIntoVirtualBase(ctx context.Context, x, y *doltdb.Commit) (*doltdb.Commit, error) {
	xHash, err := x.HashOf()
	if err != nil {
		return nil, err
	}
	yHash, err := y.HashOf()
	if err != nil {
		return nil, err
	}
	baseHash, ok, err := db.mergeBase(ctx, xHash, yHash)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("merge bases %s and %s have no common ancestor", xHash, yHash)
	}
	xAM, err := amFromCommitHash(ctx, db, xHash.String())
	if err != nil {
		return nil, err
	}
	yAM, err := amFromCommitHash(ctx, db, yHash.String())
	if err != nil {
		return nil, err
	}
	baseAM, err := amFromCommitHash(ctx, db, baseHash.String())
	if err != nil {
		return nil, err
	}
	mergedAM, conflicts, viewConflicts, metaConflicts, deferred, err := mergeAddressMapsWithConflicts(ctx, db, xAM, yAM, baseAM,
		yHash, baseHash, "merge base "+xHash.String(), "merge base "+yHash.String())
	if err != nil {
		return nil, err
	}
	if len(conflicts) > 0 || len(viewConflicts) > 0 || len(metaConflicts) > 0 || len(deferred) > 0 {
		return nil, fmt.Errorf("merge bases %s and %s conflict", xHash, yHash)
	}
	root, err := rootValueFromAM(ctx, db, mergedAM)
	if err != nil {
		return nil, err
	}
	date, err := laterCommitterDate(ctx, x, y)
	if err != nil {
		return nil, err
	}
	ident := datas.CommitIdent{Name: virtualMergeBaseName, Email: virtualMergeBaseEmail, Date: datas.CommitDateAt(date)}
	meta, err := datas.NewCommitMetaWithAuthorCommitter(ident, ident, virtualMergeBaseDescription)
	if err != nil {
		return nil, err
	}
	return doltdb.NewDanglingCommit(ctx, root, []*doltdb.Commit{x, y}, meta)
}

func laterCommitterDate(ctx context.Context, x, y *doltdb.Commit) (time.Time, error) {
	xMeta, err := x.GetCommitMeta(ctx)
	if err != nil {
		return time.Time{}, err
	}
	yMeta, err := y.GetCommitMeta(ctx)
	if err != nil {
		return time.Time{}, err
	}
	xDate, yDate := xMeta.Committer.Date.Time(), yMeta.Committer.Date.Time()
	if yDate.After(xDate) {
		return yDate, nil
	}
	return xDate, nil
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
