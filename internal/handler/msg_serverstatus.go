// Copyright 2021 FerretDB Inc.
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
	"os"
	"path/filepath"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
	"github.com/dolthub/dumbodb/internal/version"
)

// MsgServerStatus implements `serverStatus` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgServerStatus(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	host, err := os.Hostname()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	exec, err := os.Executable()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	uptime := time.Since(h.StateProvider.Get().Start)

	commandMetrics := types.MakeDocument(0)
	metrics := must.NotFail(types.NewDocument("commands", commandMetrics))

	stats, err := h.b.Status(connCtx, new(backends.StatusParams))
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	res := must.NotFail(types.NewDocument(
		"host", host,
		"version", version.Get().MongoDBVersion,
		"process", filepath.Base(exec),
		"pid", int64(os.Getpid()),
		"uptime", uptime.Seconds(),
		"uptimeMillis", uptime.Milliseconds(),
		"uptimeEstimate", int64(uptime.Seconds()),
		"localTime", time.Now(),
		"metrics", metrics,
		"catalogStats", must.NotFail(types.NewDocument(
			"collections", int32(stats.CountCollections),
			"capped", stats.CountCappedCollections,
			"clustered", int32(0),
			"timeseries", int32(0),
			"views", int32(0),
			"internalCollections", int32(0),
			"internalViews", int32(0),
		)),
		"ok", float64(1),
	))
	if h.ReplicationTopology != nil {
		snapshot := h.ReplicationTopology.Snapshot()
		persisted := h.ReplicationTopology.ControlSnapshot()
		res.Set("repl", h.replicationServerStatusRepl(snapshot))
		metrics.Set("repl", replicationServerStatusMetrics(snapshot, persisted))
		res.Set("opcountersRepl", replicationOperationCounters(snapshot.Runtime))
	}

	// Honor section include/exclude filters: {serverStatus: 1, <section>: 0}
	// omits that section, matching MongoDB. Only the sub-document sections are
	// excludable; the scalar top-level fields are always present.
	for _, section := range []string{"metrics", "catalogStats", "repl", "opcountersRepl"} {
		if serverStatusSectionExcluded(document, section) {
			res.Remove(section)
		}
	}

	return documentOpMsg(
		res,
	)
}

func (h *Handler) replicationServerStatusRepl(state topology.Snapshot) *types.Document {
	document := must.NotFail(types.NewDocument(
		"topologyVersion", h.topologyVersionDocument(),
		"isWritablePrimary", false,
		"secondary", state.State == topology.StateSecondary,
		"primaryOnlyServices", types.MakeDocument(0),
		"rbid", state.RBID,
		"userWriteBlockMode", int32(1),
		"userWriteBlockReason", int32(0),
		"userWriteBlockModeCounters", must.NotFail(types.NewDocument(
			"Unspecified", int64(0),
			"ClusterToClusterMigrationInProgress", int64(0),
			"DiskUseThresholdExceeded", int64(0),
		)),
	))
	if state.Configuration == nil {
		document.Set("isreplicaset", true)
		document.Set("info", "Does not have a valid replica set config")
		return document
	}
	document.Set("setName", state.SetName)
	document.Set("me", state.MemberHost)
	appendReplicaSetHello(document, state)
	return document
}

func replicationServerStatusMetrics(state topology.Snapshot, persisted control.State) *types.Document {
	runtime := state.Runtime
	applyBuffer := must.NotFail(types.NewDocument(
		"count", int64(runtime.BufferEntries),
		"maxCount", int64(runtime.BufferEntryLimit),
		"maxSizeBytes", runtime.BufferByteLimit,
		"sizeBytes", runtime.BufferBytes,
	))
	writeBuffer := must.NotFail(types.NewDocument(
		"count", int64(0),
		"maxSizeBytes", int64(0),
		"sizeBytes", int64(0),
	))
	failedInitialSyncAttempts := int64(initialSyncFailureCount(persisted.FailureHistory))
	completedInitialSyncs := int64(0)
	if persisted.InitialSyncPhase == control.InitialSyncComplete {
		completedInitialSyncs = 1
	}
	return must.NotFail(types.NewDocument(
		"apply", must.NotFail(types.NewDocument(
			"batchSize", runtime.AppliedOperations,
			"batches", must.NotFail(types.NewDocument(
				"num", runtime.PublishedCommits,
				"totalMillis", int64(0),
			)),
			"ops", runtime.AppliedOperations,
		)),
		"buffer", must.NotFail(types.NewDocument(
			"apply", applyBuffer,
			"write", writeBuffer,
			"count", int64(runtime.BufferEntries),
			"maxSizeBytes", runtime.BufferByteLimit,
			"sizeBytes", runtime.BufferBytes,
		)),
		"initialSync", must.NotFail(types.NewDocument(
			"completed", completedInitialSyncs,
			"failedAttempts", failedInitialSyncAttempts,
			"failures", failedInitialSyncAttempts,
		)),
		"network", must.NotFail(types.NewDocument(
			"ops", runtime.FetchedOperations,
			"readersCreated", runtime.OplogReadersCreated,
			"oplogFetcherHighestFetchedOptime", opTimeDocument(persisted.Checkpoint.Fetched),
			"oplogFetcherLagSeconds", opTimeLagSeconds(runtime.SourceOplogNewest, persisted.Checkpoint.Fetched),
		)),
		"syncSource", must.NotFail(types.NewDocument(
			"numSelections", runtime.SourceSelections,
			"numTimesChoseSame", runtime.SourceSelectionsSame,
			"numTimesChoseDifferent", runtime.SourceSelectionsDifferent,
			"numTimesCouldNotFind", runtime.SourceSelectionsUnavailable,
		)),
		"write", must.NotFail(types.NewDocument(
			"batchSize", runtime.AppliedOperations,
			"batches", must.NotFail(types.NewDocument(
				"num", runtime.PublishedCommits,
				"totalMillis", int64(0),
			)),
		)),
	))
}

func replicationOperationCounters(runtime topology.RuntimeStatus) *types.Document {
	return must.NotFail(types.NewDocument(
		"insert", runtime.ReplicatedInserts,
		"query", int64(0),
		"update", runtime.ReplicatedUpdates,
		"delete", runtime.ReplicatedDeletes,
		"getmore", int64(0),
		"command", runtime.ReplicatedCommands,
	))
}

func opTimeLagSeconds(newest, fetched control.OpTime) int64 {
	if newest.Seconds <= fetched.Seconds {
		return 0
	}
	return int64(newest.Seconds - fetched.Seconds)
}

// serverStatusSectionExcluded reports whether the serverStatus command
// requested that section be excluded, i.e. {<section>: 0} or {<section>:
// false}. A missing or truthy value leaves the section included.
func serverStatusSectionExcluded(document *types.Document, section string) bool {
	v, _ := document.Get(section)
	switch x := v.(type) {
	case bool:
		return !x
	case int32:
		return x == 0
	case int64:
		return x == 0
	case float64:
		return x == 0
	default:
		return false
	}
}
