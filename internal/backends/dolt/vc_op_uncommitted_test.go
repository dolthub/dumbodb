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
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// vcOpFixture sets up db with a base commit, a feature branch with one commit,
// and one more commit on main, so a merge of feature into main is a true
// three-way merge. It returns the feature and main commit ids.
func vcOpFixture(t *testing.T, b *Backend) (featureCommit, mainCommit string) {
	t.Helper()
	ctx := context.Background()
	insertDoc(t, b, "db", "c", must.NotFail(types.NewDocument("_id", "base")))
	commitDB(t, b, "db", "base")
	_, err := b.DumboDBBranch(ctx, &backends.BranchParams{DBName: "db", Action: "add", From: "main", Name: "feature"})
	require.NoError(t, err)
	insertDoc(t, b, "db@feature", "c", must.NotFail(types.NewDocument("_id", "feat")))
	res, err := b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "db", Branch: "feature", Message: "feat", Author: "a"})
	require.NoError(t, err)
	insertDoc(t, b, "db", "c", must.NotFail(types.NewDocument("_id", "main-committed")))
	return res.CommitID, commitDB(t, b, "db", "main")
}

// vcOps runs each operation on main and returns its error.
func vcOps(featureCommit, mainCommit string) map[string]func(context.Context, *Backend) error {
	return map[string]func(context.Context, *Backend) error{
		"merge": func(ctx context.Context, b *Backend) error {
			_, err := b.DumboDBMerge(ctx, &backends.MergeParams{DBName: "db", Into: "main", From: "feature"})
			return err
		},
		"cherry-pick": func(ctx context.Context, b *Backend) error {
			_, err := b.DumboDBCherryPick(ctx, &backends.CherryPickParams{DBName: "db", Branch: "main", Commit: featureCommit})
			return err
		},
		"revert": func(ctx context.Context, b *Backend) error {
			_, err := b.DumboDBRevert(ctx, &backends.RevertParams{DBName: "db", Branch: "main", Commit: mainCommit})
			return err
		},
		"rebase": func(ctx context.Context, b *Backend) error {
			_, err := b.DumboDBRebase(ctx, &backends.RebaseParams{DBName: "db", Branch: "main", Onto: "feature"})
			return err
		},
	}
}

func docIDs(t *testing.T, b *Backend, ctx context.Context, dbName, collName string) map[any]bool {
	t.Helper()
	db := must.NotFail(b.Database(dbName))
	coll, err := db.Collection(collName)
	require.NoError(t, err)
	res, err := coll.Query(ctx, &backends.QueryParams{})
	require.NoError(t, err)
	defer res.Iter.Close()
	ids := map[any]bool{}
	for {
		_, doc, err := res.Iter.Next()
		if err != nil {
			break
		}
		ids[must.NotFail(doc.Get("_id"))] = true
	}
	return ids
}

// A merge, cherry-pick, revert or rebase refuses to start when the branch has
// uncommitted changes, and leaves them and the branch as they were. A server's
// working set is shared by every session, so the operation cannot carry one
// client's uncommitted writes through, as git would (dolthub/dumbodb#167).
func TestVCOpsRefuseUncommittedChanges(t *testing.T) {
	for _, name := range []string{"merge", "cherry-pick", "revert", "rebase", "fast-forward merge"} {
		t.Run(name, func(t *testing.T) {
			b := newTestBackend(t)
			ctx := context.Background()
			var op func(context.Context, *Backend) error
			if name == "fast-forward merge" {
				insertDoc(t, b, "db", "c", must.NotFail(types.NewDocument("_id", "base")))
				commitDB(t, b, "db", "base")
				_, err := b.DumboDBBranch(ctx, &backends.BranchParams{DBName: "db", Action: "add", From: "main", Name: "feature"})
				require.NoError(t, err)
				insertDoc(t, b, "db@feature", "c", must.NotFail(types.NewDocument("_id", "feat")))
				_, err = b.DumboDBCommit(ctx, &backends.CommitParams{DBName: "db", Branch: "feature", Message: "feat", Author: "a"})
				require.NoError(t, err)
				op = vcOps("", "")["merge"]
			} else {
				op = vcOps(vcOpFixture(t, b))[name]
			}
			headBefore := must.NotFail(resolveRootishToCommitHash(ctx, must.NotFail(b.getOrOpenDB(ctx, "db", false)), "main"))
			insertDoc(t, b, "db", "c", must.NotFail(types.NewDocument("_id", "uncommitted")))

			err := op(ctx, b)
			require.ErrorIs(t, err, backends.ErrUncommittedChanges)
			require.ErrorContains(t, err, `"c"`, "the error names the collection with uncommitted changes")
			require.True(t, docIDs(t, b, context.Background(), "db", "c")["uncommitted"], "the uncommitted write must survive")
			headAfter := must.NotFail(resolveRootishToCommitHash(ctx, must.NotFail(b.getOrOpenDB(ctx, "db", false)), "main"))
			require.Equal(t, headBefore, headAfter, "a refused operation must not move the branch")

			commitDB(t, b, "db", "commit the write")
			require.NoError(t, op(ctx, b), "once committed, the operation proceeds")
		})
	}
}

