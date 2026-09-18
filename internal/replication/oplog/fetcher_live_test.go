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

package oplog

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
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/testutil"
	"github.com/dolthub/dumbodb/internal/replication/topology"
)

func TestLiveMongoInclusiveOplogFetch(t *testing.T) {
	mongodBinary := os.Getenv("MONGOD_BIN")
	if mongodBinary == "" {
		t.Skip("set MONGOD_BIN to run the live MongoDB oplog fetch test")
	}
	mongoAddress := freeOplogTestAddress(t)
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
	waitForOplogTestPort(t, mongoAddress)

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
	waitForOplogPrimary(t, ctx, admin)

	var boundary struct {
		Timestamp primitive.Timestamp `bson:"ts"`
		Term      int64               `bson:"t"`
	}
	findOne := options.FindOne().SetSort(mongobson.D{{Key: "$natural", Value: -1}})
	if err := client.Database("local").Collection("oplog.rs").FindOne(ctx, mongobson.D{}, findOne).Decode(&boundary); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Database("history_test").Collection("events").InsertOne(ctx, mongobson.D{{Key: "value", Value: 1}}); err != nil {
		t.Fatal(err)
	}
	checkpoint := control.OpTime{Seconds: boundary.Timestamp.T, Increment: boundary.Timestamp.I, Term: boundary.Term}
	for _, useExhaust := range []bool{false, true} {
		name := "ordinary_getMore"
		if useExhaust {
			name = "exhaust_getMore"
		}
		t.Run(name, func(t *testing.T) {
			manager := liveFetcherManager(t, mongoAddress, checkpoint)
			buffer, err := NewBuffer(BufferLimits{Entries: 100, Bytes: 4 * 1024 * 1024})
			if err != nil {
				t.Fatal(err)
			}
			fetcher, err := NewFetcher(manager, buffer, nil)
			if err != nil {
				t.Fatal(err)
			}
			fetcher.exhaust = useExhaust
			fetchContext, fetchCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer fetchCancel()
			_ = fetcher.Fetch(fetchContext)
			entries := buffer.Drain(100, 4*1024*1024)
			if len(entries) == 0 {
				t.Fatal("live oplog fetch returned no entries after the continuity boundary")
			}
			for _, entry := range entries {
				if entry.OpTime.Compare(checkpoint) <= 0 {
					t.Fatalf("buffer contains inclusive continuity entry: %+v", entry.OpTime)
				}
			}
		})
	}
}

func liveFetcherManager(t *testing.T, source string, checkpoint control.OpTime) *topology.Manager {
	t.Helper()
	_, store := testutil.NewControlStore(t, control.Configuration{
		SetName: "rs0", MemberHost: "dumbo.example:27017",
	})
	manager := topology.New(store)
	if err := manager.InstallConfiguration(control.ReplicaConfiguration{
		SetName: "rs0", Version: 1, Term: checkpoint.Term, ProtocolVersion: 1, ReplicaSetID: "live-test",
		Members: []control.MemberConfiguration{
			{MemberID: 0, Host: source, Priority: 1, Votes: 1},
			{MemberID: 3, Host: "dumbo.example:27017", Hidden: true, Priority: 0, Votes: 0},
		},
	}, checkpoint.Term); err != nil {
		t.Fatal(err)
	}
	if err := manager.ObserveHeartbeat(source, topology.Heartbeat{
		SetName: "rs0", MemberID: 0, State: topology.StatePrimary, Term: checkpoint.Term, PrimaryID: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkInitialSyncComplete(control.Checkpoint{
		Fetched: checkpoint, Buffered: checkpoint, Written: checkpoint, Durable: checkpoint, Applied: checkpoint,
	}); err != nil {
		t.Fatal(err)
	}
	return manager
}

func freeOplogTestAddress(t *testing.T) string {
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

func waitForOplogTestPort(t *testing.T, address string) {
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

func waitForOplogPrimary(t *testing.T, ctx context.Context, admin *mongo.Database) {
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
