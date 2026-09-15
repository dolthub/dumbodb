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

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/types"
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
}

func TestReplicationHeartbeatReturnsNewerConfigAndTracksPrimary(t *testing.T) {
	handler := configuredReplicationHandler(t)
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
	state := handler.ReplicationTopology.Snapshot()
	if state.Term != 9 || state.PrimaryID != 1 || state.PrimaryHost != "primary.example:27017" {
		t.Fatalf("heartbeat topology = %+v", state)
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

func configuredReplicationHandler(t *testing.T) *Handler {
	t.Helper()
	store, err := control.Open(t.TempDir(), control.Configuration{
		SetName: "rs0", Branch: "mongo", MemberHost: "dumbo.example:27017",
	})
	if err != nil {
		t.Fatal(err)
	}
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
			ReplSetName:         "rs0",
			ReplicationTopology: manager,
		},
		processID:       types.NewObjectID(),
		topologyChanged: make(chan struct{}),
	}
}
