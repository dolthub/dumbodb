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
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// liveMongod starts a single-member mongod replica set named rs0 and returns
// its address and a direct client. extraArgs are appended to mongod's flags.
func liveMongod(t *testing.T, clientOpts *options.ClientOptions, extraArgs ...string) (string, *mongo.Client) {
	t.Helper()
	mongodBinary := os.Getenv("MONGOD_BIN")
	if mongodBinary == "" {
		t.Skip("set MONGOD_BIN to run live replication tests")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	require.NoError(t, listener.Close())

	args := append([]string{
		"--replSet", "rs0", "--port", port, "--dbpath", t.TempDir(),
		"--bind_ip", "127.0.0.1", "--nounixsocket",
	}, extraArgs...)
	cmd := exec.Command(mongodBinary, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if clientOpts == nil {
		clientOpts = options.Client()
	}
	client, err := mongo.Connect(clientOpts.ApplyURI("mongodb://" + address + "/?directConnection=true"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	ctx := context.Background()
	require.NoError(t, client.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetInitiate", Value: bson.D{
		{Key: "_id", Value: "rs0"},
		{Key: "members", Value: bson.A{bson.D{{Key: "_id", Value: 0}, {Key: "host", Value: address}}}},
	}}}).Err())

	require.Eventually(t, func() bool {
		var hello bson.M
		err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)
		return err == nil && hello["isWritablePrimary"] == true
	}, 30*time.Second, 100*time.Millisecond, "mongod did not become primary")

	return address, client
}

// addDumboMember reconfigures the mongod replica set to include member as a
// hidden, non-voting member.
func addDumboMember(t *testing.T, primary *mongo.Client, member string) {
	t.Helper()
	ctx := context.Background()
	var current struct {
		Config bson.M `bson:"config"`
	}
	require.NoError(t, primary.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetGetConfig", Value: 1}}).Decode(&current))
	current.Config["version"] = current.Config["version"].(int32) + 1
	current.Config["members"] = append(current.Config["members"].(bson.A), bson.D{
		{Key: "_id", Value: 3}, {Key: "host", Value: member},
		{Key: "hidden", Value: true}, {Key: "priority", Value: 0}, {Key: "votes", Value: 0},
	})
	require.NoError(t, primary.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetReconfig", Value: current.Config}}).Err())
}

func dumboMemberState(ctx context.Context, primary *mongo.Client, member string) string {
	var status bson.M
	if err := primary.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&status); err != nil {
		return err.Error()
	}
	members, _ := status["members"].(bson.A)
	for _, m := range members {
		var mm bson.M
		raw, err := bson.Marshal(m)
		if err != nil || bson.Unmarshal(raw, &mm) != nil {
			continue
		}
		if mm["name"] == member {
			return fmt.Sprint(mm["stateStr"])
		}
	}
	return "absent"
}

// A DumboDB server added to a real mongod replica set learns its
// configuration from the primary's inbound heartbeat and its own polling, and
// replicates data.
func TestLiveReplication_DumboJoinsMongodReplicaSet(t *testing.T) {
	_, primary := liveMongod(t, nil)
	ctx := context.Background()
	_, err := primary.Database("live").Collection("c").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "v", Value: "x"}})
	require.NoError(t, err)

	env := startDumboDB(t, "--replSet", "rs0")
	member := fmt.Sprintf("127.0.0.1:%d", env.Port)
	addDumboMember(t, primary, member)

	require.Eventually(t, func() bool {
		return dumboMemberState(ctx, primary, member) == "SECONDARY"
	}, 60*time.Second, 250*time.Millisecond, "dumbo never became SECONDARY: %s", dumboMemberState(ctx, primary, member))

	direct, err := mongo.Connect(options.Client().ApplyURI(fmt.Sprintf("mongodb://%s/?directConnection=true", member)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = direct.Disconnect(context.Background()) })
	require.Eventually(t, func() bool {
		n, err := direct.Database("live").Collection("c").CountDocuments(ctx, bson.D{})
		return err == nil && n == 1
	}, 30*time.Second, 250*time.Millisecond)
}
