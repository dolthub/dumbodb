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

// TestDumboDBConfig_PullPush exercises the direction-grouped config model.
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

	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "origin", RefSpec: "main"}); err != nil {
		t.Fatalf("named push: %v", err)
	}
	if doc := readBranchDoc(t, b, dbName, "main"); doc != nil {
		t.Errorf("named push must not record config, got %v", doc)
	}

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

	fres, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: dbName})
	if err != nil {
		t.Fatalf("bare fetch: %v", err)
	}
	if fres.Remote != "origin" {
		t.Errorf("bare fetch remote = %q, want origin", fres.Remote)
	}
}

// TestDumboDBConfig_BarePushErrors covers the cases where a bare push cannot resolve a target.
func TestDumboDBConfig_BarePushErrors(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	const dbName = "mydb"

	insertDoc(t, b, dbName, "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, dbName, "c1")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: dbName, Action: "add", Name: "origin", URL: "file://" + t.TempDir()}); err != nil {
		t.Fatalf("add origin: %v", err)
	}

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

	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, ConnBranch: "main"}); err == nil {
		t.Error("push with no remote and no config: want error, got nil")
	}
	if _, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: dbName}); err == nil {
		t.Error("fetch with no remote and no config: want error, got nil")
	}
}

// TestDumboDBConfig_DeleteClearsConfig verifies that deleting a branch drops its stored config.
func TestDumboDBConfig_DeleteClearsConfig(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	const dbName = "mydb"

	insertDoc(t, b, dbName, "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, dbName, "c1")
	for _, r := range []struct{ name, url string }{
		{"origin", "file://" + t.TempDir()},
		{"origin2", "file://" + t.TempDir()},
	} {
		if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: dbName, Action: "add", Name: r.name, URL: r.url}); err != nil {
			t.Fatalf("add remote %s: %v", r.name, err)
		}
	}

	branch := func(p *backends.BranchParams) {
		t.Helper()
		if _, err := b.DumboDBBranch(ctx, p); err != nil {
			t.Fatalf("DumboDBBranch %+v: %v", p, err)
		}
	}

	branch(&backends.BranchParams{Action: "add", DBName: dbName, From: "main", Name: "release"})
	if _, err := b.applyBranchConfig(ctx, dbName, "release", &backends.BranchConfigUpdate{
		PullRemote: strp("origin"), PullBranch: strp("main"),
		PushRemote: strp("origin2"), PushBranch: strp("release"),
	}); err != nil {
		t.Fatalf("set config: %v", err)
	}
	if _, ok, err := b.readBranchConfig(ctx, dbName, "release"); err != nil || !ok {
		t.Fatalf("config must be present before delete (ok=%v, err=%v)", ok, err)
	}

	branch(&backends.BranchParams{Action: "remove", DBName: dbName, From: "main", Name: "release", Force: true})
	if _, ok, err := b.readBranchConfig(ctx, dbName, "release"); err != nil || ok {
		t.Fatalf("config must be cleared after delete (ok=%v, err=%v)", ok, err)
	}

	branch(&backends.BranchParams{Action: "add", DBName: dbName, From: "main", Name: "release"})
	if _, ok, err := b.readBranchConfig(ctx, dbName, "release"); err != nil || ok {
		t.Fatalf("recreated branch must have no config (ok=%v, err=%v)", ok, err)
	}

	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, ConnBranch: "release"}); err == nil {
		t.Error("bare push on a recreated branch with no config: want error, got nil")
	}
}

