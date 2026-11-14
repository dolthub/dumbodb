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
	"io"
	"log/slog"
	"testing"

	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
)

// A merge commit is built on the HEAD it read, with that HEAD as its first
// parent. If HEAD moves before the commit is written while the working set
// stays put, the commit must not land: it would replace the moved HEAD and
// drop the commit it pointed at from the branch.
func TestMergeRefusesMovedHead(t *testing.T) {
	ctx := context.Background()
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	const dbName = "headmoved"
	writerCtx, _ := ctxWithSessionAs(t, be, "writer-lsid", "")
	db, err := be.Database(dbName)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(writerCtx, &backends.CreateCollectionParams{Name: "items"}))
	commitOn := func(branch string) hash.Hash {
		res, err := be.DumboDBCommit(writerCtx, &backends.CommitParams{DBName: dbName, Branch: branch, Message: branch, Author: "a"})
		require.NoError(t, err)
		return hash.Parse(res.CommitID)
	}
	commitOn(defaultBranch)
	for _, branch := range []string{"feature", "other"} {
		_, err = be.DumboDBBranch(writerCtx, &backends.BranchParams{DBName: dbName, Action: "add", From: defaultBranch, Name: branch})
		require.NoError(t, err)
		insertInTransaction(t, be, writerCtx, dbName+"@"+branch, "on-"+branch)
		commitTransaction(t, be, writerCtx)
	}
	commitOn("feature")
	otherHead := commitOn("other")

	state, err := be.getOrOpenDB(ctx, dbName, false)
	require.NoError(t, err)
	fired := false
	hook := func() {
		if !fired {
			fired = true
			require.NoError(t, state.doltDB.SetHead(ctx, doltref.NewBranchRef(defaultBranch), otherHead))
		}
	}
	_, err = be.DumboDBMerge(context.WithValue(writerCtx, branchOpHookKey{}, hook), &backends.MergeParams{
		DBName: dbName, Into: defaultBranch, From: "feature", NoFF: true, Author: "a",
	})
	require.True(t, fired)
	require.ErrorIs(t, err, backends.ErrWriteRaced)
	head, err := branchHeadHash(ctx, state, defaultBranch)
	require.NoError(t, err)
	require.Equal(t, otherHead, head, "the merge replaced a HEAD it was not built on")
}
