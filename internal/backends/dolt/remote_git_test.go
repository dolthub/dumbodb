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
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/ref"

	"github.com/dolthub/dumbodb/internal/backends"
)

// seedGitRemote creates a bare git repo at path and seeds it with one plain
// commit on the given branch. Dolt git remotes require at least one branch to
// exist before the first push, so this stands in for a provisioned git host.
func seedGitRemote(t *testing.T, bareRepo, branch string) {
	t.Helper()
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=dumbo", "GIT_AUTHOR_EMAIL=dumbo@test",
			"GIT_COMMITTER_NAME=dumbo", "GIT_COMMITTER_EMAIL=dumbo@test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	if err := exec.Command("git", "init", "--bare", bareRepo).Run(); err != nil {
		t.Fatalf("git init --bare: %v", err)
	}
	seed := t.TempDir()
	runGit(seed, "init")
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed README: %v", err)
	}
	runGit(seed, "add", "README")
	runGit(seed, "commit", "-m", "seed")
	runGit(seed, "branch", "-M", branch)
	runGit(seed, "push", bareRepo, branch)
}

// TestDumboDBGit_SSHTransportWiring exercises the git+ssh code path (URL
// handling + GIT_SSH_COMMAND propagation into the git the factory shells out to)
// without a real sshd, by installing a GIT_SSH_COMMAND wrapper that runs the
// transport command locally against the bare repo. The bats suite covers git+ssh
// against a real OpenSSH daemon in CI; this gives fast, hermetic coverage of the
// dumbo-side wiring everywhere.
func TestDumboDBGit_SSHTransportWiring(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH; skipping git+ssh wiring test")
	}
	ctx := context.Background()

	bareRepo := filepath.Join(t.TempDir(), "remote.git")
	seedGitRemote(t, bareRepo, "main")

	// A GIT_SSH_COMMAND that ignores the host and evaluates git's transport
	// command locally, so git+ssh reaches the bare repo with no daemon.
	wrapper := filepath.Join(t.TempDir(), "ssh-wrapper.sh")
	if err := os.WriteFile(wrapper, []byte("#!/usr/bin/env bash\neval \"${@: -1}\"\n"), 0o755); err != nil {
		t.Fatalf("write ssh wrapper: %v", err)
	}
	t.Setenv("GIT_SSH_COMMAND", wrapper)

	// git+ssh URL with an absolute path to the bare repo; the wrapper serves it.
	remoteURL := "git+ssh://git@127.0.0.1" + bareRepo

	b := newTestBackend(t)
	insertDoc(t, b, "src", "coll", mustDoc(t, "_id", int64(1), "v", "via-ssh"))
	c1 := commitDB(t, b, "src", "c1")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "src", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: "src", Remote: "origin", RefSpec: "main"}); err != nil {
		t.Fatalf("push over git+ssh wrapper: %v", err)
	}

	cres, err := b.DumboDBClone(ctx, &backends.CloneParams{From: remoteURL, As: "clonedb"})
	if err != nil {
		t.Fatalf("clone over git+ssh wrapper: %v", err)
	}
	if cres.Commit != c1 {
		t.Errorf("clone commit = %s, want c1 %s", cres.Commit, c1)
	}
	assertDocValue(t, ctx, b, "clonedb", "coll", int64(1), "via-ssh")
}

// TestDumboDBGit_FileRoundTrip exercises the GitRemoteFactory path (dolt stored
// in a git repository) hermetically over git+file://. It validates the
// dumbo-side wiring shared by every git+ scheme -- the git_cache_root param and
// PrepareDB running `git init --bare` -- so git+ssh differs only in transport.
// Requires the git CLI, which the factory shells out to.
func TestDumboDBGit_FileRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH; skipping git+file round-trip")
	}
	ctx := context.Background()
	b := newTestBackend(t)

	// Dolt git remotes require the bare repo to already have an initial branch,
	// so seed one (a plain git commit) before pushing -- exactly how a real
	// git+ssh host would be provisioned.
	bareRepo := filepath.Join(t.TempDir(), "remote.git")
	seedGitRemote(t, bareRepo, "main")
	remoteURL := "git+file://" + bareRepo

	insertDoc(t, b, "src", "coll", mustDoc(t, "_id", int64(1), "v", "git-hello"))
	c1 := commitDB(t, b, "src", "c1")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "src", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote: %v", err)
	}

	res, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: "src", Remote: "origin", RefSpec: "main"})
	if err != nil {
		t.Fatalf("push to git+file: %v", err)
	}
	if res.CommitPushed != c1 {
		t.Errorf("pushed commit = %s, want c1 %s", res.CommitPushed, c1)
	}

	// A second commit advances the git remote head.
	insertDoc(t, b, "src", "coll", mustDoc(t, "_id", int64(2), "v", "git-two"))
	c2 := commitDB(t, b, "src", "c2")
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: "src", Remote: "origin", RefSpec: "main"}); err != nil {
		t.Fatalf("push c2: %v", err)
	}

	// Clone the git remote into a fresh database and read the data back.
	cres, err := b.DumboDBClone(ctx, &backends.CloneParams{From: remoteURL, As: "clonedb"})
	if err != nil {
		t.Fatalf("clone from git+file: %v", err)
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
	assertDocValue(t, ctx, b, "clonedb", "coll", int64(2), "git-two")

	// Fetch into a third database and confirm the remote-tracking ref for main.
	insertDoc(t, b, "dst", "coll", mustDoc(t, "_id", int64(99)))
	commitDB(t, b, "dst", "seed")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "dst", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote (dst): %v", err)
	}
	fres, err := b.DumboDBFetch(ctx, &backends.FetchParams{DBName: "dst", Remote: "origin"})
	if err != nil {
		t.Fatalf("fetch from git+file: %v", err)
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