// raceFixture is a backend with sessions and a database whose collection
// "items" holds one committed document, "base", on main and on a branch
// "feature" taken from it.
type raceFixture struct {
	t        *testing.T
	be       *Backend
	writer   context.Context
	op       context.Context
	db       string
	commitID map[string]string
}

func newRaceFixture(t *testing.T) *raceFixture {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)
	f := &raceFixture{t: t, be: be, db: "vcrace", commitID: map[string]string{}}
	f.writer, _ = ctxWithSessionAs(t, be, "writer-lsid", "")
	f.op, _ = ctxWithSessionAs(t, be, "op-lsid", "")
	db, err := be.Database(f.db)
	require.NoError(t, err)
	require.NoError(t, db.CreateCollection(f.writer, &backends.CreateCollectionParams{Name: "items"}))
	f.commitDoc(defaultBranch, "base", must.NotFail(types.NewDocument("_id", "base")))
	_, err = be.DumboDBBranch(f.op, &backends.BranchParams{DBName: f.db, Action: "add", From: defaultBranch, Name: "feature"})
	require.NoError(t, err)
	return f
}

func (f *raceFixture) target(branch string) string {
	if branch == defaultBranch {
		return f.db
	}
	return f.db + "@" + branch
}

// commitDoc inserts doc on branch and commits it as a commit named name.
func (f *raceFixture) commitDoc(branch, name string, doc *types.Document) {
	insertDocInTransaction(f.t, f.be, f.writer, f.target(branch), doc)
	commitTransaction(f.t, f.be, f.writer)
	res, err := f.be.DumboDBCommit(f.op, &backends.CommitParams{DBName: f.db, Branch: branch, Message: name, Author: "a"})
	require.NoError(f.t, err)
	f.commitID[name] = res.CommitID
}

func (f *raceFixture) head() string {
	state, err := f.be.getOrOpenDB(context.Background(), f.db, false)
	require.NoError(f.t, err)
	h, err := resolveRootishToCommitHash(context.Background(), state, defaultBranch)
	require.NoError(f.t, err)
	return h.String()
}

func (f *raceFixture) mergeOp() string {
	st, err := f.be.DumboDBStatus(f.op, &backends.VersioningStatusParams{DBName: f.db, Branch: defaultBranch})
	require.NoError(f.t, err)
	return st.MergeOp
}

func (f *raceFixture) ids() map[any]bool {
	readerCtx, _ := ctxWithSessionAs(f.t, f.be, "reader-lsid", "")
	return docIDs(f.t, f.be, readerCtx, f.db, "items")
}

// racing returns a context whose operation, after reading the branch,
// publishes a session write of document id before its first compare-and-swap.
func (f *raceFixture) racing(id string) context.Context {
	insertInTransaction(f.t, f.be, f.writer, f.db, id)
	fired := false
	return context.WithValue(f.op, branchOpHookKey{}, func() {
		if fired {
			return
		}
		fired = true
		require.NoError(f.t, f.be.OnTransactionCommit(f.writer, conninfo.Get(f.writer).Owner()))
	})
}

func (f *raceFixture) mergeFeature(ctx context.Context) error {
	_, err := f.be.DumboDBMerge(ctx, &backends.MergeParams{DBName: f.db, Into: defaultBranch, From: "feature", Author: "a"})
	return err
}

func (f *raceFixture) rebaseOntoFeature(ctx context.Context) error {
	_, err := f.be.DumboDBRebase(ctx, &backends.RebaseParams{DBName: f.db, Branch: defaultBranch, Onto: "feature"})
	return err
}

