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

package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	mongobson "go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/topology"
)

func TestLiveRuntimeInitialSyncSteadyApplyAndRestart(t *testing.T) {
	mongodBinary := os.Getenv("MONGOD_BIN")
	if mongodBinary == "" {
		t.Skip("set MONGOD_BIN to run the live replication runtime test")
	}
	mongoAddress := freeRuntimeAddress(t)
	_, mongoPort, err := net.SplitHostPort(mongoAddress)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		mongodBinary, "--replSet", "rs0", "--port", mongoPort,
		"--dbpath", t.TempDir(), "--bind_ip", "127.0.0.1", "--nounixsocket",
	)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	waitForRuntimePort(t, mongoAddress)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://"+mongoAddress+"/?directConnection=true"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	admin := client.Database("admin")
	initiate := mongobson.D{{Key: "replSetInitiate", Value: mongobson.D{
		{Key: "_id", Value: "rs0"},
		{Key: "members", Value: mongobson.A{mongobson.D{{Key: "_id", Value: 0}, {Key: "host", Value: mongoAddress}}}},
	}}}
	if err := admin.RunCommand(ctx, initiate).Err(); err != nil {
		t.Fatal(err)
	}
	waitForRuntimePrimary(t, ctx, admin)
	collection := client.Database("runtime_test").Collection("events")
	if _, err := collection.InsertOne(ctx, mongobson.D{{Key: "_id", Value: 1}, {Key: "value", Value: "initial"}}); err != nil {
		t.Fatal(err)
	}

	dataDirectory := t.TempDir()
	controlDirectory := t.TempDir()
	backend, manager, runtime := newLiveRuntime(t, dataDirectory, controlDirectory, mongoAddress)
	runtimeContext, stopRuntime := context.WithCancel(ctx)
	runtimeDone := make(chan struct{})
	go func() {
		runtime.Run(runtimeContext)
		close(runtimeDone)
	}()
	waitForRuntimeState(t, ctx, manager, topology.StateSecondary)
	waitForRuntimeDocuments(t, ctx, backend, 1)

	if _, err := collection.InsertOne(ctx, mongobson.D{{Key: "_id", Value: 2}, {Key: "value", Value: "steady"}}); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeDocuments(t, ctx, backend, 2)
	stopRuntime()
	select {
	case <-runtimeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("replication runtime did not stop after cancellation")
	}
	backend.Close()

	reopenedBackend, reopenedManager, reopenedRuntime := newLiveRuntime(t, dataDirectory, controlDirectory, mongoAddress)
	defer reopenedBackend.Close()
	restartContext, stopRestart := context.WithCancel(ctx)
	restartDone := make(chan struct{})
	go func() {
		reopenedRuntime.Run(restartContext)
		close(restartDone)
	}()
	defer func() {
		stopRestart()
		select {
		case <-restartDone:
		case <-time.After(5 * time.Second):
			t.Error("replication runtime did not stop after cancellation")
		}
	}()
	waitForRuntimeState(t, ctx, reopenedManager, topology.StateSecondary)
	if _, err := collection.InsertOne(ctx, mongobson.D{{Key: "_id", Value: 3}, {Key: "value", Value: "restart"}}); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeDocuments(t, ctx, reopenedBackend, 3)

	beforeRemoval := reopenedManager.ControlSnapshot()
	removedConfiguration := *reopenedManager.Snapshot().Configuration
	removedConfiguration.Version++
	removedConfiguration.Members = append([]control.MemberConfiguration(nil), removedConfiguration.Members[:1]...)
	if err := reopenedManager.InstallConfiguration(removedConfiguration, reopenedManager.Snapshot().Term); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeState(t, ctx, reopenedManager, topology.StateRemoved)
	waitForRuntimePhase(t, ctx, reopenedManager, "detached")
	if _, err := collection.InsertOne(ctx, mongobson.D{{Key: "_id", Value: 4}, {Key: "value", Value: "after-removal"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if count := runtimeDocumentCount(t, ctx, reopenedBackend); count != 3 {
		t.Fatalf("removed member replicated post-removal write: document count = %d, want 3", count)
	}
	afterRemoval := reopenedManager.ControlSnapshot()
	if afterRemoval.Checkpoint != beforeRemoval.Checkpoint || len(afterRemoval.CommitIntervals) != len(beforeRemoval.CommitIntervals) {
		t.Fatalf("removal changed replicated history: before=%+v after=%+v", beforeRemoval, afterRemoval)
	}
	select {
	case <-restartDone:
		t.Fatal("replication runtime stopped when the member was removed")
	default:
	}
}

func newLiveRuntime(t *testing.T, dataDirectory, _ string, source string) (backends.Backend, *topology.Manager, *Runtime) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	backend, err := dolt.NewBackend(dataDirectory, logger, false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	store, err := control.Open(backend, control.Configuration{SetName: "rs0", MemberHost: "dumbo.example:27017"})
	if err != nil {
		backend.Close()
		t.Fatal(err)
	}
	manager := topology.New(store)
	if manager.Snapshot().Configuration == nil {
		if err := manager.InstallConfiguration(control.ReplicaConfiguration{
			SetName: "rs0", Version: 1, Term: 1, ProtocolVersion: 1, ReplicaSetID: "live-test",
			Members: []control.MemberConfiguration{
				{MemberID: 0, Host: source, Priority: 1, Votes: 1},
				{MemberID: 1, Host: "dumbo.example:27017", Hidden: true, Priority: 0, Votes: 0},
			},
		}, 1); err != nil {
			backend.Close()
			t.Fatal(err)
		}
	}
	if err := manager.ObserveHeartbeat(source, topology.Heartbeat{
		SetName: "rs0", MemberID: 0, State: topology.StatePrimary, Term: 1, PrimaryID: 0,
	}); err != nil {
		backend.Close()
		t.Fatal(err)
	}
	runtime, err := New(backend, store, manager, logger, nil)
	if err != nil {
		backend.Close()
		t.Fatal(err)
	}
	return backend, manager, runtime
}

func waitForRuntimeDocuments(t *testing.T, ctx context.Context, backend backends.Backend, count int64) {
	t.Helper()
	database, err := backend.Database("runtime_test")
	if err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection("events")
	if err != nil {
		t.Fatal(err)
	}
	for {
		result, err := collection.Count(ctx, nil)
		if err == nil && result.Count == count {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %d replicated documents: %v", count, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitForRuntimeState(t *testing.T, ctx context.Context, manager *topology.Manager, state topology.MemberState) {
	t.Helper()
	for manager.Snapshot().State != state {
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for replication state %s: %v", state, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitForRuntimePhase(t *testing.T, ctx context.Context, manager *topology.Manager, phase string) {
	t.Helper()
	for manager.Snapshot().Runtime.Phase != phase {
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for replication phase %s: %v", phase, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func runtimeDocumentCount(t *testing.T, ctx context.Context, backend backends.Backend) int64 {
	t.Helper()
	database, err := backend.Database("runtime_test")
	if err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection("events")
	if err != nil {
		t.Fatal(err)
	}
	result, err := collection.Count(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result.Count
}

func freeRuntimeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func waitForRuntimePort(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("mongod did not listen on %s", address)
}

func waitForRuntimePrimary(t *testing.T, ctx context.Context, admin *mongo.Database) {
	t.Helper()
	for {
		var response struct {
			Writable bool `bson:"isWritablePrimary"`
		}
		if err := admin.RunCommand(ctx, mongobson.D{{Key: "hello", Value: 1}}).Decode(&response); err == nil && response.Writable {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(fmt.Errorf("waiting for primary: %w", ctx.Err()))
		case <-time.After(100 * time.Millisecond):
		}
	}
}
