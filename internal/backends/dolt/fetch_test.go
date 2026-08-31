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

	"github.com/dolthub/dumbodb/internal/backends"
)

// TestDumboDBFetch_AllBranches verifies fetch pulls every remote branch into
// tracking refs, without moving local branch heads.
func TestDumboDBFetch_AllBranches(t *testing.T) {
	ctx := context.Background()
	remoteURL := "file://" + t.TempDir()

	// Server A: commit on main, push to the remote.
	a := newTestBackend(t)
	insertDoc(t, a, "mydb", "col", mustDoc(t, "_id", int64(1), "v", "x"))
	c1 := commitDB(t, a, "mydb", "c1")
	if _, err := a.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "mydb", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	if _, err := a.DumboDBPush(ctx, &backends.PushParams{DBName: "mydb", Remote: "origin", Branch: "main", BranchExplicit: true}); err != nil {
		t.Fatalf("push main: %v", err)
	}

	// Give the remote a second branch directly (as another pusher would).
	nbf := mustDB(t, a, "mydb").doltDB.Format()
	remoteDB, err := doltdb.LoadDoltDBWithParams(ctx, nbf, remoteURL, filesys.LocalFS, map[string]interface{}{
		dbfactory.DisableSingletonCacheParam: "true",
	})
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	mainCommit, err := remoteDB.ResolveCommitRef(ctx, ref.NewBranchRef("main"))
	if err != nil {
		t.Fatalf("resolve remote main: %v", err)
	}
	if err := remoteDB.SetHeadToCommit(ctx, ref.NewBranchRef("dev"), mainCommit); err != nil {
		t.Fatalf("create remote dev branch: %v", err)
	}
	_ = remoteDB.Close()

	// Server B: fresh db, add remote, fetch ALL branches.
	b := newTestBackend(t)
	insertDoc(t, b, "mydb", "col", mustDoc(t, "_id", int64(99)))
	bInit := commitDB(t, b, "mydb", "b-init")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "mydb", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("B add remote: %v", err)
	}

	res, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: "mydb", Remote: "origin"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	got := map[string]string{}
	for _, fr := range res.Branches {
		got[fr.Branch] = fr.Commit
	}
	if got["main"] != c1 {
		t.Errorf("fetched main = %q, want c1 %s", got["main"], c1)
	}
	if got["dev"] != c1 {
		t.Errorf("fetched dev = %q, want c1 %s", got["dev"], c1)
	}
	if len(res.Branches) < 2 {
		t.Errorf("fetched %d branches, want >= 2", len(res.Branches))
	}

	// Tracking refs exist for both remote branches.
	st := mustDB(t, b, "mydb")
	for _, brName := range []string{"main", "dev"} {
		cm, err := st.doltDB.ResolveCommitRef(ctx, ref.NewRemoteRef("origin", brName))
		if err != nil {
			t.Fatalf("resolve tracking ref origin/%s: %v", brName, err)
		}
		h, _ := cm.HashOf()
		if h.String() != c1 {
			t.Errorf("refs/remotes/origin/%s = %s, want c1 %s", brName, h.String(), c1)
		}
	}

	// Fetch must not move the local branch head.
	bh, err := st.doltDB.ResolveCommitRef(ctx, ref.NewBranchRef("main"))
	if err != nil {
		t.Fatalf("resolve local main: %v", err)
	}
	lh, _ := bh.HashOf()
	if lh.String() != bInit {
		t.Errorf("local refs/heads/main = %s, want b-init %s (fetch must not move the branch head)", lh.String(), bInit)
	}

	// Idempotent re-fetch.
	if _, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: "mydb", Remote: "origin"}); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
}

func TestDumboDBFetch_RemoteNotFound(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	insertDoc(t, b, "mydb", "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, "mydb", "c1")
	if _, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: "mydb", Remote: "ghost"}); err == nil {
		t.Error("fetch from unknown remote: want error, got nil")
	}
}