// A client write published while a merge, cherry-pick, revert or rebase runs
// makes the operation fail with the retryable ErrWriteRaced, leaving the
// branch, its HEAD and the operation state as they were, instead of being
// overwritten by a result computed without it (dolthub/dumbodb#167). The write
// is then uncommitted, so the operation is refused until it is committed.
func TestVCOpsFailWhenAWriteLandsWhileTheyRun(t *testing.T) {
	x := func(v int32) *types.Document { return must.NotFail(types.NewDocument("_id", "x", "v", v)) }
	doc := func(id string) *types.Document { return must.NotFail(types.NewDocument("_id", id)) }
	for _, tc := range []struct {
		name  string
		setup func(f *raceFixture)
		run   func(f *raceFixture, ctx context.Context) error
	}{
		{"merge", func(f *raceFixture) {
			f.commitDoc("feature", "feat", doc("feat"))
			f.commitDoc(defaultBranch, "main", doc("main"))
		}, (*raceFixture).mergeFeature},
		{"fast-forward merge", func(f *raceFixture) {
			f.commitDoc("feature", "feat", doc("feat"))
		}, (*raceFixture).mergeFeature},
		{"merge pausing on a conflict", func(f *raceFixture) {
			f.commitDoc("feature", "feat", x(1))
			f.commitDoc(defaultBranch, "main", x(2))
		}, (*raceFixture).mergeFeature},
		{"cherry-pick", func(f *raceFixture) {
			f.commitDoc("feature", "feat", doc("feat"))
		}, func(f *raceFixture, ctx context.Context) error {
			_, err := f.be.DumboDBCherryPick(ctx, &backends.CherryPickParams{DBName: f.db, Branch: defaultBranch, Commit: f.commitID["feat"]})
			return err
		}},
		{"revert", func(f *raceFixture) {
			f.commitDoc(defaultBranch, "main", doc("main"))
		}, func(f *raceFixture, ctx context.Context) error {
			_, err := f.be.DumboDBRevert(ctx, &backends.RevertParams{DBName: f.db, Branch: defaultBranch, Commit: f.commitID["main"], Author: "a"})
			return err
		}},
		{"rebase of two commits", func(f *raceFixture) {
			f.commitDoc("feature", "feat", doc("feat"))
			f.commitDoc(defaultBranch, "m1", doc("m1"))
			f.commitDoc(defaultBranch, "m2", doc("m2"))
		}, (*raceFixture).rebaseOntoFeature},
		{"rebase pausing on its first commit", func(f *raceFixture) {
			f.commitDoc("feature", "feat", x(1))
			f.commitDoc(defaultBranch, "m1", x(2))
		}, (*raceFixture).rebaseOntoFeature},
		{"rebase pausing on a later commit", func(f *raceFixture) {
			f.commitDoc("feature", "feat", x(1))
			f.commitDoc(defaultBranch, "m1", doc("m1"))
			f.commitDoc(defaultBranch, "m2", x(2))
		}, (*raceFixture).rebaseOntoFeature},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRaceFixture(t)
			tc.setup(f)
			headBefore := f.head()

			err := tc.run(f, f.racing("during-op"))
			require.ErrorIs(t, err, backends.ErrWriteRaced)
			require.True(t, f.ids()["during-op"], "the write published while the operation ran was lost")
			require.Equal(t, headBefore, f.head(), "an operation that lost the race moved the branch")
			require.Empty(t, f.mergeOp(), "an operation that lost the race left itself in progress")

			require.ErrorIs(t, tc.run(f, f.op), backends.ErrUncommittedChanges)
		})
	}
}

