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

// Package support holds the shared test harness and response decoders used by
// both the dumbodb-internal tests (package tests) and the manual-verification
// analogs (package verify). It lives in its own importable package because Go
// test files cannot be shared across directories.
package support

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// buildOnce ensures the dumbodb binary is built exactly once per test run.
var buildOnce sync.Once

type Env struct {
	server    *server
	Client    *mongo.Client
	dataDir   string
	Port      int
	extraArgs []string
}

// Restart stops the server and starts a fresh one with the same data dir and
// flags, on the same port when it is still free, then reconnects the client.
func (env *Env) Restart(tb testing.TB) {
	tb.Helper()
	if env.Client != nil {
		env.Client.Disconnect(context.Background()) //nolint:errcheck
	}
	env.server.stop()

	env.server, env.Port = startServer(tb, env.Port, env.dataDir, env.extraArgs)
	env.Client = connect(tb, env.Port)
}

// DataDir returns the server's --data-dir, for tests that inspect on-disk state.
func (env *Env) DataDir() string {
	return env.dataDir
}

// RepoRoot returns the repository root directory (two levels above this file).
func RepoRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

// StartDumboDB launches a fresh dumbodb instance on a random free port.
// Optional extraArgs are appended to the dumbodb command line (e.g. "--auto-commit").
func StartDumboDB(tb testing.TB, extraArgs ...string) *Env {
	tb.Helper()

	port := freePort(tb)
	dataDir := tb.TempDir()
	binary := serverBinary()

	var buildErr error
	buildOnce.Do(func() {
		if mkErr := os.MkdirAll(filepath.Dir(binary), 0o755); mkErr != nil {
			buildErr = mkErr
			return
		}
		// Build to a private path and rename into place. Packages tests and
		// verify run as separate processes sharing this path, and execve
		// fails with ETXTBSY while another builder holds the target open for
		// write. Rename is atomic and never leaves the final path writable.
		tmp := fmt.Sprintf("%s.tmp.%d", binary, os.Getpid())
		build := exec.Command("go", "build", "-o", tmp, "./cmd/dumbodb/")
		build.Dir = RepoRoot()
		if out, err := build.CombinedOutput(); err != nil {
			os.Remove(tmp)
			buildErr = fmt.Errorf("failed to build dumbodb: %w\n%s", err, out)
			return
		}
		if err := os.Rename(tmp, binary); err != nil {
			os.Remove(tmp)
			buildErr = fmt.Errorf("failed to install dumbodb binary: %w", err)
		}
	})
	if buildErr != nil {
		tb.Fatalf("failed to build dumbodb: %v", buildErr)
	}

	env := &Env{
		dataDir:   dataDir,
		extraArgs: extraArgs,
	}
	env.server, env.Port = startServer(tb, port, dataDir, extraArgs)
	tb.Cleanup(func() {
		if env.Client != nil {
			env.Client.Disconnect(context.Background()) //nolint:errcheck
		}
	})
	env.Client = connect(tb, env.Port)

	return env
}

func serverBinary() string {
	return filepath.Join(RepoRoot(), ".runtime", "bin", "dolt")
}

