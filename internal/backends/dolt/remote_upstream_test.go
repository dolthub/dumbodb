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

// TestDumboDBUpstream_RecordedAndDefaulted covers the whole tracking lifecycle:
// an explicit push records the upstream, a target-less push/fetch defaults to
// it, and re-pushing to a different remote updates it.
func TestDumboDBUpstream_RecordedAndDefaulted(t *testing.T) {
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

	// Explicit push records the upstream for main.
	res, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "origin", Branch: "main"})
	if err != nil {
		t.Fatalf("push origin: %v", err)
	}
	if res.Remote != "origin" {
		t.Errorf("push result remote = %q, want origin", res.Remote)
	}
	assertUpstream(t, b, dbName, "main", "origin", "main")

	// Push with no target defaults to the recorded upstream (origin).
	res2, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Branch: "main"})
	if err != nil {
		t.Fatalf("push no-target: %v", err)
	}
	if res2.Remote != "origin" {
		t.Errorf("defaulted push remote = %q, want origin", res2.Remote)
	}

	// Fetch with no remote defaults to the upstream too.
	fres, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: dbName})
	if err != nil {
		t.Fatalf("fetch no-remote: %v", err)
	}
	if fres.Remote != "origin" {
		t.Errorf("defaulted fetch remote = %q, want origin", fres.Remote)
	}

	// Re-pushing to a different remote updates the upstream.
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "other", Branch: "main"}); err != nil {
		t.Fatalf("push other: %v", err)
	}
	assertUpstream(t, b, dbName, "main", "other", "main")

	// Now a target-less fetch follows the updated upstream.
	fres2, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: dbName})
	if err != nil {
		t.Fatalf("fetch after update: %v", err)
	}
	if fres2.Remote != "other" {
		t.Errorf("fetch remote after update = %q, want other", fres2.Remote)
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
	// Only dbA pushes.
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: "dbA", Remote: "origin", Branch: "main"}); err != nil {
		t.Fatalf("push dbA: %v", err)
	}
	assertUpstream(t, b, "dbA", "main", "origin", "main")
	if doc := readBranchDoc(t, b, "dbB", "main"); doc != nil {
		t.Errorf("dbB should have no upstream doc, got %v", doc)
	}
}

// TestDumboDBUpstream_MissingErrors covers the clear error when no target is
// given and no upstream is configured.
func TestDumboDBUpstream_MissingErrors(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	const dbName = "mydb"

	insertDoc(t, b, dbName, "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, dbName, "c1")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: dbName, Action: "add", Name: "origin", URL: "file://" + t.TempDir()}); err != nil {
		t.Fatalf("add origin: %v", err)
	}

	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Branch: "main"}); err == nil {
		t.Error("push with no target and no upstream: want error, got nil")
	}
	if _, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: dbName}); err == nil {
		t.Error("fetch with no remote and no upstream: want error, got nil")
	}
}
