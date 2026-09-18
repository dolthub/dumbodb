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
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/testutil"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/state"
)

func TestReplicationInspectionCommands(t *testing.T) {
	handler := configuredReplicationHandler(t)
	configResponse, err := handler.MsgReplSetGetConfig(context.Background(), wire.MustOpMsg("replSetGetConfig", int32(1), "$db", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	configDocument, err := opMsgDocument(configResponse)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := responseValue(configDocument, "config").(*types.Document); !ok {
		t.Fatalf("config = %T", responseValue(configDocument, "config"))
	}
	if responseValue(configDocument, "commitmentStatus") != nil {
		t.Fatal("replSetGetConfig returned commitmentStatus")
	}

	rbidResponse, err := handler.MsgReplSetGetRBID(context.Background(), wire.MustOpMsg("replSetGetRBID", int32(1), "$db", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	rbidDocument, err := opMsgDocument(rbidResponse)
	if err != nil {
		t.Fatal(err)
	}
	if rbid := responseValue(rbidDocument, "rbid"); rbid != int64(0) {
		t.Fatalf("rbid = %v", rbid)
	}

	isSelf, err := handler.MsgIsSelf(context.Background(), wire.MustOpMsg("_isSelf", int32(1), "$db", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	isSelfDocument, err := opMsgDocument(isSelf)
	if err != nil {
		t.Fatal(err)
	}
	if id := responseValue(isSelfDocument, "id"); id != handler.processID {
		t.Fatalf("_isSelf id = %v, want %v", id, handler.processID)
	}

	statusResponse, err := handler.MsgReplSetGetStatus(context.Background(), wire.MustOpMsg("replSetGetStatus", int32(1), "$db", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	statusDocument, err := opMsgDocument(statusResponse)
	if err != nil {
		t.Fatal(err)
	}
	members, ok := responseValue(statusDocument, "members").(*types.Array)
	if !ok || members.Len() != 2 {
		t.Fatalf("status members = %T %v", responseValue(statusDocument, "members"), responseValue(statusDocument, "members"))
	}
}

func TestReplicationStatusReportsTerminalInitialSyncFailure(t *testing.T) {
	handler := configuredReplicationHandler(t)
	failure := control.InitialSyncFailure{
		Namespace: "archive.items",
		BSONType:  "JavaScript",
		Message:   "DumboDB does not support BSON type JavaScript while cloning archive.items",
	}
	if err := handler.ReplicationTopology.MarkInitialSyncFailed(failure); err != nil {
		t.Fatal(err)
	}
	response, err := handler.MsgReplSetGetStatus(context.Background(), wire.MustOpMsg("replSetGetStatus", int32(1), "$db", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := opMsgDocument(response)
	if err != nil {
		t.Fatal(err)
	}
	if responseValue(document, "myState") != int32(topology.StateRecovering) {
		t.Fatalf("terminal myState = %v", responseValue(document, "myState"))
	}
	status, ok := responseValue(document, "initialSyncStatus").(*types.Document)
	if !ok {
		t.Fatalf("initialSyncStatus = %T", responseValue(document, "initialSyncStatus"))
	}
	if responseValue(status, "initialSyncFailure") != failure.Message ||
		responseValue(status, "namespace") != failure.Namespace || responseValue(status, "bsonType") != failure.BSONType {
		t.Fatalf("initialSyncStatus = %v", status)
	}
	members := responseValue(document, "members").(*types.Array)
	selfValue, err := members.Get(0)
	if err != nil {
		t.Fatal(err)
	}
	self := selfValue.(*types.Document)
	if responseValue(self, "infoMessage") != failure.Message {
		t.Fatalf("self infoMessage = %v", responseValue(self, "infoMessage"))
	}
}

func TestReplicationStatusReportsCatalogFailureWithoutBSONType(t *testing.T) {
	handler := configuredReplicationHandler(t)
	failure := control.InitialSyncFailure{
		Namespace: "archive.active_items",
		Message:   "replication rejected archive.active_items option \"viewOn\": view replication is unsupported",
	}
	if err := handler.ReplicationTopology.MarkInitialSyncFailed(failure); err != nil {
		t.Fatal(err)
	}
	response, err := handler.MsgReplSetGetStatus(context.Background(), wire.MustOpMsg("replSetGetStatus", int32(1), "$db", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := opMsgDocument(response)
	if err != nil {
		t.Fatal(err)
	}
	status := responseValue(document, "initialSyncStatus").(*types.Document)
	if responseValue(status, "initialSyncFailure") != failure.Message || responseValue(status, "namespace") != failure.Namespace {
		t.Fatalf("initialSyncStatus = %v", status)
	}
	if status.Has("bsonType") {
		t.Fatalf("catalog failure has BSON type: %v", status)
	}
}

func TestReplicationHeartbeatReturnsNewerConfigAndTracksPrimary(t *testing.T) {
	handler := configuredReplicationHandler(t)
	checkpoint := control.Checkpoint{
		Fetched:  control.OpTime{Seconds: 19, Increment: 5, Term: 9},
		Buffered: control.OpTime{Seconds: 19, Increment: 5, Term: 9},
		Written:  control.OpTime{Seconds: 17, Increment: 4, Term: 9},
		Durable:  control.OpTime{Seconds: 13, Increment: 3, Term: 9},
		Applied:  control.OpTime{Seconds: 11, Increment: 2, Term: 9},
	}
	if err := handler.ReplicationTopology.MarkInitialSyncComplete(checkpoint); err != nil {
		t.Fatal(err)
	}
	request := wire.MustOpMsg(
		"replSetHeartbeat", "rs0",
		"configVersion", int64(3),
		"configTerm", int64(3),
		"from", "primary.example:27017",
		"fromId", int32(1),
		"term", int64(9),
		"primaryId", int32(1),
		"$db", "admin",
	)
	response, err := handler.MsgReplSetHeartbeat(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	document, err := opMsgDocument(response)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := responseValue(document, "config").(*types.Document); !ok {
		t.Fatalf("heartbeat config = %T", responseValue(document, "config"))
	}
	if responseValue(document, "time") != nil {
		t.Fatal("heartbeat returned time")
	}
	wallTimes := map[string]uint32{
		"wallTime":        checkpoint.Applied.Seconds,
		"writtenWallTime": checkpoint.Written.Seconds,
		"durableWallTime": checkpoint.Durable.Seconds,
	}
	for field, expectedSeconds := range wallTimes {
		value, ok := responseValue(document, field).(time.Time)
		if !ok {
			t.Fatalf("heartbeat %s = %T, want BSON UTC datetime", field, responseValue(document, field))
		}
		if value.Unix() != int64(expectedSeconds) {
			t.Fatalf("heartbeat %s = %v, want Unix %d", field, value, expectedSeconds)
		}
	}
	if _, ok := responseValue(document, "opTime").(*types.Document); !ok {
		t.Fatalf("heartbeat opTime = %T, want document", responseValue(document, "opTime"))
	}
	if responseValue(document, "appliedOpTime") != nil || responseValue(document, "appliedWallTime") != nil {
		t.Fatal("heartbeat used non-protocol applied optime field names")
	}
	state := handler.ReplicationTopology.Snapshot()
	if state.Term != 9 || state.PrimaryID != 1 || state.PrimaryHost != "primary.example:27017" {
		t.Fatalf("heartbeat topology = %+v", state)
	}
}

func TestReplicationHeartbeatOmitsCurrentConfigurationWithoutConfigTerm(t *testing.T) {
	handler := configuredReplicationHandler(t)
	request := wire.MustOpMsg(
		"replSetHeartbeat", "rs0",
		"configVersion", int64(4),
		"from", "primary.example:27017",
		"fromId", int32(1),
		"term", int64(9),
		"primaryId", int32(1),
		"$db", "admin",
	)
	response, err := handler.MsgReplSetHeartbeat(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	document, err := opMsgDocument(response)
	if err != nil {
		t.Fatal(err)
	}
	if responseValue(document, "config") != nil {
		t.Fatal("heartbeat returned the requester's current configuration")
	}
	if responseValue(document, "time") != nil {
		t.Fatal("heartbeat returned time")
	}
}

func TestReplicationStatusReportsDurableOptimes(t *testing.T) {
	handler := configuredReplicationHandler(t)
	checkpoint := control.Checkpoint{
		Fetched:  control.OpTime{Seconds: 19, Increment: 5, Term: 9},
		Buffered: control.OpTime{Seconds: 19, Increment: 5, Term: 9},
		Written:  control.OpTime{Seconds: 17, Increment: 4, Term: 9},
		Durable:  control.OpTime{Seconds: 13, Increment: 3, Term: 9},
		Applied:  control.OpTime{Seconds: 11, Increment: 2, Term: 9},
	}
	if err := handler.ReplicationTopology.MarkInitialSyncComplete(checkpoint); err != nil {
		t.Fatal(err)
	}
	committed := control.OpTime{Seconds: 12, Increment: 7, Term: 9}
	if err := handler.ReplicationTopology.ObserveHeartbeat("primary.example:27017", topology.Heartbeat{
		SetName: "rs0", MemberID: 1, State: topology.StatePrimary, Term: 9, PrimaryID: 1,
		Applied: checkpoint.Written, Written: checkpoint.Written, Durable: checkpoint.Durable, Committed: committed,
	}); err != nil {
		t.Fatal(err)
	}
	response, err := handler.MsgReplSetGetStatus(context.Background(), wire.MustOpMsg("replSetGetStatus", int32(1), "$db", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := opMsgDocument(response)
	if err != nil {
		t.Fatal(err)
	}
	optimes, ok := responseValue(document, "optimes").(*types.Document)
	if !ok {
		t.Fatalf("optimes = %T, want document", responseValue(document, "optimes"))
	}
	expected := map[string]control.OpTime{
		"lastCommittedOpTime": committed,
		"appliedOpTime":       checkpoint.Applied,
		"durableOpTime":       checkpoint.Durable,
		"writtenOpTime":       checkpoint.Written,
	}
	for field, expectedOpTime := range expected {
		assertStatusOpTime(t, optimes, field, expectedOpTime)
	}
	expectedWallTimes := map[string]control.OpTime{
		"lastCommittedWallTime": committed,
		"lastAppliedWallTime":   checkpoint.Applied,
		"lastDurableWallTime":   checkpoint.Durable,
		"lastWrittenWallTime":   checkpoint.Written,
	}
	for field, expectedOpTime := range expectedWallTimes {
		value, ok := responseValue(optimes, field).(time.Time)
		if !ok || value.Unix() != int64(expectedOpTime.Seconds) {
			t.Fatalf("optimes.%s = %v, want Unix %d", field, value, expectedOpTime.Seconds)
		}
	}
}

func assertStatusOpTime(t *testing.T, document *types.Document, field string, expected control.OpTime) {
	t.Helper()
	value, ok := responseValue(document, field).(*types.Document)
	if !ok {
		t.Fatalf("optimes.%s = %T, want document", field, responseValue(document, field))
	}
	timestamp, ok := responseValue(value, "ts").(types.Timestamp)
	if !ok || uint64(timestamp) != uint64(expected.Seconds)<<32|uint64(expected.Increment) {
		t.Fatalf("optimes.%s.ts = %v, want %v", field, timestamp, expected)
	}
	if term := responseValue(value, "t"); term != expected.Term {
		t.Fatalf("optimes.%s.t = %v, want %d", field, term, expected.Term)
	}
}

func TestReplicationHeartbeatRejectsInvalidProtocolFields(t *testing.T) {
	handler := configuredReplicationHandler(t)
	tests := []struct {
		name  string
		field string
		value any
		code  handlererrors.ErrorCode
	}{
		{name: "config version type", field: "configVersion", value: "4", code: handlererrors.ErrTypeMismatch},
		{name: "term type", field: "term", value: float64(4), code: handlererrors.ErrTypeMismatch},
		{name: "heartbeat version", field: "hbv", value: int32(2), code: handlererrors.ErrorCode(40666)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := wire.MustOpMsg(
				"replSetHeartbeat", "rs0",
				"configVersion", int64(3),
				"from", "primary.example:27017",
				"fromId", int32(1),
				"term", int64(9),
				"$db", "admin",
			)
			document, err := opMsgDocument(request)
			if err != nil {
				t.Fatal(err)
			}
			document.Set(test.field, test.value)
			request, err = documentOpMsg(document)
			if err != nil {
				t.Fatal(err)
			}
			_, err = handler.MsgReplSetHeartbeat(context.Background(), request)
			commandError, ok := err.(*handlererrors.CommandError)
			if !ok || commandError.Code() != test.code {
				t.Fatalf("error = %v, want code %d", err, test.code)
			}
		})
	}
}

func TestReplSetUpdatePositionIsExplicitlyUnsupported(t *testing.T) {
	handler := configuredReplicationHandler(t)
	_, err := handler.MsgReplSetUpdatePositionUnsupported(context.Background(), wire.MustOpMsg("replSetUpdatePosition", int32(1), "$db", "admin"))
	commandError, ok := err.(*handlererrors.CommandError)
	if !ok || commandError.Code() != handlererrors.ErrorCode(115) {
		t.Fatalf("error = %v, want code 115", err)
	}
}

func TestServerStatusReportsMongoReplicationSections(t *testing.T) {
	handler := configuredReplicationHandler(t)
	last := control.OpTime{Seconds: 100, Increment: 2, Term: 3}
	checkpoint := control.Checkpoint{Fetched: last, Buffered: last, Written: last, Durable: last, Applied: last}
	if err := handler.ReplicationTopology.MarkInitialSyncComplete(checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := handler.ReplicationTopology.ObserveHeartbeat("primary.example:27017", topology.Heartbeat{
		SetName: "rs0", MemberID: 1, State: topology.StatePrimary, Term: 3, PrimaryID: 1,
		Applied:   control.OpTime{Seconds: 105, Increment: 1, Term: 3},
		Committed: control.OpTime{Seconds: 104, Increment: 1, Term: 3},
	}); err != nil {
		t.Fatal(err)
	}
	handler.ReplicationTopology.SetRuntimePhase("steady")
	handler.ReplicationTopology.ObserveBuffer(7, 4096, 50000, 256<<20)
	handler.ReplicationTopology.ObserveSourceOplogHead(control.OpTime{Seconds: 105, Increment: 1, Term: 3})
	handler.ReplicationTopology.RecordAppliedEntry([]string{"i"})
	handler.ReplicationTopology.RecordPublishedCommit()
	handler.ReplicationTopology.RecordFetchedOperation()
	handler.ReplicationTopology.RecordOplogReaderCreated()

	connectionInfo := conninfo.New()
	connectionInfo.SetBypassBackendAuth()
	serverContext := conninfo.Ctx(context.Background(), connectionInfo)
	serverResponse, err := handler.MsgServerStatus(serverContext, wire.MustOpMsg("serverStatus", int32(1), "$db", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	serverDocument, err := opMsgDocument(serverResponse)
	if err != nil {
		t.Fatal(err)
	}
	repl := responseValue(serverDocument, "repl").(*types.Document)
	if responseValue(repl, "setName") != "rs0" || responseValue(repl, "secondary") != true || responseValue(repl, "primary") != "primary.example:27017" {
		t.Fatalf("serverStatus repl = %v", repl)
	}
	metrics := responseValue(responseValue(serverDocument, "metrics").(*types.Document), "repl").(*types.Document)
	apply := responseValue(metrics, "apply").(*types.Document)
	if responseValue(apply, "ops") != int64(1) {
		t.Fatalf("serverStatus metrics.repl.apply = %v", apply)
	}
	buffer := responseValue(responseValue(metrics, "buffer").(*types.Document), "apply").(*types.Document)
	if responseValue(buffer, "count") != int64(7) || responseValue(buffer, "sizeBytes") != int64(4096) {
		t.Fatalf("serverStatus metrics.repl.buffer.apply = %v", buffer)
	}
	network := responseValue(metrics, "network").(*types.Document)
	if responseValue(network, "ops") != int64(1) || responseValue(network, "readersCreated") != int64(1) {
		t.Fatalf("serverStatus metrics.repl.network = %v", network)
	}
	opcounters := responseValue(serverDocument, "opcountersRepl").(*types.Document)
	if responseValue(opcounters, "insert") != int64(1) {
		t.Fatalf("serverStatus opcountersRepl = %v", opcounters)
	}
	if value, _ := serverDocument.Get("replication"); value != nil {
		t.Fatalf("serverStatus retained non-MongoDB replication section: %v", value)
	}
	if _, ok := handler.Commands()["dumboReplicationStatus"]; ok {
		t.Fatal("dumboReplicationStatus remains registered")
	}
	statusResponse, err := handler.MsgReplSetGetStatus(serverContext, wire.MustOpMsg(
		"replSetGetStatus", int32(1), "$db", "admin",
	))
	if err != nil {
		t.Fatal(err)
	}
	statusDocument, err := opMsgDocument(statusResponse)
	if err != nil {
		t.Fatal(err)
	}
	if responseValue(statusDocument, "myState") != int32(topology.StateSecondary) {
		t.Fatalf("replSetGetStatus myState = %v", responseValue(statusDocument, "myState"))
	}

	filteredResponse, err := handler.MsgServerStatus(serverContext, wire.MustOpMsg(
		"serverStatus", int32(1), "repl", int32(0), "metrics", false,
		"opcountersRepl", int64(0), "$db", "admin",
	))
	if err != nil {
		t.Fatal(err)
	}
	filteredDocument, err := opMsgDocument(filteredResponse)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"repl", "metrics", "opcountersRepl"} {
		if value, _ := filteredDocument.Get(section); value != nil {
			t.Fatalf("excluded serverStatus section %s = %v", section, value)
		}
	}
}

func configuredReplicationHandler(t *testing.T) *Handler {
	t.Helper()
	backend, store := testutil.NewControlStore(t, control.Configuration{
		SetName: "rs0", MemberHost: "dumbo.example:27017",
	})
	manager := topology.New(store)
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
	return &Handler{
		NewOpts: &NewOpts{
			Backend:             backend,
			ReplSetName:         "rs0",
			ReplicationTopology: manager,
			StateProvider:       state.NewProvider(),
			L:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		b:               backend,
		processID:       types.NewObjectID(),
		topologyChanged: make(chan struct{}),
	}
}