func freePort(tb testing.TB) int {
	tb.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// server is a running dumbodb process. exited closes when the process ends.
type server struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

func (s *server) stop() {
	s.cmd.Process.Kill() //nolint:errcheck
	<-s.exited
}

// startServer starts dumbodb on port and returns once that process itself
// logs that it is listening. A free port can be taken by another test's server
// before this one binds it, and a dial would then reach that server, so a
// process that exits before listening is retried on a new port. Returns the
// port in use.
func startServer(tb testing.TB, port int, dataDir string, extraArgs []string) (*server, int) {
	tb.Helper()

	const attempts = 5
	for attempt := 1; ; attempt++ {
		s, listening := launchServer(tb, port, dataDir, extraArgs)
		select {
		case <-listening:
			return s, port
		case <-s.exited:
			if attempt == attempts {
				tb.Fatalf("dumbodb exited before listening on port %d, %d attempts", port, attempts)
			}
			port = freePort(tb)
		case <-time.After(15 * time.Second):
			s.stop()
			tb.Fatalf("dumbodb did not listen on port %d within 15s", port)
		}
	}
}

// launchServer starts one dumbodb process. listening closes when the process
// logs that it is listening on addr, which it does only after binding it.
func launchServer(tb testing.TB, port int, dataDir string, extraArgs []string) (*server, <-chan struct{}) {
	tb.Helper()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	args := append([]string{"--addr", addr, "--data-dir", dataDir}, extraArgs...)
	cmd := exec.Command(serverBinary(), args...)
	stderr, err := cmd.StderrPipe()
	require.NoError(tb, err)
	require.NoError(tb, cmd.Start())

	s := &server{cmd: cmd, exited: make(chan struct{})}
	listening := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		announced := false
		for scanner.Scan() {
			line := scanner.Text()
			if !announced && strings.Contains(line, "Listening on ") && strings.Contains(line, " "+addr+"...") {
				announced = true
				close(listening)
			}
		}
		io.Copy(io.Discard, stderr) //nolint:errcheck
		cmd.Wait()                  //nolint:errcheck
		close(s.exited)
	}()
	tb.Cleanup(s.stop)
	return s, listening
}

// connect returns a client for the server on port. DefaultDocumentM decodes
// sub-documents into bson.M when the surrounding element type is interface{},
// which most tests type-assert against.
func connect(tb testing.TB, port int) *mongo.Client {
	tb.Helper()
	client, err := mongo.Connect(options.Client().
		ApplyURI(fmt.Sprintf("mongodb://127.0.0.1:%d/", port)).
		SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true}))
	require.NoError(tb, err)
	return client
}

func (env *Env) Collection(tb testing.TB) *mongo.Collection {
	tb.Helper()

	name := fmt.Sprintf("col_%d", rand.Int64())
	coll := env.Client.Database("testdb").Collection(name)
	tb.Cleanup(func() {
		coll.Drop(context.Background()) //nolint:errcheck
	})

	return coll
}

func Commit(tb testing.TB, env *Env, dbName, message string, author ...string) string {
	tb.Helper()

	a := "testuser"
	if len(author) > 0 {
		a = author[0]
	}

	ctx := context.Background()
	var result bson.M
	err := env.Client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: int32(1)},
		{Key: "message", Value: message},
		{Key: "author", Value: a},
	}).Decode(&result)
	require.NoError(tb, err, "doltCommit must succeed")

	hash, ok := result["commitId"].(string)
	require.True(tb, ok, "doltCommit must return a string hash, got %T", result["commitId"])
	require.NotEmpty(tb, hash, "commit hash must not be empty")
	return hash
}

// CommitAllowEmpty runs doltCommit with allowEmpty:true so it succeeds even
// when the working set has no pending changes versus HEAD.
func CommitAllowEmpty(tb testing.TB, env *Env, dbName, message string, author ...string) string {
	tb.Helper()

	a := "testuser"
	if len(author) > 0 {
		a = author[0]
	}

	ctx := context.Background()
	var result bson.M
	err := env.Client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "doltCommit", Value: int32(1)},
		{Key: "message", Value: message},
		{Key: "author", Value: a},
		{Key: "allowEmpty", Value: true},
	}).Decode(&result)
	require.NoError(tb, err, "doltCommit (allowEmpty) must succeed")

	hash, ok := result["commitId"].(string)
	require.True(tb, ok, "doltCommit must return a string hash, got %T", result["commitId"])
	require.NotEmpty(tb, hash, "commit hash must not be empty")
	return hash
}

// RunCommandRaw runs a command and returns the raw BSON response as bson.M,
// even when the response contains ok:0 (which the driver would otherwise turn
// into a CommandError without exposing the document).
func RunCommandRaw(t *testing.T, db *mongo.Database, cmd interface{}) bson.M {
	t.Helper()

	result := db.RunCommand(context.Background(), cmd)
	rawBytes, err := result.Raw()
	if err != nil {
		if rawBytes == nil {
			t.Fatalf("RunCommandRaw: no raw bytes and error: %v", err)
		}
	}

	var m bson.M
	dec := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(rawBytes)))
	dec.DefaultDocumentM()
	require.NoError(t, dec.Decode(&m))
	return m
}

