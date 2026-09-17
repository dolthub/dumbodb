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
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/handler/common"
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
	persisted := h.ReplicationTopology.ControlSnapshot()
	infoMessage := ""
	if state.InitialSyncFailure != nil {
		infoMessage = state.InitialSyncFailure.Message
	} else if len(persisted.FailureHistory) != 0 {
		infoMessage = persisted.FailureHistory[len(persisted.FailureHistory)-1].Message
	}
	members := types.MakeArray(len(state.Members) + 1)
	members.Append(replicaStatusMember(state.MemberID, state.MemberHost, state.State, true, true, state.Checkpoint.Applied, time.Now(), infoMessage))
	memberIDs := make([]int, 0, len(state.Members))
	for memberID := range state.Members {
		if memberID != state.MemberID {
			memberIDs = append(memberIDs, memberID)
		}
	}
	sort.Ints(memberIDs)
	for _, memberID := range memberIDs {
		member := state.Members[memberID]
		members.Append(replicaStatusMember(member.MemberID, member.Host, member.State, false, member.Healthy, member.Applied, member.LastHeartbeat, ""))
	}
	response := must.NotFail(types.NewDocument(
		"set", state.SetName,
		"date", time.Now(),
		"myState", int32(state.State),
		"term", state.Term,
		"syncSourceHost", state.SyncSource,
		"syncSourceId", memberIDForHost(state, state.SyncSource),
		"heartbeatIntervalMillis", int64(2000),
		"optimes", replicaStatusOpTimes(state),
		"members", members,
		"ok", float64(1),
	))
	if persisted.InitialSyncPhase != control.InitialSyncComplete {
		initialSync := must.NotFail(types.NewDocument(
			"phase", string(persisted.InitialSyncPhase),
			"failedInitialSyncAttempts", initialSyncFailureCount(persisted.FailureHistory),
			"maxFailedInitialSyncAttempts", int32(1),
		))
		if state.InitialSyncFailure != nil {
			initialSync.Set("initialSyncFailure", state.InitialSyncFailure.Message)
			initialSync.Set("namespace", state.InitialSyncFailure.Namespace)
			initialSync.Set("bsonType", state.InitialSyncFailure.BSONType)
		}
		if persisted.InitialSyncAttempt != nil {
			initialSync.Set("beginFetchOpTime", opTimeDocument(persisted.InitialSyncAttempt.BeginFetch))
			initialSync.Set("beginApplyOpTime", opTimeDocument(persisted.InitialSyncAttempt.BeginApply))
			initialSync.Set("stopOpTime", opTimeDocument(persisted.InitialSyncAttempt.Stop))
		}
		initialSync.Set("collectionsTotal", int64(state.Runtime.InitialSyncCollectionsTotal))
		initialSync.Set("collectionsCompleted", int64(state.Runtime.InitialSyncCollectionsCompleted))
		initialSync.Set("documentsCopied", state.Runtime.InitialSyncDocuments)
		if state.Runtime.InitialSyncCurrentNamespace != "" {
			initialSync.Set("currentNamespace", state.Runtime.InitialSyncCurrentNamespace)
		}
		response.Set("initialSyncStatus", initialSync)
	}
	return documentOpMsg(response)
}

func initialSyncFailureCount(history []control.ReplicationFailure) int32 {
	var count int32
	for _, failure := range history {
		if failure.Stage == "initial_sync" {
			count++
		}
	}
	return count
}

func (h *Handler) MsgDumboReplicationDetach(_ context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}
	if err := common.RejectUnknownFields(document); err != nil {
		return nil, err
	}
	if err := requireAdminReplicationCommand(msg); err != nil {
		return nil, err
	}
	if h.ReplicationTopology == nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrorCode(76), "not running with --replSet")
	}
	if err := h.ReplicationTopology.Detach(); err != nil {
		return nil, lazyerrors.Error(err)
	}
	return documentOpMsg(must.NotFail(types.NewDocument("state", "detached", "ok", float64(1))))
}

