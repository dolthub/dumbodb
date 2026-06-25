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
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// Env holds a running dumbodb process and a connected MongoDB client.
type Env struct {
	cmd     *exec.Cmd
	Client  *mongo.Client
	dataDir string
	Port    int
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

	// Find a free port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	dataDir := tb.TempDir()
	binary := filepath.Join(RepoRoot(), ".runtime", "bin", "dolt")

	// Build once per test run to ensure the binary is up-to-date with current source.
	var buildErr error
	buildOnce.Do(func() {
		if mkErr := os.MkdirAll(filepath.Dir(binary), 0o755); mkErr != nil {
			buildErr = mkErr
			return
		}
		build := exec.Command("go", "build", "-o", binary, "./cmd/dumbodb/")
		build.Dir = RepoRoot()
		if out, err := build.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("failed to build dumbodb: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		tb.Fatalf("failed to build dumbodb: %v", buildErr)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	args := append([]string{"--addr", addr, "--data-dir", dataDir}, extraArgs...)
	cmd := exec.Command(binary, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	require.NoError(tb, cmd.Start())

	env := &Env{
		cmd:     cmd,
		dataDir: dataDir,
		Port:    port,
	}

	tb.Cleanup(func() {
		if env.Client != nil {
			env.Client.Disconnect(context.Background()) //nolint:errcheck
		}
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
	})

	// Wait for dumbodb to be ready.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Connect with the MongoDB driver. DefaultDocumentM decodes sub-documents
	// into bson.M when the surrounding element type is interface{}, which most
	// tests type-assert against.
	client, err := mongo.Connect(options.Client().
		ApplyURI(fmt.Sprintf("mongodb://%s/", addr)).
		SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true}))
	require.NoError(tb, err)
	env.Client = client

	return env
}

// Collection returns a fresh collection for each test.
func (env *Env) Collection(tb testing.TB) *mongo.Collection {
	tb.Helper()

	name := fmt.Sprintf("col_%d", rand.Int64())
	coll := env.Client.Database("testdb").Collection(name)
	tb.Cleanup(func() {
		coll.Drop(context.Background()) //nolint:errcheck
	})

	return coll
}

// Commit runs doltCommit on the given database and returns the commit hash.
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

// StatusResult holds the decoded top-level response from a doltStatus command.
type StatusResult struct {
	Branch   string
	CommitID string // HEAD commit hash; present only when workspace is clean
	Tables   []TableStatusEntry
}

// TableStatusEntry holds one entry from the "collections" array of a doltStatus response.
type TableStatusEntry struct {
	Name     string
	Status   string
	Added    int
	Modified int
	Deleted  int
}

// DecodeStatusResult parses the raw bson.M from a doltStatus RunCommand into the
// typed helpers above, failing the test if the shape is unexpected.
func DecodeStatusResult(t *testing.T, raw bson.M) StatusResult {
	t.Helper()

	branch, _ := raw["branch"].(string)
	commitID, _ := raw["commitId"].(string)

	rawTables, ok := raw["collections"]
	require.True(t, ok, "doltStatus result missing 'collections' field")

	tablesArr, ok := rawTables.(bson.A)
	require.True(t, ok, "doltStatus 'collections' is not an array, got %T", rawTables)

	var out StatusResult
	out.Branch = branch
	out.CommitID = commitID

	for _, tbl := range tablesArr {
		tm, ok := tbl.(bson.M)
		require.True(t, ok, "collections entry is not a document, got %T", tbl)

		entry := TableStatusEntry{
			Name:     fmt.Sprintf("%v", tm["name"]),
			Status:   fmt.Sprintf("%v", tm["status"]),
			Added:    ToInt(tm["added"]),
			Modified: ToInt(tm["modified"]),
			Deleted:  ToInt(tm["deleted"]),
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

// RunStatus issues a doltStatus command on the named db and returns the decoded result.
func RunStatus(t *testing.T, env *Env, dbName string) StatusResult {
	t.Helper()
	ctx := context.Background()

	var raw bson.M
	require.NoError(t, env.Client.Database(dbName).RunCommand(ctx, bson.D{
		{Key: "doltStatus", Value: int32(1)},
	}).Decode(&raw))

	return DecodeStatusResult(t, raw)
}

// LogResult holds the decoded top-level response from a doltLog command.
type LogResult struct {
	Commits []CommitEntry
}

// CommitEntry holds one entry from the "commits" array of a doltLog response.
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

// DecodeLogResult parses the raw bson.M from a doltLog RunCommand into the typed
// helpers above, failing the test if the shape is unexpected.
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
