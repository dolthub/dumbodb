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

// Package tests contains dongo-specific regression and integration tests.
//
// MongoDB parity tests (compatibility between MongoDB and Dongo) live in the
// dolthub/dongo-parity-testing repository. This package retains only tests for
// dongo-internal behaviors that have no MongoDB equivalent, such as internal
// resource management and implementation-specific edge cases.
//
// Run with:
//
//	go test -v ./tests/
package tests

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// dongoTestEnv holds a running dongo process and a connected MongoDB client.
type dongoTestEnv struct {
	cmd     *exec.Cmd
	client  *mongo.Client
	dataDir string
	port    int
}

// repoRoot returns the repository root directory (two levels above this file).
func repoRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..")
}

// startDongo launches a fresh dongo instance on a random free port.
func startDongo(tb testing.TB) *dongoTestEnv {
	tb.Helper()

	// Find a free port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	dataDir := tb.TempDir()
	binary := filepath.Join(repoRoot(), ".runtime", "bin", "dongo")

	if _, err := os.Stat(binary); os.IsNotExist(err) {
		// Try to build on the fly.
		build := exec.Command("go", "build", "-o", binary, "./cmd/dongo/")
		build.Dir = repoRoot()
		if out, err := build.CombinedOutput(); err != nil {
			tb.Fatalf("failed to build dongo: %v\n%s", err, out)
		}
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command(binary, "--addr", addr, "--data-dir", dataDir)
	cmd.Stdout = nil
	cmd.Stderr = nil
	require.NoError(tb, cmd.Start())

	env := &dongoTestEnv{
		cmd:     cmd,
		dataDir: dataDir,
		port:    port,
	}

	tb.Cleanup(func() {
		if env.client != nil {
			env.client.Disconnect(context.Background()) //nolint:errcheck
		}
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
	})

	// Wait for dongo to be ready.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Connect with the MongoDB driver.
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(fmt.Sprintf("mongodb://%s/", addr)))
	require.NoError(tb, err)
	env.client = client

	return env
}

// collection returns a fresh collection for each test.
func (env *dongoTestEnv) collection(tb testing.TB) *mongo.Collection {
	tb.Helper()

	name := fmt.Sprintf("col_%d", rand.Int64())
	coll := env.client.Database("testdb").Collection(name)
	tb.Cleanup(func() {
		coll.Drop(context.Background()) //nolint:errcheck
	})

	return coll
}

// insertDocs inserts multiple documents and fails the test on error.
func insertDocs(tb testing.TB, coll *mongo.Collection, docs ...bson.D) {
	tb.Helper()

	iface := make([]interface{}, len(docs))
	for i, d := range docs {
		iface[i] = d
	}

	ctx := context.Background()
	_, err := coll.InsertMany(ctx, iface)
	require.NoError(tb, err)
}

// e is shorthand for bson.E (key-value pair in a bson.D document).
func e(key string, val interface{}) primitive.E {
	return primitive.E{Key: key, Value: val}
}

// d builds a bson.D from key-value pairs.
func d(elems ...primitive.E) bson.D {
	return bson.D(elems)
}

// TestBSON_array_nested is a regression test for nested array support (do-dor).
// MongoDB supports arrays containing arrays; dongo must store and retrieve them
// without error.
func TestBSON_array_nested(t *testing.T) {
	env := startDongo(t)
	coll := env.collection(t)

	ctx := context.Background()

	// Insert a document that contains a nested array: { arr: [[1,2],[3,4]] }.
	doc := d(e("arr", bson.A{bson.A{int32(1), int32(2)}, bson.A{int32(3), int32(4)}}))
	_, err := coll.InsertOne(ctx, doc)
	require.NoError(t, err, "inserting a document with nested arrays must not fail")

	// Verify the document round-trips correctly.
	var result bson.D
	require.NoError(t, coll.FindOne(ctx, bson.D{}).Decode(&result))

	var arr bson.A
	for _, el := range result {
		if el.Key == "arr" {
			arr = el.Value.(bson.A)
			break
		}
	}
	require.NotNil(t, arr, "arr field must be present in the retrieved document")
	require.Len(t, arr, 2, "arr must have two sub-arrays")
}

// TestFind_CursorCleanupOnFilterError is a regression test for MultiCloser leaks.
//
// When a find command fails mid-iteration due to an invalid filter (e.g. a
// $jsonSchema with a malformed "required" clause), the cursor's underlying
// MultiCloser must be closed before the error is returned to the client.
// If it is not, GC will fire the finalizer and panic with:
//
//	panic: *iterator.MultiCloser has not been finalized
//
// This test does NOT run in parallel so that forcing GC here does not interact
// with other goroutines' resource tracking.
func TestFind_CursorCleanupOnFilterError(t *testing.T) {
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("name", "alice")),
		d(e("_id", int32(2)), e("name", "bob")),
	)

	ctx := context.Background()

	// $jsonSchema with required as a string (not an array) causes FilterDocument
	// to return an error on the first document, which fails ConsumeValuesN.
	// The fix: MsgFind calls h.cursors.CloseAndRemove before returning the error,
	// properly finalizing the MultiCloser.
	_, err := coll.Find(ctx, d(e("$jsonSchema", d(e("required", "not-an-array")))))
	require.Error(t, err)

	// Force GC multiple times to flush the finalizer goroutine.
	// If the MultiCloser leaked (Close not called), this will panic.
	for i := 0; i < 5; i++ {
		runtime.GC()
	}
}
