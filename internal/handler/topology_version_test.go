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

package handler

import (
	"context"
	"testing"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/replication/control"
	repltopology "github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestAwaitTopologyVersionReturnsOnChange(t *testing.T) {
	handler := testTopologyHandler()
	request := awaitableHelloRequest(handler.processID, 0, 10_000)
	result := make(chan error, 1)
	go func() {
		result <- handler.awaitTopologyVersion(context.Background(), request)
	}()

	select {
	case err := <-result:
		t.Fatalf("awaitTopologyVersion returned before a change: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	handler.BumpTopologyVersion()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("awaitTopologyVersion did not return after a change")
	}
}

func TestAwaitTopologyVersionReturnsImmediatelyForStaleOrDifferentProcess(t *testing.T) {
	handler := testTopologyHandler()
	handler.BumpTopologyVersion()
	for _, request := range []*types.Document{
		awaitableHelloRequest(handler.processID, 0, 10_000),
		awaitableHelloRequest(types.NewObjectID(), 1, 10_000),
	} {
		started := time.Now()
		if err := handler.awaitTopologyVersion(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
			t.Fatalf("stale topology request waited %s", elapsed)
		}
	}
}

func TestAwaitTopologyVersionRejectsFutureCounter(t *testing.T) {
	handler := testTopologyHandler()
	err := handler.awaitTopologyVersion(context.Background(), awaitableHelloRequest(handler.processID, 1, 0))
	commandError, ok := err.(*handlererrors.CommandError)
	if !ok || commandError.Code() != handlererrors.ErrorCode(31382) {
		t.Fatalf("error = %v, want code 31382", err)
	}
}

func TestReplicaSetHelloReportsSecondaryState(t *testing.T) {
	handler := testTopologyHandler()
	response, err := handler.hello(
		context.Background(),
		must.NotFail(types.NewDocument("hello", int32(1), "$db", "admin")),
		"dumbo.example:27017",
		"rs0",
	)
	if err != nil {
		t.Fatal(err)
	}
	if responseValue(response, "isWritablePrimary") != false || responseValue(response, "secondary") != true {
		t.Fatalf("hello primary/secondary = %v/%v", responseValue(response, "isWritablePrimary"), responseValue(response, "secondary"))
	}
	if responseValue(response, "setName") != "rs0" || responseValue(response, "me") != "dumbo.example:27017" {
		t.Fatalf("hello setName/me = %v/%v", responseValue(response, "setName"), responseValue(response, "me"))
	}
	if _, ok := responseValue(response, "topologyVersion").(*types.Document); !ok {
		t.Fatalf("topologyVersion = %T", responseValue(response, "topologyVersion"))
	}
}

func TestExhaustHelloRequiresAwaitableFields(t *testing.T) {
	message := wire.MustOpMsg("hello", int32(1), "$db", "admin")
	message.Flags = wire.OpMsgFlags(wire.OpMsgExhaustAllowed)
	request := must.NotFail(types.NewDocument("hello", int32(1), "$db", "admin"))
	if err := validateExhaustHello(message, request); err == nil {
		t.Fatal("exhaust hello without awaitable fields succeeded")
	}
}

func TestReplicaSetHelloDoesNotReportSecondaryBeforeInitialSync(t *testing.T) {
	controlStore, err := control.Open(t.TempDir(), control.Configuration{
		SetName: "rs0", MemberHost: "dumbo.example:27017",
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := repltopology.New(controlStore)
	handler := testTopologyHandler()
	handler.ReplicationTopology = manager
	configuration := control.ReplicaConfiguration{
		SetName: "rs0", Version: 4, Term: 3, ProtocolVersion: 1, ReplicaSetID: "set-id",
		Members: []control.MemberConfiguration{
			{MemberID: 1, Host: "primary.example:27017", Priority: 1, Votes: 1},
			{MemberID: 3, Host: "dumbo.example:27017", Hidden: true, Priority: 0, Votes: 0},
		},
	}
	if err := manager.InstallConfiguration(configuration, 3); err != nil {
		t.Fatal(err)
	}
	request := must.NotFail(types.NewDocument("hello", int32(1), "$db", "admin"))
	response, err := handler.hello(context.Background(), request, "dumbo.example:27017", "rs0")
	if err != nil {
		t.Fatal(err)
	}
	if responseValue(response, "secondary") != false {
		t.Fatalf("secondary before initial sync = %v", responseValue(response, "secondary"))
	}
	if responseValue(response, "isWritablePrimary") != false {
		t.Fatalf("isWritablePrimary before initial sync = %v", responseValue(response, "isWritablePrimary"))
	}
	if responseValue(response, "setVersion") != int64(4) {
		t.Fatalf("setVersion = %v", responseValue(response, "setVersion"))
	}
	checkpoint := control.Checkpoint{}
	if err := manager.MarkInitialSyncComplete(checkpoint); err != nil {
		t.Fatal(err)
	}
	response, err = handler.hello(context.Background(), request, "dumbo.example:27017", "rs0")
	if err != nil {
		t.Fatal(err)
	}
	if responseValue(response, "secondary") != true {
		t.Fatalf("secondary after initial sync = %v", responseValue(response, "secondary"))
	}
	if responseValue(response, "isWritablePrimary") != false {
		t.Fatalf("isWritablePrimary after initial sync = %v", responseValue(response, "isWritablePrimary"))
	}
}

func TestReplicaSetLegacyHelloNeverReportsPrimary(t *testing.T) {
	handler := testTopologyHandler()
	response, err := handler.hello(
		context.Background(),
		must.NotFail(types.NewDocument("ismaster", int32(1), "$db", "admin")),
		"dumbo.example:27017",
		"rs0",
	)
	if err != nil {
		t.Fatal(err)
	}
	if responseValue(response, "ismaster") != false {
		t.Fatalf("replica-set ismaster = %v", responseValue(response, "ismaster"))
	}
}

func TestStandaloneHelloReportsWritablePrimary(t *testing.T) {
	handler := testTopologyHandler()
	response, err := handler.hello(
		context.Background(),
		must.NotFail(types.NewDocument("hello", int32(1), "$db", "admin")),
		"dumbo.example:27017",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if responseValue(response, "isWritablePrimary") != true {
		t.Fatalf("standalone isWritablePrimary = %v", responseValue(response, "isWritablePrimary"))
	}
}

func testTopologyHandler() *Handler {
	return &Handler{
		NewOpts:         &NewOpts{MaxBsonObjectSizeBytes: types.MaxDocumentLen},
		processID:       types.NewObjectID(),
		topologyChanged: make(chan struct{}),
	}
}

func awaitableHelloRequest(processID types.ObjectID, counter, maxAwaitTimeMS int64) *types.Document {
	return must.NotFail(types.NewDocument(
		"hello", int32(1),
		"topologyVersion", topologyVersionDocument(topologyVersion{processID: processID, counter: counter}),
		"maxAwaitTimeMS", maxAwaitTimeMS,
		"$db", "admin",
	))
}

func responseValue(document *types.Document, field string) any {
	value, _ := document.Get(field)
	return value
}
