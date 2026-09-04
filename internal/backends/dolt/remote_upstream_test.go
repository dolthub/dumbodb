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
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// readBranchDoc returns the raw admin.system.branches document for a branch, or
// nil if none, so tests can assert the stored _id/scoping directly.
func readBranchDoc(t *testing.T, b *Backend, dbName, branch string) *types.Document {
	t.Helper()
	adminDB, err := b.Database("admin")
	if err != nil {
		t.Fatalf("admin db: %v", err)
	}
	coll, err := adminDB.Collection(branchesCollection)
	if err != nil {
		t.Fatalf("branches coll: %v", err)
	}
	id := branchID(dbName, branch)
	qr, err := coll.Query(context.Background(), &backends.QueryParams{
		Filter: must.NotFail(types.NewDocument("_id", id)),
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer qr.Iter.Close()
	for {
		_, doc, err := qr.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			t.Fatalf("iter: %v", err)
		}
		if idv, _ := doc.Get("_id"); idv == id {
			return doc
		}
	}
	return nil
}

// assertPullUpstream checks the stored config.pull.{remote,branch} for a branch.
func assertPullUpstream(t *testing.T, b *Backend, dbName, branch, wantRemote, wantBranch string) {
	t.Helper()
	doc := readBranchDoc(t, b, dbName, branch)
	if doc == nil {
		t.Fatalf("no system.branches doc for %s.%s", dbName, branch)
	}
	if idv, _ := doc.Get("_id"); idv != branchID(dbName, branch) {
		t.Errorf("_id = %v, want %q", idv, branchID(dbName, branch))
	}
	if dbv, _ := doc.Get("db"); dbv != dbName {
		t.Errorf("db = %v, want %q", dbv, dbName)
	}
	if bv, _ := doc.Get("branch"); bv != branch {
		t.Errorf("branch = %v, want %q", bv, branch)
	}
	cfgv, _ := doc.Get("config")
	cfgDoc, ok := cfgv.(*types.Document)
	if !ok {
		t.Fatalf("config is not a document: %T", cfgv)
	}
	pullv, _ := cfgDoc.Get("pull")
	pullDoc, ok := pullv.(*types.Document)
	if !ok {
		t.Fatalf("config.pull is not a document: %T", pullv)
	}
	if rv, _ := pullDoc.Get("remote"); rv != wantRemote {
		t.Errorf("config.pull.remote = %v, want %q", rv, wantRemote)
	}
	if rf, _ := pullDoc.Get("branch"); rf != wantBranch {
		t.Errorf("config.pull.branch = %v, want %q", rf, wantBranch)
	}
}

// strp returns a pointer to a string literal, for BranchConfigUpdate fields.
func strp(s string) *string { return &s }

// TestDumboDBConfig_PullPush exercises the direction-grouped config model: a bare
// push follows config.push when set, else config.pull; an explicit push never
// mutates the stored config (there is no push -u).
func TestDumboDBConfig_PullPush(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	const dbName = "mydb"

	originURL := "file://" + t.TempDir()
	otherURL := "file://" + t.TempDir()

	insertDoc(t, b, dbName, "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, dbName, "c1")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: dbName, Action: "add", Name: "origin", URL: originURL}); err != nil {
		t.Fatalf("add origin: %v", err)
	}
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: dbName, Action: "add", Name: "other", URL: otherURL}); err != nil {
		t.Fatalf("add other: %v", err)
	}

	// (1) A named push does not record any config (there is no push -u).
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "origin", RefSpec: "main"}); err != nil {
		t.Fatalf("named push: %v", err)
	}
	if doc := readBranchDoc(t, b, dbName, "main"); doc != nil {
		t.Errorf("named push must not record config, got %v", doc)
	}

	// (2) config.pull is set explicitly via setConfig; a bare push follows it.
	if _, err := b.applyBranchConfig(ctx, dbName, "main", &backends.BranchConfigUpdate{
		PullRemote: strp("origin"), PullBranch: strp("main"),
	}); err != nil {
		t.Fatalf("set config.pull: %v", err)
	}
	assertPullUpstream(t, b, dbName, "main", "origin", "main")

	insertDoc(t, b, dbName, "col", mustDoc(t, "_id", int64(2)))
	commitDB(t, b, dbName, "c2")
	res, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, ConnBranch: "main"})
	if err != nil {
		t.Fatalf("bare push (config.pull): %v", err)
	}
	if res.Remote != "origin" || res.RemoteBranch != "main" {
		t.Errorf("bare push = %s/%s, want origin/main", res.Remote, res.RemoteBranch)
	}

	// (3) config.push overrides config.pull for a bare push (triangular target).
	if _, err := b.applyBranchConfig(ctx, dbName, "main", &backends.BranchConfigUpdate{
		PushRemote: strp("other"), PushBranch: strp("review"),
	}); err != nil {
		t.Fatalf("set config.push: %v", err)
	}
	res, err = b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, ConnBranch: "main"})
	if err != nil {
		t.Fatalf("bare push (config.push): %v", err)
	}
	if res.Remote != "other" || res.RemoteBranch != "review" {
		t.Errorf("bare push = %s/%s, want other/review", res.Remote, res.RemoteBranch)
	}

	// (4) An explicit push to origin does not change the stored config.
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "origin", RefSpec: "main"}); err != nil {
		t.Fatalf("explicit push origin: %v", err)
	}
	assertPullUpstream(t, b, dbName, "main", "origin", "main")
	push, err := b.getBranchPush(ctx, dbName, "main")
	if err != nil {
		t.Fatalf("get config.push: %v", err)
	}
	if push.remote != "other" || push.branch != "review" {
		t.Errorf("config.push = %s/%s, want other/review", push.remote, push.branch)
	}

	// A bare fetch follows the default branch's config.pull remote.
	fres, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: dbName})
	if err != nil {
		t.Fatalf("bare fetch: %v", err)
	}
	if fres.Remote != "origin" {
		t.Errorf("bare fetch remote = %q, want origin", fres.Remote)
	}
}

