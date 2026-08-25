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
	"net"
	"path/filepath"
	"sync"
	"testing"

	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
	"github.com/dolthub/dolt/go/libraries/doltcore/remotesrv"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/nbs"

	"github.com/dolthub/dumbodb/internal/backends"
)

const remotesrvMemTableSize = 128 * 1024 * 1024

// localCSCache is a minimal DBCache backing an in-process remotesrv with local
// NBS stores, one per repo path. It mirrors the LocalCSCache in Dolt's remotesrv
// binary, which lives in package main and so cannot be imported here.
type localCSCache struct {
	mu  sync.Mutex
	dbs map[string]remotesrv.RemoteSrvStore
	fs  filesys.Filesys
}

func newLocalCSCache(fs filesys.Filesys) *localCSCache {
	return &localCSCache{dbs: make(map[string]remotesrv.RemoteSrvStore), fs: fs}
}

func (c *localCSCache) Get(ctx context.Context, repoPath, nbfVerStr string) (remotesrv.RemoteSrvStore, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := filepath.FromSlash(repoPath)
	if cs, ok := c.dbs[id]; ok {
		return cs, nil
	}

	if err := c.fs.MkDirs(id); err != nil {
		return nil, err
	}
	path, err := c.fs.Abs(id)
	if err != nil {
		return nil, err
	}
	cs, err := nbs.NewLocalStore(ctx, nbfVerStr, path, remotesrvMemTableSize, nbs.NewUnlimitedMemQuotaProvider(), false)
	if err != nil {
		return nil, err
	}
	c.dbs[id] = cs
	return cs, nil
}

// startRemotesrv brings up an in-process remotesapi server over insecure http,
// serving repos out of dir. It returns the host:port the server advertises and a
// stop function. gRPC and file traffic are multiplexed on a single listener.
func startRemotesrv(t *testing.T, dir string) (host string, stop func()) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	fs, err := filesys.LocalFilesysWithWorkingDir(dir)
	if err != nil {
		t.Fatalf("remotesrv fs: %v", err)
	}

	server, err := remotesrv.NewServer(remotesrv.ServerArgs{
		HttpHost:           addr,
		HttpListenAddr:     addr,
		GrpcListenAddr:     addr,
		FS:                 fs,
		DBCache:            newLocalCSCache(fs),
		ConcurrencyControl: remotesapi.PushConcurrencyControl_PUSH_CONCURRENCY_CONTROL_IGNORE_WORKING_SET,
	})
	if err != nil {
		t.Fatalf("remotesrv NewServer: %v", err)
	}
	listeners, err := server.Listeners()
	if err != nil {
		t.Fatalf("remotesrv Listeners: %v", err)
	}
	go server.Serve(listeners)

	return addr, func() { server.GracefulStop() }
}

// TestDumboDBPushFetch_HTTPRoundTrip exercises the full remotesapi gRPC client
// path (dial provider, chunk transfer, ref update) against an in-process
// remotesrv over insecure http. No credentials are involved: an insecure remote
// requires none, and DOLT_ROOT_PATH is pointed at an empty dir so the host's
// real dolt credentials are never read.
func TestDumboDBPushFetch_HTTPRoundTrip(t *testing.T) {
	t.Setenv("DOLT_ROOT_PATH", t.TempDir())
	ctx := context.Background()

	remoteHost, stop := startRemotesrv(t, t.TempDir())
	defer stop()
	remoteURL := fmt.Sprintf("http://%s/dumbo/roundtrip", remoteHost)

	// Producer db: seed, commit, push to the empty remote.
	src := newTestBackend(t)
	insertDoc(t, src, "srcdb", "col", mustDoc(t, "_id", int64(1), "v", int64(1)))
	c1 := commitDB(t, src, "srcdb", "c1")

	if _, err := src.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "srcdb", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	res, err := src.DumboDBPush(ctx, &backends.PushParams{DBName: "srcdb", Remote: "origin", Branch: "main"})
	if err != nil {
		t.Fatalf("push c1: %v", err)
	}
	if res.Commit != c1 {
		t.Errorf("pushed commit = %s, want c1 %s", res.Commit, c1)
	}

	// A second commit must transfer the new chunks and advance the remote head.
	insertDoc(t, src, "srcdb", "col", mustDoc(t, "_id", int64(2), "v", int64(2)))
	c2 := commitDB(t, src, "srcdb", "c2")
	if _, err := src.DumboDBPush(ctx, &backends.PushParams{DBName: "srcdb", Remote: "origin", Branch: "main"}); err != nil {
		t.Fatalf("push c2: %v", err)
	}

	// Consumer db: fetch from the same remote, assert the tracking ref matches.
	dst := newTestBackend(t)
	insertDoc(t, dst, "dstdb", "col", mustDoc(t, "_id", int64(99)))
	commitDB(t, dst, "dstdb", "seed")
	if _, err := dst.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "dstdb", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote (dst): %v", err)
	}
	fres, err := dst.DumboDBFetch(ctx, &backends.FetchParams{DBName: "dstdb", Remote: "origin"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
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

// TestDumboDBClone_HTTPRemote clones a new database from an in-process remotesrv
// over insecure http, exercising the gRPC clone path with no credentials.
func TestDumboDBClone_HTTPRemote(t *testing.T) {
	t.Setenv("DOLT_ROOT_PATH", t.TempDir())
	ctx := context.Background()

	remoteHost, stop := startRemotesrv(t, t.TempDir())
	defer stop()
	remoteURL := fmt.Sprintf("http://%s/dumbo/clonesrc", remoteHost)

	// Seed a source db and push it to the remote.
	src := newTestBackend(t)
	insertDoc(t, src, "srcdb", "coll", mustDoc(t, "_id", int64(1), "v", "cloned-over-grpc"))
	c1 := commitDB(t, src, "srcdb", "c1")
	if _, err := src.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "srcdb", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	if _, err := src.DumboDBPush(ctx, &backends.PushParams{DBName: "srcdb", Remote: "origin", Branch: "main"}); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Clone from the remote into a fresh backend/database.
	dst := newTestBackend(t)
	res, err := dst.DumboDBClone(ctx, &backends.CloneParams{From: remoteURL, As: "clonedb"})
	if err != nil {
		t.Fatalf("clone over http: %v", err)
	}
	if res.Commit != c1 {
		t.Errorf("clone default commit = %s, want c1 %s", res.Commit, c1)
	}
	if res.DefaultBranch != "main" {
		t.Errorf("clone default branch = %q, want main", res.DefaultBranch)
	}

	// The cloned database is readable with the source document present.
	adb, err := dst.Database("clonedb")
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
		if err != nil {
			break
		}
		if id, _ := doc.Get("_id"); id == int64(1) {
			found = true
			if v, _ := doc.Get("v"); v != "cloned-over-grpc" {
				t.Errorf("cloned doc v = %v, want \"cloned-over-grpc\"", v)
			}
		}
	}
	if !found {
		t.Error("cloned document _id:1 not found (gRPC clone did not materialize data)")
	}
}