func (h *Handler) MsgDumboReplicationStatus(_ context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}
	if err := common.RejectUnknownFields(document, "sourceOpTime", "commitID", "database"); err != nil {
		return nil, err
	}
	if err := requireAdminReplicationCommand(msg); err != nil {
		return nil, err
	}
	state, err := h.replicationState()
	if err != nil {
		return nil, err
	}
	controlState := h.ReplicationTopology.ControlSnapshot()
	response := replicationStatusDocument(state, controlState)
	lookup, err := h.replicationProvenanceLookup(document)
	if err != nil {
		return nil, err
	}
	if lookup != nil {
		response.Set("provenanceLookup", lookup)
	}
	response.Set("ok", float64(1))
	return documentOpMsg(response)
}

func requireAdminReplicationCommand(msg *wire.OpMsg) error {
	command, database, _ := wireCommandTarget(msg)
	if database == "admin" {
		return nil
	}
	return handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrUnauthorized,
		fmt.Sprintf("%s may only be run against the admin database", command),
		command,
	)
}

func (h *Handler) replicationProvenanceLookup(document *types.Document) (*types.Document, error) {
	sourceValue, _ := document.Get("sourceOpTime")
	commitValue, _ := document.Get("commitID")
	databaseValue, _ := document.Get("database")
	if sourceValue != nil && commitValue != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "sourceOpTime and commitID are mutually exclusive")
	}
	if sourceValue == nil && commitValue == nil {
		if databaseValue != nil {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "database requires commitID")
		}
		return nil, nil
	}
	if sourceValue != nil {
		if databaseValue != nil {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "database is not valid with sourceOpTime")
		}
		opTime, err := statusOpTime(sourceValue)
		if err != nil {
			return nil, err
		}
		interval, found := h.ReplicationTopology.CommitFor(opTime)
		return provenanceLookupDocument(found, interval), nil
	}
	commitID, ok := commitValue.(string)
	if !ok || commitID == "" {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, "commitID must be a non-empty string")
	}
	var interval control.CommitInterval
	var found bool
	if databaseValue == nil {
		interval, found = h.ReplicationTopology.CommitForID(commitID)
	} else {
		database, ok := databaseValue.(string)
		if !ok || database == "" {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, "database must be a non-empty string")
		}
		interval, found = h.ReplicationTopology.CommitForDatabaseCommit(database, commitID)
	}
	return provenanceLookupDocument(found, interval), nil
}

func statusOpTime(value any) (control.OpTime, error) {
	document, ok := value.(*types.Document)
	if !ok {
		return control.OpTime{}, handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, "sourceOpTime must be a document")
	}
	timestampValue, _ := document.Get("ts")
	timestamp, ok := timestampValue.(types.Timestamp)
	if !ok {
		return control.OpTime{}, handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, "sourceOpTime.ts must be a timestamp")
	}
	termValue, _ := document.Get("t")
	var term int64
	switch value := termValue.(type) {
	case int32:
		term = int64(value)
	case int64:
		term = value
	default:
		return control.OpTime{}, handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, "sourceOpTime.t must be an integer")
	}
	return control.OpTime{Seconds: uint32(uint64(timestamp) >> 32), Increment: uint32(timestamp), Term: term}, nil
}

func provenanceLookupDocument(found bool, interval control.CommitInterval) *types.Document {
	if !found {
		return must.NotFail(types.NewDocument("found", false))
	}
	document := commitIntervalDocument(interval)
	document.Set("found", true)
	return document
}

