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

package tests

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestPortFlag verifies that --port overrides the port in --addr.
func TestPortFlag(t *testing.T) {
	// Find a free port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	dataDir := t.TempDir()
	binary := filepath.Join(repoRoot(), ".runtime", "bin", "dolt")

	// Start dumbodb with --port (no --addr, so default addr is used with port override).
	cmd := exec.Command(binary, "--port", fmt.Sprintf("%d", port), "--data-dir", dataDir)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// Wait for server to be ready.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Connect and verify basic operation.
	client, err := mongo.Connect(options.Client().
		ApplyURI(fmt.Sprintf("mongodb://%s/", addr)).
		SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true}))
	require.NoError(t, err)
	t.Cleanup(func() { client.Disconnect(context.Background()) })

	ctx := context.Background()
	var result bson.M
	require.NoError(t, client.Database("testdb").RunCommand(ctx, bson.D{
		{Key: "doltStatus", Value: int32(1)},
	}).Decode(&result))
	assert.EqualValues(t, 1, result["ok"])
}

// TestAddrFlag verifies that --addr sets the listen address.
func TestAddrFlag(t *testing.T) {
	// Find a free port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	dataDir := t.TempDir()
	binary := filepath.Join(repoRoot(), ".runtime", "bin", "dolt")

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command(binary, "--addr", addr, "--data-dir", dataDir)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	// Wait for server to be ready.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Connect and verify basic operation.
	client, err := mongo.Connect(options.Client().
		ApplyURI(fmt.Sprintf("mongodb://%s/", addr)).
		SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true}))
	require.NoError(t, err)
	t.Cleanup(func() { client.Disconnect(context.Background()) })

	ctx := context.Background()
	var result bson.M
	require.NoError(t, client.Database("testdb").RunCommand(ctx, bson.D{
		{Key: "doltStatus", Value: int32(1)},
	}).Decode(&result))
	assert.EqualValues(t, 1, result["ok"])
}

// TestAutoCommitFlag verifies that --auto-commit is accepted and causes
// writes to auto-commit. This complements TestAutoCommit in
// auto_commit_test.go by testing the flag parsing path.
func TestAutoCommitFlag(t *testing.T) {
	env := startDumboDB(t, "--auto-commit")
	ctx := context.Background()

	coll := env.Client.Database("flagtest").Collection("items")
	_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: int32(1)}, {Key: "v", Value: 1}})
	require.NoError(t, err)

	// Without explicit doltCommit, the log should have 2 entries
	// (Initialize + auto-commit from the insert).
	var raw bson.M
	require.NoError(t, env.Client.Database("flagtest").RunCommand(ctx, bson.D{
		{Key: "doltLog", Value: int32(1)},
	}).Decode(&raw))
	lr := decodeLogResult(t, raw)
	assert.GreaterOrEqual(t, len(lr.Commits), 2,
		"auto-commit must produce at least Initialize + 1 auto-insert commit")
}