// A paused operation's own steps cannot be raced by a client write: the
// branch accepts none while it is paused. The only write that could still
// reach it -- a transaction begun before the pause -- is refused when it tries
// to commit in the middle of a resolve, continue or abort, and the step goes
// ahead as if it had not tried.
func TestPausedOpStepsCannotBeRacedByAWrite(t *testing.T) {
	x := func(v int32) *types.Document { return must.NotFail(types.NewDocument("_id", "x", "v", v)) }
	for _, tc := range []struct {
		name  string
		start func(f *raceFixture) error
		cont  func(f *raceFixture, ctx context.Context) error
		abort func(f *raceFixture, ctx context.Context) error
		op    string
	}{
		{"merge", (*raceFixture).mergeFeatureOp, func(f *raceFixture, ctx context.Context) error {
			_, err := f.be.DumboDBMerge(ctx, &backends.MergeParams{DBName: f.db, Into: defaultBranch, Continue: true, Author: "a"})
			return err
		}, func(f *raceFixture, ctx context.Context) error {
			_, err := f.be.DumboDBMerge(ctx, &backends.MergeParams{DBName: f.db, Into: defaultBranch, Abort: true})
			return err
		}, "merge"},
		{"rebase", (*raceFixture).rebaseOntoFeatureOp, func(f *raceFixture, ctx context.Context) error {
			_, err := f.be.DumboDBRebase(ctx, &backends.RebaseParams{DBName: f.db, Branch: defaultBranch, Continue: true})
			return err
		}, func(f *raceFixture, ctx context.Context) error {
			_, err := f.be.DumboDBRebase(ctx, &backends.RebaseParams{DBName: f.db, Branch: defaultBranch, Abort: true})
			return err
		}, "rebase"},
	} {
		// setup pauses the operation with a client transaction that wrote
		// before the pause and has not committed.
		setup := func(t *testing.T) *raceFixture {
			f := newRaceFixture(t)
			f.commitDoc("feature", "feat", x(1))
			f.commitDoc(defaultBranch, "m1", must.NotFail(types.NewDocument("_id", "m1")))
			f.commitDoc(defaultBranch, "m2", x(2))
			insertInTransaction(t, f.be, f.writer, f.db, "early")
			require.Error(t, tc.start(f))
			require.Equal(t, tc.op, f.mergeOp())
			return f
		}
		// committing returns a context whose step tries to commit that
		// transaction before each of its writes, and the result of the try.
		committing := func(f *raceFixture) (context.Context, *error) {
			commitErr := new(error)
			fired := false
			return context.WithValue(f.op, branchOpHookKey{}, func() {
				if !fired {
					fired = true
					*commitErr = f.be.OnTransactionCommit(f.writer, conninfo.Get(f.writer).Owner())
				}
			}), commitErr
		}
		resolve := func(f *raceFixture, ctx context.Context) error {
			conflicts, err := f.be.DumboDBConflicts(f.op, &backends.ConflictsParams{DBName: f.db, Branch: defaultBranch})
			require.NoError(f.t, err)
			_, err = f.be.DumboDBResolveConflict(ctx, &backends.ResolveConflictParams{
				DBName: f.db, Branch: defaultBranch, Collection: "items",
				ConflictID: conflicts.Collections[0].Conflicts[0].ConflictID, Resolution: "theirs",
			})
			return err
		}
		refusedAsInConflict := func(t *testing.T, err *error) {
			t.Helper()
			require.True(t, backends.ErrorCodeIs(*err, backends.ErrorCodeBranchInConflict), "the commit was not refused: %v", *err)
		}

		t.Run(tc.name+"/resolve and continue", func(t *testing.T) {
			f := setup(t)
			ctx, commitErr := committing(f)
			require.NoError(t, resolve(f, ctx))
			refusedAsInConflict(t, commitErr)
			require.NoError(t, tc.cont(f, f.op))
			require.Empty(t, f.mergeOp())
			require.False(t, f.ids()["early"], "a refused commit landed")
		})

		t.Run(tc.name+"/continue", func(t *testing.T) {
			f := setup(t)
			require.NoError(t, resolve(f, f.op))
			ctx, commitErr := committing(f)
			require.NoError(t, tc.cont(f, ctx))
			refusedAsInConflict(t, commitErr)
			require.Empty(t, f.mergeOp())
		})

		t.Run(tc.name+"/abort", func(t *testing.T) {
			f := setup(t)
			ctx, commitErr := committing(f)
			require.NoError(t, tc.abort(f, ctx))
			refusedAsInConflict(t, commitErr)
			require.Empty(t, f.mergeOp())
			require.Equal(t, f.commitID["m2"], f.head())
		})
	}
}

func (f *raceFixture) mergeFeatureOp() error      { return f.mergeFeature(f.op) }
func (f *raceFixture) rebaseOntoFeatureOp() error { return f.rebaseOntoFeature(f.op) }

func insertDocInTransaction(t *testing.T, be *Backend, ctx context.Context, dbName string, doc *types.Document) {
	t.Helper()
	sess := sessionFromContext(ctx)
	require.NotNil(t, sess)
	_, err := sess.StartTransaction(sqlctx.Wrap(ctx, sess), sql.ReadWrite)
	require.NoError(t, err)
	db, err := be.Database(dbName)
	require.NoError(t, err)
	coll, err := db.Collection("items")
	require.NoError(t, err)
	_, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{doc}})
	require.NoError(t, err)
}

