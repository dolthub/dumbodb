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
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/ref"

	"github.com/dolthub/dumbodb/internal/backends"
)

// TestDumboDBPushFetch_DoltHubLive pushes to and fetches from a real DoltHub
// repository over the https gRPC transport. It is gated on
// DUMBO_DOLTHUB_TEST_REPO (an "org/repo" the caller can write to) so it skips
// cleanly in CI for anyone without credentials. The operator must have run
// `dolt login`; this test reads those credentials from the standard location.
//
// The repo host defaults to doltremoteapi.dolthub.com and can be overridden with
// DUMBO_DOLTHUB_TEST_HOST. Each run pushes a uniquely named branch to avoid
// racing concurrent runs and to keep re-runs independent.
func TestDumboDBPushFetch_DoltHubLive(t *testing.T) {
	repo := os.Getenv("DUMBO_DOLTHUB_TEST_REPO")
	if repo == "" {
		t.Skip("DUMBO_DOLTHUB_TEST_REPO not set; skipping live DoltHub integration test")
	}
	host := os.Getenv("DUMBO_DOLTHUB_TEST_HOST")
	if host == "" {
		host = "doltremoteapi.dolthub.com"
	}
	remoteURL := fmt.Sprintf("https://%s/%s", host, repo)

	ctx := context.Background()
	branch := fmt.Sprintf("dumbo-live-%d", time.Now().UnixNano())

	// Producer: seed and commit on main, then branch off at that commit. The
	// test helpers always write to the default branch, so the unique branch is
	// created from main's HEAD and carries that commit. A per-run branch name
	// keeps concurrent runs and re-runs independent on the shared repo.
	src := newTestBackend(t)
	insertDoc(t, src, "srcdb", "col", mustDoc(t, "_id", int64(1), "v", int64(42)))
	want := commitDB(t, src, "srcdb", "seed on main")
	if _, err := src.DumboDBBranch(ctx, &backends.BranchParams{DBName: "srcdb", From: "main", Name: branch}); err != nil {
		t.Fatalf("create branch %s: %v", branch, err)
	}

	if _, err := src.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "srcdb", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	pres, err := src.DumboDBPush(ctx, &backends.PushParams{DBName: "srcdb", Remote: "origin", RefSpec: branch})
	if err != nil {
		t.Fatalf("push to DoltHub %s: %v", remoteURL, err)
	}
	if pres.CommitPushed != want {
		t.Errorf("pushed commit = %s, want %s", pres.CommitPushed, want)
	}
	t.Logf("pushed %s @ %s to %s", branch, want, remoteURL)

	// Consumer: fetch from the same remote, confirm the branch came back at the
	// pushed commit.
	dst := newTestBackend(t)
	insertDoc(t, dst, "dstdb", "col", mustDoc(t, "_id", int64(99)))
	commitDB(t, dst, "dstdb", "seed")
	if _, err := dst.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "dstdb", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote (dst): %v", err)
	}
	fres, err := dst.DumboDBFetch(ctx, &backends.FetchParams{DBName: "dstdb", Remote: "origin"})
	if err != nil {
		t.Fatalf("fetch from DoltHub: %v", err)
	}

	var got string
	for _, br := range fres.Branches {
		if br.Branch == branch {
			got = br.Commit
		}
	}
	if got != want {
		t.Errorf("fetched branch %s = %q, want %s (branches: %+v)", branch, got, want, fres.Branches)
	}
}

// TestDumboDBClone_DoltHubLive clones a real DoltHub repository into a new
// server-side database over the https gRPC transport. Gated on
// DUMBO_DOLTHUB_TEST_REPO; requires `dolt login`. See the push/fetch live test
// above for the env contract.
func TestDumboDBClone_DoltHubLive(t *testing.T) {
	repo := os.Getenv("DUMBO_DOLTHUB_TEST_REPO")
	if repo == "" {
		t.Skip("DUMBO_DOLTHUB_TEST_REPO not set; skipping live DoltHub clone test")
	}
	host := os.Getenv("DUMBO_DOLTHUB_TEST_HOST")
	if host == "" {
		host = "doltremoteapi.dolthub.com"
	}
	remoteURL := fmt.Sprintf("https://%s/%s", host, repo)

	ctx := context.Background()
	b := newTestBackend(t)

	res, err := b.DumboDBClone(ctx, &backends.CloneParams{From: remoteURL, As: "hubclone"})
	if err != nil {
		t.Fatalf("clone from DoltHub %s: %v", remoteURL, err)
	}
	if res.DB != "hubclone" {
		t.Errorf("clone db = %q, want hubclone", res.DB)
	}
	if len(res.Branches) == 0 {
		t.Fatal("clone returned no branches")
	}
	if res.DefaultBranch == "" || res.Commit == "" {
		t.Errorf("clone default branch/commit empty: branch=%q commit=%q", res.DefaultBranch, res.Commit)
	}
	t.Logf("cloned %s: default=%s @ %s, branches=%v", remoteURL, res.DefaultBranch, res.Commit, res.Branches)

	// The cloned database opens and its default branch head resolves.
	st := mustDB(t, b, "hubclone")
	if _, err := st.doltDB.ResolveCommitRef(ctx, ref.NewBranchRef(res.DefaultBranch)); err != nil {
		t.Errorf("resolve cloned default branch %q: %v", res.DefaultBranch, err)
	}
}
