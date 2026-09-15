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

package topology

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	mongobson "go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/dolthub/dumbodb/internal/replication/control"
)

func TestLiveMongoHeartbeatConfiguration(t *testing.T) {
	mongodBinary := os.Getenv("MONGOD_BIN")
	if mongodBinary == "" {
		t.Skip("set MONGOD_BIN to run the live MongoDB heartbeat test")
	}
	mongoAddress := freeTestAddress(t)
	dumboAddress := freeTestAddress(t)
	_, mongoPort, err := net.SplitHostPort(mongoAddress)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		mongodBinary,
		"--replSet", "rs0",
		"--port", mongoPort,
		"--dbpath", t.TempDir(),
		"--bind_ip", "127.0.0.1",
		"--nounixsocket",
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
	waitForTestPort(t, mongoAddress)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
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
	waitForPrimary(t, ctx, admin)

	var current struct {
		Config mongobson.M `bson:"config"`
	}
	if err := admin.RunCommand(ctx, mongobson.D{{Key: "replSetGetConfig", Value: 1}}).Decode(&current); err != nil {
		t.Fatal(err)
	}
	current.Config["version"] = current.Config["version"].(int32) + 1
	members := current.Config["members"].(mongobson.A)
	current.Config["members"] = append(members, mongobson.D{
		{Key: "_id", Value: 3},
		{Key: "host", Value: dumboAddress},
		{Key: "hidden", Value: true},
		{Key: "priority", Value: 0},
		{Key: "votes", Value: 0},
	})
	if err := admin.RunCommand(ctx, mongobson.D{{Key: "replSetReconfig", Value: current.Config}}).Err(); err != nil {
		t.Fatal(err)
	}

	store, err := control.Open(t.TempDir(), control.Configuration{SetName: "rs0", Branch: "mongo", MemberHost: dumboAddress})
	if err != nil {
		t.Fatal(err)
	}
	manager := New(store)
	if err := manager.ObserveMemberContact(mongoAddress, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	mesh := NewHeartbeatMesh(manager, nil)
	mesh.poll(ctx)
	mesh.close()
	state := manager.Snapshot()
	if state.Configuration == nil || state.MemberID != 3 || state.State != StateStartup2 {
		t.Fatalf("live heartbeat topology = %+v", state)
	}
	if state.PrimaryHost != mongoAddress || state.SyncSource != mongoAddress {
		t.Fatalf("live heartbeat source = %+v", state)
	}
}

func freeTestAddress(t *testing.T) string {
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

func waitForTestPort(t *testing.T, address string) {
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

func waitForPrimary(t *testing.T, ctx context.Context, admin *mongo.Database) {
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
