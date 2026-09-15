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
	"fmt"
	"sort"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) MsgIsSelf(_ context.Context, _ *wire.OpMsg) (*wire.OpMsg, error) {
	return documentOpMsg(must.NotFail(types.NewDocument("id", h.processID, "ok", float64(1))))
}

func (h *Handler) MsgReplSetGetRBID(_ context.Context, _ *wire.OpMsg) (*wire.OpMsg, error) {
	state, err := h.replicationState()
	if err != nil {
		return nil, err
	}
	return documentOpMsg(must.NotFail(types.NewDocument("rbid", state.RBID, "ok", float64(1))))
}

func (h *Handler) MsgReplSetGetConfig(_ context.Context, _ *wire.OpMsg) (*wire.OpMsg, error) {
	state, err := h.replicationState()
	if err != nil {
		return nil, err
	}
	if state.Configuration == nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrorCode(94), "no replica set config has been received")
	}
	return documentOpMsg(must.NotFail(types.NewDocument(
		"config", replicaConfigurationDocument(*state.Configuration),
		"commitmentStatus", false,
		"ok", float64(1),
	)))
}

func (h *Handler) MsgReplSetGetStatus(_ context.Context, _ *wire.OpMsg) (*wire.OpMsg, error) {
	state, err := h.replicationState()
	if err != nil {
		return nil, err
	}
	members := types.MakeArray(len(state.Members) + 1)
	members.Append(replicaStatusMember(state.MemberID, state.MemberHost, state.State, true, true, state.Checkpoint.Applied, time.Now()))
	memberIDs := make([]int, 0, len(state.Members))
	for memberID := range state.Members {
		if memberID != state.MemberID {
			memberIDs = append(memberIDs, memberID)
		}
	}
	sort.Ints(memberIDs)
	for _, memberID := range memberIDs {
		member := state.Members[memberID]
		members.Append(replicaStatusMember(member.MemberID, member.Host, member.State, false, member.Healthy, member.Applied, member.LastHeartbeat))
	}
	response := must.NotFail(types.NewDocument(
		"set", state.SetName,
		"date", time.Now(),
		"myState", int32(state.State),
		"term", state.Term,
		"syncSourceHost", state.SyncSource,
		"syncSourceId", memberIDForHost(state, state.SyncSource),
		"heartbeatIntervalMillis", int64(2000),
		"members", members,
		"ok", float64(1),
	))
	return documentOpMsg(response)
}

func (h *Handler) MsgReplSetHeartbeat(_ context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	request, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}
	state, err := h.replicationState()
	if err != nil {
		return nil, err
	}
	setValue, _ := request.Get("replSetHeartbeat")
	setName, ok := setValue.(string)
	if !ok || setName != state.SetName {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrorCode(93), fmt.Sprintf("replSetHeartbeat set name %q does not match %q", setName, state.SetName))
	}
	from, _ := request.Get("from")
	fromHost, ok := from.(string)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, "replSetHeartbeat.from must be a string")
	}
	configVersion, err := requiredIntegerValue(request, "configVersion")
	if err != nil {
		return nil, err
	}
	term, err := requiredIntegerValue(request, "term")
	if err != nil {
		return nil, err
	}
	fromIDValue, err := optionalIntegerValue(request, "fromId", -1)
	if err != nil {
		return nil, err
	}
	primaryIDValue, err := optionalIntegerValue(request, "primaryId", -1)
	if err != nil {
		return nil, err
	}
	heartbeatVersion, err := optionalIntegerValue(request, "hbv", 1)
	if err != nil {
		return nil, err
	}
	if heartbeatVersion != 1 {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrorCode(40666), fmt.Sprintf("Found invalid value for field hbv: %d", heartbeatVersion))
	}
	fromID := int(fromIDValue)
	primaryID := int(primaryIDValue)
	if fromID >= 0 && fromHost != "" {
		if err := h.ReplicationTopology.ObserveMemberContact(fromHost, fromID, term, primaryID); err != nil {
			return nil, err
		}
		state = h.ReplicationTopology.Snapshot()
	}

	response := must.NotFail(types.NewDocument(
		"set", state.SetName,
		"state", int32(state.State),
		"term", state.Term,
		"v", configurationVersion(state.Configuration),
		"configTerm", configurationTerm(state.Configuration),
		"primaryId", int32(state.PrimaryID),
		"time", time.Now().Unix(),
		"appliedOpTime", opTimeDocument(state.Checkpoint.Applied),
		"writtenOpTime", opTimeDocument(state.Checkpoint.Written),
		"durableOpTime", opTimeDocument(state.Checkpoint.Durable),
		"ok", float64(1),
	))
	if state.SyncSource != "" {
		response.Set("syncingTo", state.SyncSource)
	}
	requestVersion := configVersion
	requestTerm, err := optionalIntegerValue(request, "configTerm", -1)
	if err != nil {
		return nil, err
	}
	if state.Configuration != nil && (requestTerm < state.Configuration.Term || requestTerm == state.Configuration.Term && requestVersion < state.Configuration.Version) {
		response.Set("config", replicaConfigurationDocument(*state.Configuration))
	}
	return documentOpMsg(response)
}

