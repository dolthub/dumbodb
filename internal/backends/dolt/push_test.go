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
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/dumbodb/internal/backends"
)

// assertRemoteHead opens the file:// remote and checks refs/heads/<branch>.
func assertRemoteHead(t *testing.T, nbf *types.NomsBinFormat, url, branch, wantCommit string) {
	t.Helper()
	ctx := context.Background()

	remoteDB, err := doltdb.LoadDoltDBWithParams(ctx, nbf, url, filesys.LocalFS, map[string]interface{}{
		dbfactory.DisableSingletonCacheParam: "true",
	})
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	defer func() { _ = remoteDB.Close() }()

	cm, err := remoteDB.ResolveCommitRef(ctx, ref.NewBranchRef(branch))
	if err != nil {
		t.Fatalf("resolve remote %s: %v", branch, err)
	}
	h, err := cm.HashOf()
	if err != nil {
		t.Fatalf("hashOf: %v", err)
	}
	if h.String() != wantCommit {
		t.Errorf("remote refs/heads/%s = %s, want %s", branch, h.String(), wantCommit)
	}
}

func TestDumboDBPush_FileRoundTrip(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	const dbName = "mydb"

	insertDoc(t, b, dbName, "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	c1 := commitDB(t, b, dbName, "c1")

	nbf := mustDB(t, b, dbName).doltDB.Format()

	remoteURL := "file://" + t.TempDir()
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: dbName, Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote: %v", err)
	}

	// first push to a brand-new remote
	res, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "origin", RefSpec: "main"})
	if err != nil {
		t.Fatalf("push c1: %v", err)
	}
	if res.CommitPushed != c1 {
		t.Errorf("pushed commit = %s, want c1 %s", res.CommitPushed, c1)
	}
	assertRemoteHead(t, nbf, remoteURL, "main", c1)

	// idempotent re-push: no error, remote unchanged
	res2, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "origin", RefSpec: "main"})
	if err != nil {
		t.Fatalf("re-push: %v", err)
	}
	if !res2.UpToDate {
		t.Logf("re-push UpToDate=false (acceptable if push re-applied the same head)")
	}
	assertRemoteHead(t, nbf, remoteURL, "main", c1)

	// second commit advances the remote head
	insertDoc(t, b, dbName, "col", mustDoc(t, "_id", int64(2), "v", int64(2)))
	c2 := commitDB(t, b, dbName, "c2")
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "origin", RefSpec: "main"}); err != nil {
		t.Fatalf("push c2: %v", err)
	}
	assertRemoteHead(t, nbf, remoteURL, "main", c2)
}

func TestDumboDBPush_RemoteNotFound(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	insertDoc(t, b, "mydb", "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, "mydb", "c1")

	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: "mydb", Remote: "ghost", RefSpec: "main"}); err == nil {
		t.Error("push to unknown remote: want error, got nil")
	}
}

func TestDumboDBPush_UnsupportedScheme(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	const dbName = "mydb"
	insertDoc(t, b, dbName, "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, dbName, "c1")

	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: dbName, Action: "add", Name: "box", URL: "ssh://host/org/repo"}); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: dbName, Remote: "box", RefSpec: "main"}); err == nil {
		t.Error("push to unsupported scheme: want error, got nil")
	}
}

// TestDumboDBPush_NewBranchAtExistingCommit covers the case where a branch's tip
// commit is already on the remote (its chunks are present). Push must still
// create the remote branch ref even though no chunks transfer.
func TestDumboDBPush_NewBranchAtExistingCommit(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	remoteURL := "file://" + t.TempDir()

	insertDoc(t, b, "mydb", "col", mustDoc(t, "_id", int64(1)))
	c1 := commitDB(t, b, "mydb", "c1")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "mydb", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: "mydb", Remote: "origin", RefSpec: "main"}); err != nil {
		t.Fatalf("push main: %v", err)
	}

	// New local branch at the same commit; its chunks are already on the remote.
	if _, err := b.DumboDBBranch(ctx, &backends.BranchParams{Action: "add", DBName: "mydb", From: "main", Name: "dev"}); err != nil {
		t.Fatalf("create dev: %v", err)
	}
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: "mydb", Remote: "origin", RefSpec: "dev"}); err != nil {
		t.Fatalf("push dev: %v", err)
	}

	// The remote must now have refs/heads/dev at c1.
	nbf := mustDB(t, b, "mydb").doltDB.Format()
	remoteDB, err := doltdb.LoadDoltDBWithParams(ctx, nbf, remoteURL, filesys.LocalFS, map[string]interface{}{
		dbfactory.DisableSingletonCacheParam: "true",
	})
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	defer func() { _ = remoteDB.Close() }()

	cm, err := remoteDB.ResolveCommitRef(ctx, ref.NewBranchRef("dev"))
	if err != nil {
		t.Fatalf("remote refs/heads/dev not created: %v", err)
	}
	h, _ := cm.HashOf()
	if h.String() != c1 {
		t.Errorf("remote refs/heads/dev = %s, want c1 %s", h.String(), c1)
	}
}
