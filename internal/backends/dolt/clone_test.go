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
	"path/filepath"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/ref"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestDumboDBClone_FromFileRemote(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	remoteURL := "file://" + t.TempDir()

	// Source db: insert, commit, push to the file:// remote.
	insertDoc(t, b, "src", "coll", mustDoc(t, "_id", int64(1), "v", "hello"))
	c1 := commitDB(t, b, "src", "c1")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "src", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: "src", Remote: "origin", RefSpec: "main"}); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Clone the remote into a brand-new database.
	res, err := b.DumboDBClone(ctx, &backends.CloneParams{From: remoteURL, As: "clonedb"})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if res.DB != "clonedb" {
		t.Errorf("clone db = %s, want clonedb", res.DB)
	}

	// Cloned main head resolves to c1.
	st := mustDB(t, b, "clonedb")
	cm, err := st.doltDB.ResolveCommitRef(ctx, ref.NewBranchRef("main"))
	if err != nil {
		t.Fatalf("resolve clonedb main: %v", err)
	}
	h, _ := cm.HashOf()
	if h.String() != c1 {
		t.Errorf("clonedb refs/heads/main = %s, want c1 %s", h.String(), c1)
	}

	// The cloned database is readable: the document is present with its value.
	adb, err := b.Database("clonedb")
	if err != nil {
		t.Fatalf("open clonedb: %v", err)
	}
	coll, err := adb.Collection("coll")
	if err != nil {
		t.Fatalf("open collection: %v", err)
	}
	qr, err := coll.Query(ctx, nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer qr.Iter.Close()

	found := false
	for {
		_, doc, err := qr.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			t.Fatalf("iter: %v", err)
		}
		if id, _ := doc.Get("_id"); id == int64(1) {
			found = true
			if v, _ := doc.Get("v"); v != "hello" {
				t.Errorf("cloned doc v = %v, want \"hello\"", v)
			}
		}
	}
	if !found {
		t.Error("cloned document _id:1 not found in clonedb (clone did not materialize data)")
	}

	// Clone into an existing db is refused.
	if _, err := b.DumboDBClone(ctx, &backends.CloneParams{From: remoteURL, As: "clonedb"}); err == nil {
		t.Error("clone into existing db: want error, got nil")
	}

	// Unsupported scheme is refused (ssh is known but not implemented).
	if _, err := b.DumboDBClone(ctx, &backends.CloneParams{From: "ssh://host/o/r", As: "c2"}); err == nil {
		t.Error("clone ssh: want error, got nil")
	}

	// Reserved db name is refused.
	if _, err := b.DumboDBClone(ctx, &backends.CloneParams{From: remoteURL, As: "admin"}); err == nil {
		t.Error("clone into admin: want error, got nil")
	}
}

// One of this server's backup URLs may be cloned, but the clone is a plain
// database: no origin remote, tracking refs or upstream, since a remote there
// would be destroyed by the next sync.
func TestDumboDBClone_FromBackupURL(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	backupURL := "file://" + filepath.Join(t.TempDir(), "bk")

	insertDoc(t, b, "src", "coll", mustDoc(t, "_id", int64(1), "v", "backed up"))
	c1 := commitDB(t, b, "src", "c1")
	if _, err := b.DumboDBBackup(ctx, backupParams("src", "add", "nightly", backupURL)); err != nil {
		t.Fatalf("backup add: %v", err)
	}
	if _, err := b.DumboDBBackup(ctx, backupParams("src", "sync", "nightly", "")); err != nil {
		t.Fatalf("backup sync: %v", err)
	}

	res, err := b.DumboDBClone(ctx, &backends.CloneParams{From: backupURL, As: "frombk"})
	if err != nil {
		t.Fatalf("clone from backup url: %v", err)
	}
	if res.DB != "frombk" {
		t.Errorf("clone db = %s, want frombk", res.DB)
	}

	remotes, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "frombk", Action: "list"})
	if err != nil {
		t.Fatalf("remote list on backup clone: %v", err)
	}
	if len(remotes.Remotes) != 0 {
		t.Errorf("backup clone registered remotes: %+v", remotes.Remotes)
	}

	pull, err := b.getBranchPull(ctx, "frombk", defaultBranch)
	if err != nil {
		t.Fatalf("branch pull config: %v", err)
	}
	if !pull.empty() {
		t.Errorf("backup clone configured upstream: %+v", pull)
	}

	st := mustDB(t, b, "frombk")
	h, _ := st.doltDB.ResolveCommitRef(ctx, ref.NewBranchRef(defaultBranch))
	if hh, _ := h.HashOf(); hh.String() != c1 {
		t.Errorf("frombk refs/heads/main = %s, want c1 %s", hh.String(), c1)
	}
	remoteRefs, err := st.doltDB.GetRemoteRefs(ctx)
	if err != nil {
		t.Fatalf("remote refs: %v", err)
	}
	if len(remoteRefs) != 0 {
		t.Errorf("backup clone wrote tracking refs: %v", remoteRefs)
	}

	adb, err := b.Database("frombk")
	if err != nil {
		t.Fatalf("open frombk: %v", err)
	}
	coll, err := adb.Collection("coll")
	if err != nil {
		t.Fatalf("open collection: %v", err)
	}
	qr, err := coll.Query(ctx, nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer qr.Iter.Close()
	found := false
	for {
		_, doc, err := qr.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			t.Fatalf("iter: %v", err)
		}
		if id, _ := doc.Get("_id"); id == int64(1) {
			if v, _ := doc.Get("v"); v != "backed up" {
				t.Errorf("cloned doc v = %v, want \"backed up\"", v)
			}
			found = true
		}
	}
	if !found {
		t.Error("cloned document _id:1 not found in frombk (clone did not materialize data)")
	}
}