// A soft reset moves HEAD and the staged root and keeps the working root. A
// client write published after the reset read the branch must not be
// overwritten by the working root it read: the reset fails with
// ErrWriteRaced and changes nothing, and succeeds when retried.
func TestSoftResetFailsWhenAWriteLandsWhileItRuns(t *testing.T) {
	f := newRaceFixture(t)
	f.commitDoc(defaultBranch, "main", must.NotFail(types.NewDocument("_id", "main")))
	headBefore := f.head()
	reset := func(ctx context.Context) error {
		_, err := f.be.DumboDBReset(ctx, &backends.ResetParams{DBName: f.db, Branch: defaultBranch, CommitID: f.commitID["base"]})
		return err
	}

	require.ErrorIs(t, reset(f.racing("during-reset")), backends.ErrWriteRaced)
	require.Equal(t, headBefore, f.head(), "a reset that lost the race moved the branch")
	require.True(t, f.ids()["during-reset"], "the write published while the reset ran was lost")

	require.NoError(t, reset(f.op))
	require.Equal(t, f.commitID["base"], f.head())
	ids := f.ids()
	require.True(t, ids["during-reset"] && ids["main"], "a soft reset keeps the working root: %v", ids)
}

// When a command's write lands but another write replaces it before the
// command reads it back, the hash its write was published under is unknown.
// The command's next write must then fail as a race rather than swap against
// a working set it never saw.
func TestBranchTipRefusesTheNextWriteOnceItLosesTrackOfItsOwn(t *testing.T) {
	f := newRaceFixture(t)
	ctx := context.Background()
	state, err := f.be.getOrOpenDB(ctx, f.db, false)
	require.NoError(t, err)
	tip, err := state.readBranchTip(ctx, defaultBranch)
	require.NoError(t, err)

	written := tip.ws.WithStagedRoot(tip.ws.StagedRoot())
	var rsc doltdb.ReplicationStatusController
	require.NoError(t, state.doltDB.UpdateWorkingSet(ctx, doltref.NewWorkingSetRef("heads/"+defaultBranch), written, tip.hash, doltdb.TodoWorkingSetMeta(), &rsc))
	insertInTransaction(t, f.be, f.writer, f.db, "replaced-it")
	commitTransaction(t, f.be, f.writer)

	require.NoError(t, state.advanceTip(ctx, tip, written))
	require.True(t, tip.lost)
	require.ErrorIs(t, state.publishOverTip(ctx, tip, tip.ws), backends.ErrWriteRaced)
	require.True(t, f.ids()["replaced-it"])
}

// A rebase writes its replayed commits before any branch references them, and
// moves the branch onto them only in its last swap. A garbage collection that
// runs meanwhile must not sweep them: the rebase runs inside a session command,
// and GC does not finish while one is in flight.
func TestRebaseCommitsSurviveAConcurrentGC(t *testing.T) {
	f := newRaceFixture(t)
	f.commitDoc("feature", "feat", must.NotFail(types.NewDocument("_id", "feat")))
	for _, id := range []string{"m1", "m2", "m3"} {
		f.commitDoc(defaultBranch, id, must.NotFail(types.NewDocument("_id", id)))
	}
	gcCtx, _ := ctxWithSessionAs(t, f.be, "gc-lsid", "")

	gcDone := make(chan error, 1)
	gcFinishedDuringRebase := false
	hook := func() {
		go func() {
			_, err := f.be.DumboDBGC(gcCtx, &backends.GCParams{DBName: f.db, Mode: "full"})
			gcDone <- err
		}()
		select {
		case err := <-gcDone:
			gcFinishedDuringRebase = true
			gcDone <- err
		case <-time.After(500 * time.Millisecond):
		}
	}
	sess := sessionFromContext(f.op)
	require.NoError(t, sess.CommandBegin())
	err := f.rebaseOntoFeature(context.WithValue(f.op, branchOpHookKey{}, hook))
	sess.CommandEnd()
	require.NoError(t, err)
	require.NoError(t, <-gcDone)
	require.False(t, gcFinishedDuringRebase, "GC finished while the rebase command was in flight")

	logRes, err := f.be.DumboDBLog(f.op, &backends.LogParams{DBName: f.db, Branch: defaultBranch})
	require.NoError(t, err, "a commit the rebase wrote was swept")
	require.Len(t, logRes.Commits, 6, "base, feat, three replayed commits and the database's initial commit")
	ids := f.ids()
	for _, id := range []string{"base", "feat", "m1", "m2", "m3"} {
		require.True(t, ids[id], "missing %q after the rebase and GC: %v", id, ids)
	}
}