func replicationStatusDocument(state topology.Snapshot, persisted control.State) *types.Document {
	lifecycle := string(persisted.Lifecycle)
	member := must.NotFail(types.NewDocument(
		"host", state.MemberHost,
		"memberId", int32(state.MemberID),
		"state", int32(state.State),
		"stateStr", state.State.String(),
		"lifecycle", lifecycle,
	))
	configuration := must.NotFail(types.NewDocument("installed", state.Configuration != nil))
	if state.Configuration != nil {
		configuration.Set("setName", state.Configuration.SetName)
		configuration.Set("replicaSetId", state.Configuration.ReplicaSetID)
		configuration.Set("version", state.Configuration.Version)
		configuration.Set("term", state.Configuration.Term)
	}
	source := replicationSourceDocument(state, persisted)
	positions := must.NotFail(types.NewDocument(
		"fetched", opTimeDocument(persisted.Checkpoint.Fetched),
		"buffered", opTimeDocument(persisted.Checkpoint.Buffered),
		"written", opTimeDocument(persisted.Checkpoint.Written),
		"durable", opTimeDocument(persisted.Checkpoint.Durable),
		"applied", opTimeDocument(persisted.Checkpoint.Applied),
		"committed", opTimeDocument(durableCommittedOpTime(state)),
	))
	runtime := state.Runtime
	elapsed := time.Since(runtime.StartedAt).Seconds()
	if elapsed <= 0 {
		elapsed = 1
	}
	return must.NotFail(types.NewDocument(
		"member", member,
		"configuration", configuration,
		"runtime", must.NotFail(types.NewDocument(
			"phase", runtime.Phase,
			"phaseSince", runtime.PhaseSince,
			"startedAt", runtime.StartedAt,
		)),
		"initialSync", initialSyncStatusDocument(persisted, runtime),
		"source", source,
		"positions", positions,
		"lag", replicationLagDocument(state, persisted),
		"buffer", must.NotFail(types.NewDocument(
			"entries", int64(runtime.BufferEntries),
			"bytes", runtime.BufferBytes,
			"entryLimit", int64(runtime.BufferEntryLimit),
			"byteLimit", runtime.BufferByteLimit,
		)),
		"rates", must.NotFail(types.NewDocument(
			"appliedOperations", runtime.AppliedOperations,
			"publishedCommits", runtime.PublishedCommits,
			"initialSyncDocuments", runtime.InitialSyncDocuments,
			"appliedOperationsPerSecond", float64(runtime.AppliedOperations)/elapsed,
			"publishedCommitsPerSecond", float64(runtime.PublishedCommits)/elapsed,
		)),
		"failures", replicationFailuresDocument(persisted),
		"provenance", must.NotFail(types.NewDocument(
			"generation", int64(persisted.CommitLogGeneration),
			"retainedIntervals", int64(len(persisted.CommitIntervals)),
		)),
	))
}

func replicationSourceDocument(state topology.Snapshot, persisted control.State) *types.Document {
	document := must.NotFail(types.NewDocument(
		"host", persisted.CurrentSource,
		"rbid", persisted.CurrentRBID,
		"primaryHost", state.PrimaryHost,
		"primaryId", int32(state.PrimaryID),
	))
	if source := memberStatusForHost(state, persisted.CurrentSource); source != nil && !source.LastHeartbeat.IsZero() {
		document.Set("lastHeartbeat", source.LastHeartbeat)
	}
	changes := types.MakeArray(len(persisted.SourceChanges))
	for _, change := range persisted.SourceChanges {
		changes.Append(must.NotFail(types.NewDocument(
			"at", time.UnixMilli(change.AtUnixMilli).UTC(), "previous", change.Previous, "current", change.Current,
		)))
	}
	document.Set("changes", changes)
	return document
}

func memberStatusForHost(state topology.Snapshot, host string) *topology.MemberStatus {
	for _, member := range state.Members {
		if member.Host == host {
			copy := member
			return &copy
		}
	}
	return nil
}

func initialSyncStatusDocument(state control.State, runtime topology.RuntimeStatus) *types.Document {
	document := must.NotFail(types.NewDocument("phase", string(state.InitialSyncPhase)))
	document.Set("collectionsTotal", int64(runtime.InitialSyncCollectionsTotal))
	document.Set("collectionsCompleted", int64(runtime.InitialSyncCollectionsCompleted))
	document.Set("documentsCopied", runtime.InitialSyncDocuments)
	if runtime.InitialSyncCurrentNamespace != "" {
		document.Set("currentNamespace", runtime.InitialSyncCurrentNamespace)
	}
	if state.InitialSyncAttempt != nil {
		attempt := state.InitialSyncAttempt
		document.Set("attempt", must.NotFail(types.NewDocument(
			"id", attempt.ID,
			"source", attempt.Source,
			"sourceRBID", attempt.SourceRBID,
			"beginFetch", opTimeDocument(attempt.BeginFetch),
			"beginApply", opTimeDocument(attempt.BeginApply),
			"stop", opTimeDocument(attempt.Stop),
		)))
	}
	if state.InitialSyncFailure != nil {
		document.Set("failure", must.NotFail(types.NewDocument(
			"namespace", state.InitialSyncFailure.Namespace,
			"bsonType", state.InitialSyncFailure.BSONType,
			"message", state.InitialSyncFailure.Message,
		)))
	}
	return document
}

