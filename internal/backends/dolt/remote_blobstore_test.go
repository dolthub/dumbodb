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

	"github.com/dolthub/dolt/go/libraries/doltcore/ref"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// TestDumboDBBlobstore_LocalBSRoundTrip exercises the blobstore-backed remote
// code path (the same NoConjoin/Blobstore machinery s3:// uses) hermetically via
// the localbs:// scheme: push to a local filesystem blobstore, then clone it and
// fetch from it, asserting commits and data survive the round trip.
func TestDumboDBBlobstore_LocalBSRoundTrip(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	// A localbs:// URL addresses a local directory as a blobstore. Three slashes
	// keeps the host empty so the whole path is the directory.
	remoteURL := "localbs://" + t.TempDir()

	insertDoc(t, b, "src", "coll", mustDoc(t, "_id", int64(1), "v", "blob-hello"))
	c1 := commitDB(t, b, "src", "c1")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "src", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote: %v", err)
	}

	res, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: "src", Remote: "origin", Branch: "main", BranchExplicit: true})
	if err != nil {
		t.Fatalf("push to localbs: %v", err)
	}
	if res.Commit != c1 {
		t.Errorf("pushed commit = %s, want c1 %s", res.Commit, c1)
	}

	// A second commit advances the blobstore remote head.
	insertDoc(t, b, "src", "coll", mustDoc(t, "_id", int64(2), "v", "blob-two"))
	c2 := commitDB(t, b, "src", "c2")
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: "src", Remote: "origin", Branch: "main", BranchExplicit: true}); err != nil {
		t.Fatalf("push c2: %v", err)
	}

	// Clone the blobstore remote into a fresh database and read the data back.
	cres, err := b.DumboDBClone(ctx, &backends.CloneParams{From: remoteURL, As: "clonedb"})
	if err != nil {
		t.Fatalf("clone from localbs: %v", err)
	}
	if cres.Commit != c2 {
		t.Errorf("clone default commit = %s, want c2 %s", cres.Commit, c2)
	}

	st := mustDB(t, b, "clonedb")
	cm, err := st.doltDB.ResolveCommitRef(ctx, ref.NewBranchRef("main"))
	if err != nil {
		t.Fatalf("resolve clonedb main: %v", err)
	}
	h, _ := cm.HashOf()
	if h.String() != c2 {
		t.Errorf("clonedb main = %s, want c2 %s", h.String(), c2)
	}

	assertDocValue(t, ctx, b, "clonedb", "coll", int64(2), "blob-two")

	// Fetch into a third database and confirm the remote-tracking ref for main.
	insertDoc(t, b, "dst", "coll", mustDoc(t, "_id", int64(99)))
	commitDB(t, b, "dst", "seed")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "dst", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote (dst): %v", err)
	}
	fres, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: "dst", Remote: "origin"})
	if err != nil {
		t.Fatalf("fetch from localbs: %v", err)
	}
	var mainCommit string
	for _, br := range fres.Branches {
		if br.Branch == "main" {
			mainCommit = br.Commit
		}
	}
	if mainCommit != c2 {
		t.Errorf("fetched main = %q, want c2 %s", mainCommit, c2)
	}
}

// assertDocValue fails unless collection coll in dbName has a document with the
// given _id and string value v.
func assertDocValue(t *testing.T, ctx context.Context, b *Backend, dbName, coll string, id int64, v string) {
	t.Helper()
	adb, err := b.Database(dbName)
	if err != nil {
		t.Fatalf("open %s: %v", dbName, err)
	}
	c, err := adb.Collection(coll)
	if err != nil {
		t.Fatalf("open collection %s: %v", coll, err)
	}
	qr, err := c.Query(ctx, nil)
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
		if gotID, _ := doc.Get("_id"); gotID == id {
			if gotV, _ := doc.Get("v"); gotV != v {
				t.Errorf("doc _id:%d v = %v, want %q", id, gotV, v)
			}
			return
		}
	}
	t.Errorf("doc _id:%d not found in %s.%s", id, dbName, coll)
}