// TestDumboDBBranchAddWithConfig covers action "add" with setConfig: a valid
// config is applied to the new branch, and an invalid config rolls the branch
// back (add is atomic).
func TestDumboDBBranchAddWithConfig(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	const dbName = "mydb"

	insertDoc(t, b, dbName, "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, dbName, "c1")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: dbName, Action: "add", Name: "origin", URL: "file://" + t.TempDir()}); err != nil {
		t.Fatalf("add origin: %v", err)
	}

	if _, err := b.DumboDBBranch(ctx, &backends.BranchParams{
		Action: "add", DBName: dbName, From: "main", Name: "feature",
		ConfigUpdate: &backends.BranchConfigUpdate{PullRemote: strp("origin"), PullBranch: strp("main")},
	}); err != nil {
		t.Fatalf("add with config: %v", err)
	}
	assertPullUpstream(t, b, dbName, "feature", "origin", "main")

	rebase := "true"
	if _, err := b.DumboDBBranch(ctx, &backends.BranchParams{
		Action: "add", DBName: dbName, From: "main", Name: "bad",
		ConfigUpdate: &backends.BranchConfigUpdate{PullRebase: &rebase},
	}); err == nil {
		t.Fatal("add with invalid config (rebase, no upstream): want error, got nil")
	}
	if _, ok, err := b.readBranchConfig(ctx, dbName, "bad"); err != nil || ok {
		t.Fatalf("rolled-back add must leave no config (ok=%v, err=%v)", ok, err)
	}
	if _, err := b.DumboDBBranch(ctx, &backends.BranchParams{
		Action: "add", DBName: dbName, From: "main", Name: "bad",
	}); err != nil {
		t.Fatalf("re-add after rolled-back config error must succeed: %v", err)
	}
}

// TestDumboDBBranchCannotDeleteMain verifies the default branch can never be
// removed, even from another connection.
func TestDumboDBBranchCannotDeleteMain(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	const dbName = "mydb"

	insertDoc(t, b, dbName, "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, dbName, "c1")
	if _, err := b.DumboDBBranch(ctx, &backends.BranchParams{Action: "add", DBName: dbName, From: "main", Name: "feature"}); err != nil {
		t.Fatalf("add feature: %v", err)
	}

	if _, err := b.DumboDBBranch(ctx, &backends.BranchParams{Action: "remove", DBName: dbName, From: "feature", Name: "main", Force: true}); err == nil {
		t.Fatal("removing main: want error, got nil")
	}

	if _, ok, err := b.readBranchConfig(ctx, dbName, "main"); err != nil {
		t.Fatalf("main config read after failed delete: %v", err)
	} else {
		_ = ok
	}
	// main must still be listable.
	res, err := b.DumboDBBranch(ctx, &backends.BranchParams{Action: "list", DBName: dbName, From: "feature"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, br := range res.Branches {
		if br.Name == "main" {
			found = true
		}
	}
	if !found {
		t.Error("main must still exist after a rejected delete")
	}
}

// TestRemoteSyncOnMissingDB verifies push, fetch, and pull return a clean error,
// not a nil-pointer panic, when the database does not exist.
func TestRemoteSyncOnMissingDB(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	const ghost = "ghost" // never created

	// A remote can be registered without the database existing (see below); with
	// it configured, fetch still must not panic on the missing database.
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: ghost, Action: "add", Name: "origin", URL: "file://" + t.TempDir()}); err != nil {
		t.Fatalf("add remote: %v", err)
	}

	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: ghost, Remote: "origin", ConnBranch: "main", RefSpec: "main"}); err == nil {
		t.Error("push on a missing database: want error, got nil")
	}
	if _, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: ghost, Remote: "origin"}); err == nil {
		t.Error("fetch on a missing database: want error, got nil")
	}
	if _, err := b.DumboDBPull(ctx, &backends.PullParams{DBName: ghost, Branch: "main", Remote: "origin"}); err == nil {
		t.Error("pull on a missing database: want error, got nil")
	}
}

// TestRemoteAddOnMissingDB verifies a remote may be registered on a database that
// does not exist yet (remotes to a not-yet-existing peer are allowed, including
// cyclical setups).
func TestRemoteAddOnMissingDB(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	const peer = "peer" // never created

	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: peer, Action: "add", Name: "origin", URL: "file://" + t.TempDir()}); err != nil {
		t.Fatalf("remote add on a missing database must be allowed: %v", err)
	}
	list, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: peer, Action: "list"})
	if err != nil {
		t.Fatalf("remote list on a missing database: %v", err)
	}
	if len(list.Remotes) != 1 || list.Remotes[0].Name != "origin" {
		t.Fatalf("remote must be listed for a not-yet-existing database, got %+v", list.Remotes)
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