func replicationLagDocument(state topology.Snapshot, persisted control.State) *types.Document {
	newest := state.Runtime.SourceOplogNewest
	if source := memberStatusForHost(state, persisted.CurrentSource); source != nil && source.Applied.Compare(newest) > 0 {
		newest = source.Applied
	}
	lagSeconds := nonNegativeSeconds(newest, persisted.Checkpoint.Applied)
	wallClockAge := int64(0)
	if persisted.Checkpoint.Applied.Seconds != 0 {
		wallClockAge = max(int64(0), time.Now().Unix()-int64(persisted.Checkpoint.Applied.Seconds))
	}
	document := must.NotFail(types.NewDocument(
		"oplogSeconds", lagSeconds,
		"appliedWallClockAgeSeconds", wallClockAge,
	))
	oldest := state.Runtime.SourceOplogOldest
	if oldest == (control.OpTime{}) || newest == (control.OpTime{}) {
		document.Set("sourceWindowAvailable", false)
		return document
	}
	document.Set("sourceWindowAvailable", true)
	document.Set("sourceWindowOldest", opTimeDocument(oldest))
	document.Set("sourceWindowNewest", opTimeDocument(newest))
	document.Set("retainedSourceWindowSeconds", nonNegativeSeconds(newest, oldest))
	document.Set("remainingSourceWindowSeconds", nonNegativeSeconds(persisted.Checkpoint.Written, oldest))
	return document
}

func nonNegativeSeconds(later, earlier control.OpTime) int64 {
	seconds := int64(later.Seconds) - int64(earlier.Seconds)
	return max(int64(0), seconds)
}

func replicationFailuresDocument(state control.State) *types.Document {
	history := types.MakeArray(len(state.FailureHistory))
	for _, failure := range state.FailureHistory {
		history.Append(replicationFailureDocument(failure))
	}
	document := must.NotFail(types.NewDocument("retryCount", state.RetryCount, "history", history))
	if len(state.FailureHistory) != 0 {
		document.Set("last", replicationFailureDocument(state.FailureHistory[len(state.FailureHistory)-1]))
	}
	return document
}

func replicationFailureDocument(failure control.ReplicationFailure) *types.Document {
	return must.NotFail(types.NewDocument(
		"at", time.UnixMilli(failure.AtUnixMilli).UTC(),
		"stage", failure.Stage,
		"classification", failure.Classification,
		"message", failure.Message,
		"retryable", failure.Retryable,
	))
}

func commitIntervalDocument(interval control.CommitInterval) *types.Document {
	commits := types.MakeArray(len(interval.Commits))
	for _, commit := range interval.Commits {
		commits.Append(must.NotFail(types.NewDocument("database", commit.Database, "commitID", commit.CommitID)))
	}
	return must.NotFail(types.NewDocument(
		"first", opTimeDocument(interval.First),
		"last", opTimeDocument(interval.Last),
		"commitID", interval.CommitID,
		"databaseCommits", commits,
	))
}

func replicationServerStatusDocument(state topology.Snapshot, persisted control.State) *types.Document {
	newest := state.Runtime.SourceOplogNewest
	if source := memberStatusForHost(state, persisted.CurrentSource); source != nil && source.Applied.Compare(newest) > 0 {
		newest = source.Applied
	}
	remaining := int64(0)
	if state.Runtime.SourceOplogOldest != (control.OpTime{}) {
		remaining = nonNegativeSeconds(persisted.Checkpoint.Written, state.Runtime.SourceOplogOldest)
	}
	return must.NotFail(types.NewDocument(
		"state", int32(state.State),
		"retryCount", persisted.RetryCount,
		"oplogLagSeconds", nonNegativeSeconds(newest, persisted.Checkpoint.Applied),
		"sourceWindowRemainingSeconds", remaining,
		"bufferEntries", int64(state.Runtime.BufferEntries),
		"bufferBytes", state.Runtime.BufferBytes,
		"appliedOperations", state.Runtime.AppliedOperations,
		"publishedCommits", state.Runtime.PublishedCommits,
		"initialSyncDocuments", state.Runtime.InitialSyncDocuments,
		"initialSyncCollectionsCompleted", int64(state.Runtime.InitialSyncCollectionsCompleted),
	))
}

