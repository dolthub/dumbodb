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

func assertUpstream(t *testing.T, b *Backend, dbName, branch, wantRemote, wantRef string) {
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
	up, _ := doc.Get("upstream")
	upDoc, ok := up.(*types.Document)
	if !ok {
		t.Fatalf("upstream is not a document: %T", up)
	}
	if rv, _ := upDoc.Get("remote"); rv != wantRemote {
		t.Errorf("upstream.remote = %v, want %q", rv, wantRemote)
	}
	if rf, _ := upDoc.Get("ref"); rf != wantRef {
		t.Errorf("upstream.ref = %v, want %q", rf, wantRef)
	}
}

// TestDumboDBUpstream_GitSemantics mirrors git: naming a branch pushes without
// changing tracking; only setUpstream records it (and overwrites a prior one); a
// bare push follows the upstream; an explicit push never changes it.
func TestDumboDBUpstream_GitSemantics(t *testing.T) {
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

	// (1) Named push does not set an upstream (git push origin main).
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "origin", RefSpec: "main"}); err != nil {
		t.Fatalf("named push: %v", err)
	}
	if doc := readBranchDoc(t, b, dbName, "main"); doc != nil {
		t.Errorf("named push must not record an upstream, got %v", doc)
	}

	// (2) setUpstream records it (git push -u origin main).
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "origin", RefSpec: "main", SetUpstream: true}); err != nil {
		t.Fatalf("push -u origin: %v", err)
	}
	assertUpstream(t, b, dbName, "main", "origin", "main")

	// (3) A bare push (no remote) follows the upstream (git push).
	insertDoc(t, b, dbName, "col", mustDoc(t, "_id", int64(2)))
	commitDB(t, b, dbName, "c2")
	res, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, ConnBranch: "main"})
	if err != nil {
		t.Fatalf("bare push: %v", err)
	}
	if res.Remote != "origin" {
		t.Errorf("bare push remote = %q, want origin", res.Remote)
	}

	// (4) setUpstream to another remote overwrites the upstream (git push -u origin2).
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "other", RefSpec: "main", SetUpstream: true}); err != nil {
		t.Fatalf("push -u other: %v", err)
	}
	assertUpstream(t, b, dbName, "main", "other", "main")

	// (5) An explicit push to origin does not change the upstream (still other).
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "origin", RefSpec: "main"}); err != nil {
		t.Fatalf("explicit push origin: %v", err)
	}
	assertUpstream(t, b, dbName, "main", "other", "main")

	// A bare fetch follows the (default-branch) upstream too.
	fres, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: dbName})
	if err != nil {
		t.Fatalf("bare fetch: %v", err)
	}
	if fres.Remote != "other" {
		t.Errorf("bare fetch remote = %q, want other", fres.Remote)
	}
}

// TestDumboDBUpstream_BarePushErrors covers the git-faithful refusal to push
// with no branch named and no upstream configured.
func TestDumboDBUpstream_BarePushErrors(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	const dbName = "mydb"

	insertDoc(t, b, dbName, "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, dbName, "c1")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: dbName, Action: "add", Name: "origin", URL: "file://" + t.TempDir()}); err != nil {
		t.Fatalf("add origin: %v", err)
	}

	// Remote named but branch not explicit and no upstream -> error (git push origin).
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "origin", ConnBranch: "main"}); err == nil {
		t.Error("bare push to a remote with no upstream: want error, got nil")
	}
	// No remote and no upstream -> error (git push).
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, ConnBranch: "main"}); err == nil {
		t.Error("push with no remote and no upstream: want error, got nil")
	}
	// Fetch with no remote and no upstream -> error.
	if _, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: dbName}); err == nil {
		t.Error("fetch with no remote and no upstream: want error, got nil")
	}
}

// TestDumboDBUpstream_Scoping verifies upstream docs are keyed per database.
func TestDumboDBUpstream_Scoping(t *testing.T) {
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
	// Only dbA sets an upstream.
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: "dbA", Remote: "origin", RefSpec: "main", SetUpstream: true}); err != nil {
		t.Fatalf("push dbA: %v", err)
	}
	assertUpstream(t, b, "dbA", "main", "origin", "main")
	if doc := readBranchDoc(t, b, "dbB", "main"); doc != nil {
		t.Errorf("dbB should have no upstream doc, got %v", doc)
	}
}
