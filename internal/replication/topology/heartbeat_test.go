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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestHeartbeatMeshBootstrapsConfigurationFromInboundContact(t *testing.T) {
	manager := New(openControlStore(t, t.TempDir()))
	if err := manager.ObserveMemberContact("primary.example:27017", 1, 0, 1); err != nil {
		t.Fatal(err)
	}
	response := testHeartbeatResponse(t, true)
	client := &fakeHeartbeatClient{response: response}
	mesh := NewHeartbeatMesh(manager, nil)
	mesh.newClient = func(host string) heartbeatClient {
		if host != "primary.example:27017" {
			t.Fatalf("heartbeat host = %q", host)
		}
		return client
	}
	mesh.poll(context.Background())

	if len(client.requests) != 1 {
		t.Fatalf("heartbeat requests = %d, want 1", len(client.requests))
	}
	request := decodeHeartbeatMessage(t, client.requests[0])
	if version := documentValue(request, "configVersion"); version != int64(-2) {
		t.Fatalf("unconfigured configVersion = %v", version)
	}
	if from := documentValue(request, "from"); from != "" {
		t.Fatalf("unconfigured from = %v", from)
	}
	state := manager.Snapshot()
	if state.Configuration == nil || state.Configuration.Version != 4 || state.Configuration.ReplicaSetID != "0102030405060708090a0b0c" {
		t.Fatalf("installed configuration = %+v", state.Configuration)
	}
	if len(state.Configuration.RawBSON) == 0 {
		t.Fatal("installed configuration did not preserve raw BSON")
	}
	if state.State != StateStartup2 || state.PrimaryHost != "primary.example:27017" || state.SyncSource != "primary.example:27017" {
		t.Fatalf("bootstrapped topology = %+v", state)
	}
}

func TestHeartbeatMeshMarksUnreachableMemberDown(t *testing.T) {
	manager := New(openControlStore(t, t.TempDir()))
	if err := manager.InstallConfiguration(testReplicaConfiguration(), 8); err != nil {
		t.Fatal(err)
	}
	if err := manager.ObserveHeartbeat("primary.example:27017", Heartbeat{
		SetName: "rs0", MemberID: 1, State: StatePrimary, Term: 8, PrimaryID: 1,
	}); err != nil {
		t.Fatal(err)
	}
	nowValues := []time.Time{time.Unix(100, 0), time.Unix(111, 0)}
	var nowMu sync.Mutex
	mesh := NewHeartbeatMesh(manager, nil)
	mesh.timeout = 10 * time.Second
	mesh.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		result := nowValues[0]
		if len(nowValues) > 1 {
			nowValues = nowValues[1:]
		}
		return result
	}
	mesh.newClient = func(string) heartbeatClient {
		return &fakeHeartbeatClient{err: errors.New("unreachable")}
	}
	mesh.poll(context.Background())
	state := manager.Snapshot()
	if member := state.Members[1]; member.Healthy || member.State != StateDown {
		t.Fatalf("failed member = %+v", member)
	}
	if state.PrimaryID != -1 || state.PrimaryHost != "" || state.SyncSource != "" {
		t.Fatalf("topology after failure = %+v", state)
	}
}

func TestParseHeartbeatResponseRejectsWrongSet(t *testing.T) {
	response := wire.MustOpMsg(
		"set", "other",
		"state", int32(StatePrimary),
		"term", int64(8),
		"ok", float64(1),
	)
	if _, err := parseHeartbeatResponse(response, "rs0", 1, time.Now()); err == nil {
		t.Fatal("parseHeartbeatResponse accepted wrong replica set")
	}
}

type fakeHeartbeatClient struct {
	requests []*wire.OpMsg
	response *wire.OpMsg
	err      error
	closed   bool
}

func (c *fakeHeartbeatClient) Request(_ context.Context, request *wire.OpMsg) (*wire.OpMsg, error) {
	c.requests = append(c.requests, request)
	return c.response, c.err
}

func (c *fakeHeartbeatClient) Close() error {
	c.closed = true
	return nil
}

func testHeartbeatResponse(t *testing.T, includeConfiguration bool) *wire.OpMsg {
	t.Helper()
	arguments := []any{
		"set", "rs0",
		"state", int32(StatePrimary),
		"term", int64(8),
		"primaryId", int32(1),
		"appliedOpTime", testWireOpTime(t, 20),
		"writtenOpTime", testWireOpTime(t, 20),
		"durableOpTime", testWireOpTime(t, 20),
	}
	if includeConfiguration {
		configuration := testConfigurationDocument(t)
		wireConfiguration, err := bson.FromDocument(configuration)
		if err != nil {
			t.Fatal(err)
		}
		arguments = append(arguments, "config", wireConfiguration)
	}
	arguments = append(arguments, "ok", float64(1))
	return wire.MustOpMsg(arguments...)
}

func testConfigurationDocument(t *testing.T) *types.Document {
	t.Helper()
	var replicaSetID types.ObjectID
	copy(replicaSetID[:], []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
	members := types.MakeArray(2)
	members.Append(must.NotFail(types.NewDocument(
		"_id", int32(1), "host", "primary.example:27017", "priority", float64(1), "votes", int32(1),
	)))
	members.Append(must.NotFail(types.NewDocument(
		"_id", int32(3), "host", "dumbo.example:27017", "hidden", true, "priority", float64(0), "votes", int32(0),
	)))
	return must.NotFail(types.NewDocument(
		"_id", "rs0",
		"version", int64(4),
		"term", int64(3),
		"protocolVersion", int64(1),
		"members", members,
		"settings", must.NotFail(types.NewDocument("replicaSetId", replicaSetID)),
	))
}

func testWireOpTime(t *testing.T, increment uint32) *wirebson.Document {
	t.Helper()
	document := wirebson.MakeDocument(2)
	if err := document.Add("ts", wirebson.Timestamp(uint64(100)<<32|uint64(increment))); err != nil {
		t.Fatal(err)
	}
	if err := document.Add("t", int64(8)); err != nil {
		t.Fatal(err)
	}
	return document
}

func decodeHeartbeatMessage(t *testing.T, message *wire.OpMsg) *types.Document {
	t.Helper()
	raw, err := message.RawDocument()
	if err != nil {
		t.Fatal(err)
	}
	document, err := bson.ToDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	return document
}
