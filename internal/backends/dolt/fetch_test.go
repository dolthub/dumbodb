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

	"github.com/dolthub/dolt/go/libraries/doltcore/ref"

	"github.com/dolthub/dumbodb/internal/backends"
)

// TestDumboDBFetch_FileRoundTrip pushes from server A to a file:// remote, then
// fetches into a fresh server B and verifies B's tracking ref and that the
// commit's chunks were pulled.
func TestDumboDBFetch_FileRoundTrip(t *testing.T) {
	ctx := context.Background()
	remoteURL := "file://" + t.TempDir()

	// Server A: commit + push.
	a := newTestBackend(t)
	insertDoc(t, a, "mydb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	c1 := commitDB(t, a, "mydb", "c1")
	if _, err := a.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "mydb", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("A add remote: %v", err)
	}
	if _, err := a.DumboDBPush(ctx, &backends.PushParams{DBName: "mydb", Remote: "origin", Branch: "main"}); err != nil {
		t.Fatalf("A push: %v", err)
	}

	// Server B: fresh db with its own initial commit, add same remote, fetch.
	b := newTestBackend(t)
	insertDoc(t, b, "mydb", "col", mustDoc(t, "_id", int64(99)))
	bInit := commitDB(t, b, "mydb", "b-init")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "mydb", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("B add remote: %v", err)
	}

	res, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: "mydb", Remote: "origin", Branch: "main"})
	if err != nil {
		t.Fatalf("B fetch: %v", err)
	}
	if res.Commit != c1 {
		t.Errorf("fetched commit = %s, want c1 %s", res.Commit, c1)
	}

	st := mustDB(t, b, "mydb")

	// Tracking ref refs/remotes/origin/main resolves to c1 (chunks pulled).
	cm, err := st.doltDB.ResolveCommitRef(ctx, ref.NewRemoteRef("origin", "main"))
	if err != nil {
		t.Fatalf("resolve tracking ref: %v", err)
	}
	h, _ := cm.HashOf()
	if h.String() != c1 {
		t.Errorf("refs/remotes/origin/main = %s, want c1 %s", h.String(), c1)
	}

	// Fetch does not move the local branch head.
	bh, err := st.doltDB.ResolveCommitRef(ctx, ref.NewBranchRef("main"))
	if err != nil {
		t.Fatalf("resolve local main: %v", err)
	}
	lh, _ := bh.HashOf()
	if lh.String() != bInit {
		t.Errorf("local refs/heads/main = %s, want b-init %s (fetch must not move the branch head)", lh.String(), bInit)
	}

	// Idempotent re-fetch.
	if _, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: "mydb", Remote: "origin", Branch: "main"}); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
}

func TestDumboDBFetch_RemoteNotFound(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	insertDoc(t, b, "mydb", "col", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, "mydb", "c1")
	if _, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: "mydb", Remote: "ghost", Branch: "main"}); err == nil {
		t.Error("fetch from unknown remote: want error, got nil")
	}
}