// A backup store this server no longer lists (as one written by another
// server) clones like any remote. Only branch heads come across: the backup's
// uncommitted working-set changes do not.
func TestDumboDBClone_FromUnregisteredBackupStore(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	backupURL := "file://" + filepath.Join(t.TempDir(), "bk")

	insertDoc(t, b, "src", "coll", mustDoc(t, "_id", int64(1), "v", "committed"))
	c1 := commitDB(t, b, "src", "c1")
	insertDoc(t, b, "src", "coll", mustDoc(t, "_id", int64(2), "v", "uncommitted"))
	for _, action := range []string{"add", "sync", "remove"} {
		url := ""
		if action == "add" {
			url = backupURL
		}
		if _, err := b.DumboDBBackup(ctx, backupParams("src", action, "nightly", url)); err != nil {
			t.Fatalf("backup %s: %v", action, err)
		}
	}

	if _, err := b.DumboDBBackup(ctx, restoreParams(backupURL, "restored", false)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if n := countDocs(t, b, "restored", "coll"); n != 2 {
		t.Fatalf("restored docs = %d, want 2: the backup must hold the uncommitted doc", n)
	}

	if _, err := b.DumboDBClone(ctx, &backends.CloneParams{From: backupURL, As: "frombk"}); err != nil {
		t.Fatalf("clone from backup store: %v", err)
	}

	remotes, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "frombk", Action: "list"})
	if err != nil {
		t.Fatalf("remote list: %v", err)
	}
	if len(remotes.Remotes) != 1 || remotes.Remotes[0].Name != "origin" || remotes.Remotes[0].URL != backupURL {
		t.Errorf("remotes = %+v, want origin -> %s", remotes.Remotes, backupURL)
	}
	pull, err := b.getBranchPull(ctx, "frombk", defaultBranch)
	if err != nil {
		t.Fatalf("branch pull config: %v", err)
	}
	if pull.remote != "origin" || pull.branch != defaultBranch {
		t.Errorf("main upstream = %+v, want origin/%s", pull, defaultBranch)
	}

	st := mustDB(t, b, "frombk")
	h, _ := st.doltDB.ResolveCommitRef(ctx, ref.NewBranchRef(defaultBranch))
	if hh, _ := h.HashOf(); hh.String() != c1 {
		t.Errorf("frombk refs/heads/main = %s, want c1 %s", hh.String(), c1)
	}
	if remoteRefs, err := st.doltDB.GetRemoteRefs(ctx); err != nil || len(remoteRefs) == 0 {
		t.Errorf("clone with origin: tracking refs = %v, err %v; want refs/remotes/origin/*", remoteRefs, err)
	}

	coll, err := must.NotFail(b.Database("frombk")).Collection("coll")
	if err != nil {
		t.Fatalf("open collection: %v", err)
	}
	ids := idsOfInt64Docs(t, drainQuery(t, ctx, coll, &backends.QueryParams{}))
	if len(ids) != 1 || ids[0] != 1 {
		t.Errorf("frombk docs = %v, want only the committed _id 1", ids)
	}
}

func idsOfInt64Docs(t *testing.T, docs []*types.Document) []int64 {
	t.Helper()
	ids := make([]int64, 0, len(docs))
	for _, d := range docs {
		id, _ := d.Get("_id")
		n, ok := id.(int64)
		if !ok {
			t.Fatalf("_id %v is %T, want int64", id, id)
		}
		ids = append(ids, n)
	}
	return ids
}