func replicaStatusOpTimes(state topology.Snapshot) *types.Document {
	committed := durableCommittedOpTime(state)
	return must.NotFail(types.NewDocument(
		"lastCommittedOpTime", opTimeDocument(committed),
		"lastCommittedWallTime", opTimeDate(committed),
		"readConcernMajorityOpTime", opTimeDocument(committed),
		"appliedOpTime", opTimeDocument(state.Checkpoint.Applied),
		"durableOpTime", opTimeDocument(state.Checkpoint.Durable),
		"writtenOpTime", opTimeDocument(state.Checkpoint.Written),
		"lastAppliedWallTime", opTimeDate(state.Checkpoint.Applied),
		"lastDurableWallTime", opTimeDate(state.Checkpoint.Durable),
		"lastWrittenWallTime", opTimeDate(state.Checkpoint.Written),
	))
}

func durableCommittedOpTime(state topology.Snapshot) control.OpTime {
	if state.LastCommitted.Compare(state.Checkpoint.Durable) > 0 {
		return state.Checkpoint.Durable
	}
	return state.LastCommitted
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
		"opTime", opTimeDocument(state.Checkpoint.Applied),
		"wallTime", opTimeDate(state.Checkpoint.Applied),
		"writtenOpTime", opTimeDocument(state.Checkpoint.Written),
		"writtenWallTime", opTimeDate(state.Checkpoint.Written),
		"durableOpTime", opTimeDocument(state.Checkpoint.Durable),
		"durableWallTime", opTimeDate(state.Checkpoint.Durable),
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
	if len(configuration.RawBSON) != 0 {
		return must.NotFail(bson.ToDocument(wirebson.RawDocument(configuration.RawBSON)))
	}
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
	replicaSetID := any(configuration.ReplicaSetID)
	if decoded, err := hex.DecodeString(configuration.ReplicaSetID); err == nil && len(decoded) == types.ObjectIDLen {
		var objectID types.ObjectID
		copy(objectID[:], decoded)
		replicaSetID = objectID
	}
	settings := must.NotFail(types.NewDocument("replicaSetId", replicaSetID))
	return must.NotFail(types.NewDocument(
		"_id", configuration.SetName,
		"version", configuration.Version,
		"term", configuration.Term,
		"protocolVersion", configuration.ProtocolVersion,
		"members", members,
		"settings", settings,
	))
}

func replicaStatusMember(memberID int, host string, state topology.MemberState, self, healthy bool, applied control.OpTime, heartbeat time.Time, infoMessage string) *types.Document {
	document := must.NotFail(types.NewDocument(
		"_id", int32(memberID),
		"name", host,
		"health", boolFloat(healthy),
		"state", int32(state),
		"stateStr", state.String(),
		"uptime", int64(0),
		"optime", opTimeDocument(applied),
		"optimeDate", opTimeDate(applied),
	))
	if self {
		document.Set("self", true)
	} else if !heartbeat.IsZero() {
		document.Set("lastHeartbeat", heartbeat)
	}
	if infoMessage != "" {
		document.Set("infoMessage", infoMessage)
	}
	return document
}

func opTimeDocument(opTime control.OpTime) *types.Document {
	timestamp := types.Timestamp(uint64(opTime.Seconds)<<32 | uint64(opTime.Increment))
	return must.NotFail(types.NewDocument("ts", timestamp, "t", opTime.Term))
}

func opTimeDate(opTime control.OpTime) time.Time {
	return time.Unix(int64(opTime.Seconds), 0).UTC()
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