// TestDumboDBConfig_BarePushErrors covers the cases where a bare push cannot
// resolve a target: no config and no explicit remote errors; a fetch with no
// upstream errors. An explicit remote with no config pushes to the same-named
// branch (the git simple-mode name refusal is dropped).
func TestDumboDBConfig_BarePushErrors(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	const dbName = "mydb"

	insertDoc(t, b, dbName, "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, dbName, "c1")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: dbName, Action: "add", Name: "origin", URL: "file://" + t.TempDir()}); err != nil {
		t.Fatalf("add origin: %v", err)
	}

	// Explicit remote, no refspec, no config: pushes current branch to the
	// same-named branch on that remote (no config recorded).
	res, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "origin", ConnBranch: "main"})
	if err != nil {
		t.Fatalf("explicit-remote bare push: %v", err)
	}
	if res.RemoteBranch != "main" {
		t.Errorf("remoteBranch = %q, want main", res.RemoteBranch)
	}
	if doc := readBranchDoc(t, b, dbName, "main"); doc != nil {
		t.Errorf("push must not record config, got %v", doc)
	}

	// No remote and no config -> error (git push).
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, ConnBranch: "main"}); err == nil {
		t.Error("push with no remote and no config: want error, got nil")
	}
	// Fetch with no remote and no config -> error.
	if _, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: dbName}); err == nil {
		t.Error("fetch with no remote and no config: want error, got nil")
	}
}

// TestDumboDBConfig_Scoping verifies config docs are keyed per database.
func TestDumboDBConfig_Scoping(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	originURL := "file://" + t.TempDir()

	for _, db := range []string{"dbA", "dbB"} {
		insertDoc(t, b, db, "col", mustDoc(t, "_id", int64(1)))
		commitDB(t, b, db, "c1")
		if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: db, Action: "add", Name: "origin", URL: originURL}); err != nil {
			t.Fatalf("add remote %s: %v", db, err)
		}
	}
	// Only dbA sets config.pull.
	if _, err := b.applyBranchConfig(ctx, "dbA", "main", &backends.BranchConfigUpdate{
		PullRemote: strp("origin"), PullBranch: strp("main"),
	}); err != nil {
		t.Fatalf("set config dbA: %v", err)
	}
	assertPullUpstream(t, b, "dbA", "main", "origin", "main")
	if doc := readBranchDoc(t, b, "dbB", "main"); doc != nil {
		t.Errorf("dbB should have no config doc, got %v", doc)
	}
}