type StatusResult struct {
	Branch   string
	CommitID string // HEAD commit hash; present only when workspace is clean
	Tables   []TableStatusEntry
}

type TableStatusEntry struct {
	Name     string
	Status   string
	Added    int
	Modified int
	Deleted  int
}

func DecodeStatusResult(t *testing.T, raw bson.M) StatusResult {
	t.Helper()

	branch, _ := raw["branch"].(string)
	commitID, _ := raw["commitId"].(string)

	rawChanges, ok := raw["changes"]
	require.True(t, ok, "doltStatus result missing 'changes' field")

	changesArr, ok := rawChanges.(bson.A)
	require.True(t, ok, "doltStatus 'changes' is not an array, got %T", rawChanges)

	var out StatusResult
	out.Branch = branch
	out.CommitID = commitID

	for _, ch := range changesArr {
		cm, ok := ch.(bson.M)
		require.True(t, ok, "changes entry is not a document, got %T", ch)

		if cm["type"] != "collection" {
			continue
		}

		documents, _ := cm["documents"].(bson.M)
		entry := TableStatusEntry{
			Name:     fmt.Sprintf("%v", cm["name"]),
			Status:   fmt.Sprintf("%v", cm["status"]),
			Added:    ToInt(documents["added"]),
			Modified: ToInt(documents["modified"]),
			Deleted:  ToInt(documents["removed"]),
		}
		out.Tables = append(out.Tables, entry)
	}

	return out
}

// ToInt coerces a BSON numeric (int32/int64/float64) into a Go int, returning 0
// for any other (including missing) value.
func ToInt(v any) int {
	switch n := v.(type) {
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

// FindTableStatus returns the TableStatusEntry for the named collection, or nil.
func FindTableStatus(sr StatusResult, name string) *TableStatusEntry {
	for i := range sr.Tables {
		if sr.Tables[i].Name == name {
			return &sr.Tables[i]
		}
	}
	return nil
}

func RunStatus(t *testing.T, env *Env, dbName string) StatusResult {
	t.Helper()
	ctx := context.Background()

	var raw bson.M
	require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "doltStatus", Value: int32(1)},
	}).Decode(&raw))

	return DecodeStatusResult(t, raw)
}

type LogResult struct {
	Commits []CommitEntry
}

type CommitEntry struct {
	CommitID           string
	Parent1            string
	Parent2            string
	Message            string
	Author             string
	Committer          string
	CommitterTimestamp interface{}
	Refs               []string
}

func DecodeLogResult(t *testing.T, raw bson.M) LogResult {
	t.Helper()

	_, hasBranch := raw["branch"]
	require.False(t, hasBranch, "doltLog result must not include 'branch' field")

	rawCommits, ok := raw["commits"]
	require.True(t, ok, "doltLog result missing 'commits' field")

	commitsArr, ok := rawCommits.(bson.A)
	require.True(t, ok, "doltLog 'commits' is not an array, got %T", rawCommits)

	var out LogResult

	for _, c := range commitsArr {
		cm, ok := c.(bson.M)
		require.True(t, ok, "commits entry is not a document, got %T", c)

		entry := CommitEntry{
			CommitID: fmt.Sprintf("%v", cm["commitId"]),
			Message:  fmt.Sprintf("%v", cm["message"]),
			Author:   fmt.Sprintf("%v", cm["author"]),
		}
		if c, ok := cm["committer"]; ok {
			entry.Committer = fmt.Sprintf("%v", c)
		}
		if ct, ok := cm["committerTimestamp"]; ok {
			entry.CommitterTimestamp = ct
		}
		if p1, ok := cm["parent1"]; ok {
			entry.Parent1 = fmt.Sprintf("%v", p1)
		}
		if p2, ok := cm["parent2"]; ok {
			entry.Parent2 = fmt.Sprintf("%v", p2)
		}
		if rawRefs, ok := cm["refs"]; ok {
			if refsArr, ok := rawRefs.(bson.A); ok {
				for _, r := range refsArr {
					entry.Refs = append(entry.Refs, fmt.Sprintf("%v", r))
				}
			}
		}
		out.Commits = append(out.Commits, entry)
	}

	return out
}