func (h *Handler) MsgReplSetUpdatePositionUnsupported(_ context.Context, _ *wire.OpMsg) (*wire.OpMsg, error) {
	return nil, downstreamReplicationUnsupportedError()
}

func downstreamReplicationUnsupportedError() error {
	return handlererrors.NewCommandErrorMsg(handlererrors.ErrorCode(115), "DumboDB does not serve downstream oplog replication")
}

func (h *Handler) replicationState() (topology.Snapshot, error) {
	if h.ReplicationTopology == nil {
		return topology.Snapshot{}, handlererrors.NewCommandErrorMsg(handlererrors.ErrorCode(76), "not running with --replSet")
	}
	return h.ReplicationTopology.Snapshot(), nil
}

func replicaConfigurationDocument(configuration control.ReplicaConfiguration) *types.Document {
	members := types.MakeArray(len(configuration.Members))
	for _, member := range configuration.Members {
		memberDocument := must.NotFail(types.NewDocument(
			"_id", int32(member.MemberID),
			"host", member.Host,
			"priority", member.Priority,
			"votes", int32(member.Votes),
		))
		if member.Hidden {
			memberDocument.Set("hidden", true)
		}
		members.Append(memberDocument)
	}
	settings := must.NotFail(types.NewDocument("replicaSetId", configuration.ReplicaSetID))
	return must.NotFail(types.NewDocument(
		"_id", configuration.SetName,
		"version", configuration.Version,
		"term", configuration.Term,
		"protocolVersion", configuration.ProtocolVersion,
		"members", members,
		"settings", settings,
	))
}

func replicaStatusMember(memberID int, host string, state topology.MemberState, self, healthy bool, applied control.OpTime, heartbeat time.Time) *types.Document {
	document := must.NotFail(types.NewDocument(
		"_id", int32(memberID),
		"name", host,
		"health", boolFloat(healthy),
		"state", int32(state),
		"stateStr", state.String(),
		"uptime", int64(0),
		"optime", opTimeDocument(applied),
		"optimeDate", time.Unix(int64(applied.Seconds), 0),
	))
	if self {
		document.Set("self", true)
	} else if !heartbeat.IsZero() {
		document.Set("lastHeartbeat", heartbeat)
	}
	return document
}

func opTimeDocument(opTime control.OpTime) *types.Document {
	timestamp := types.Timestamp(uint64(opTime.Seconds)<<32 | uint64(opTime.Increment))
	return must.NotFail(types.NewDocument("ts", timestamp, "t", opTime.Term))
}

func memberIDForHost(state topology.Snapshot, host string) int32 {
	if host == state.MemberHost {
		return int32(state.MemberID)
	}
	for id, member := range state.Members {
		if member.Host == host {
			return int32(id)
		}
	}
	return -1
}

func optionalIntegerValue(document *types.Document, field string, defaultValue int64) (int64, error) {
	value, _ := document.Get(field)
	if value == nil {
		return defaultValue, nil
	}
	switch value := value.(type) {
	case int32:
		return int64(value), nil
	case int64:
		return value, nil
	default:
		return 0, handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, fmt.Sprintf("%s must be an integer", field))
	}
}

func requiredIntegerValue(document *types.Document, field string) (int64, error) {
	value, _ := document.Get(field)
	if value == nil {
		return 0, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, fmt.Sprintf("missing required field %s", field))
	}
	return optionalIntegerValue(document, field, 0)
}

func configurationVersion(configuration *control.ReplicaConfiguration) int64 {
	if configuration == nil {
		return -2
	}
	return configuration.Version
}

func configurationTerm(configuration *control.ReplicaConfiguration) int64 {
	if configuration == nil {
		return -1
	}
	return configuration.Term
}

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
