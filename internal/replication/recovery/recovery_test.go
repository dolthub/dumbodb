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

package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestRecoveryResetsMainToNewestSourceCommonPoint(t *testing.T) {
	ctx := context.Background()
	backend, store, manager, intervals, _ := recoveryFixture(t, ctx)
	recovery, err := New(backend, store, manager)
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedSourceClient{responses: []*wire.OpMsg{
		wire.MustOpMsg("rbid", int32(9), "ok", float64(1)),
		oplogPresenceResponse(false),
		oplogPresenceResponse(true),
	}}
	recovery.client = func(string, string) sourceClient { return client }
	if err := manager.MarkContinuityLost(true); err != nil {
		t.Fatal(err)
	}
	if err := recovery.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if count := recoveryDocumentCount(t, ctx, backend); count != 2 {
		t.Fatalf("document count after rollback = %d, want 2", count)
	}
	if retained := store.CommitIntervals(); len(retained) != 2 || retained[1].CommitID != intervals[1].CommitID {
		t.Fatalf("retained commit intervals = %+v", retained)
	}
	if state := manager.Snapshot(); state.State != topology.StateSecondary || state.Checkpoint.Applied != intervals[1].Last || state.RBID != 9 {
		t.Fatalf("topology after rollback = %+v", state)
	}
	if phase := store.Snapshot().InitialSyncPhase; phase != control.InitialSyncComplete {
		t.Fatalf("initial sync phase after rollback = %s, want complete", phase)
	}
	branches, err := backend.(backends.VersioningBackend).DumboDBBranch(ctx, &backends.BranchParams{
		DBName: "recovery", Action: "list",
	})
	if err != nil {
		t.Fatal(err)
	}
	foundAudit := false
	for _, branch := range branches.Branches {
		if branch.Name != "main" && branch.CommitID != intervals[1].Commits[0].CommitID {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Fatalf("rollback audit branch not found: %+v", branches.Branches)
	}
}

func TestRecoveryWithoutCommonPointRequiresFreshInitialSync(t *testing.T) {
	ctx := context.Background()
	backend, store, manager, intervals, _ := recoveryFixture(t, ctx)
	recovery, err := New(backend, store, manager)
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedSourceClient{responses: []*wire.OpMsg{
		wire.MustOpMsg("rbid", int32(10), "ok", float64(1)),
		oplogPresenceResponse(false),
		oplogPresenceResponse(false),
		oplogPresenceResponse(false),
	}}
	recovery.client = func(string, string) sourceClient { return client }
	if err := recovery.Run(ctx); !errors.Is(err, ErrInitialSyncRequired) {
		t.Fatalf("recovery error = %v, want initial sync required", err)
	}
	state := store.Snapshot()
	if state.InitialSyncPhase != control.InitialSyncNotStarted || len(store.CommitIntervals()) != 0 || state.Checkpoint != (control.Checkpoint{}) {
		t.Fatalf("control state after history loss = %+v", state)
	}
	branches, err := backend.(backends.VersioningBackend).DumboDBBranch(ctx, &backends.BranchParams{
		DBName: "recovery", Action: "list",
	})
	if err != nil {
		t.Fatal(err)
	}
	foundAudit := false
	for _, branch := range branches.Branches {
		if branch.Name == auditBranchName(intervals[2].Last, "r10") {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Fatalf("history-loss audit branch not found: %+v", branches.Branches)
	}
}

func TestRecoveryResumesPendingRollbackAfterRestart(t *testing.T) {
	ctx := context.Background()
	backend, store, _, intervals, _ := recoveryFixture(t, ctx)
	recoveryManager := topology.New(store)
	recovery, err := New(backend, store, recoveryManager)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := recovery.plan(ctx, "source.example:27017", 11, intervals[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginRollback(attempt); err != nil {
		t.Fatal(err)
	}
	for _, database := range attempt.Databases {
		if err := recovery.ensureAuditBranch(ctx, database.Database, attempt.AuditBranch); err != nil {
			t.Fatal(err)
		}
		if database.Database == "recovery" {
			if _, err := backend.(backends.VersioningBackend).DumboDBReset(ctx, &backends.ResetParams{
				DBName: database.Database, Branch: "main", CommitID: database.CommitID, Hard: true,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := control.Open(backend, replicationConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	restartedManager := topology.New(reopened)
	if state := restartedManager.Snapshot(); state.State != topology.StateRecovering {
		t.Fatalf("restarted topology state = %s, want RECOVERING", state.State)
	}
	restarted, err := New(backend, reopened, restartedManager)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if count := recoveryDocumentCount(t, ctx, backend); count != 2 {
		t.Fatalf("document count after resumed rollback = %d, want 2", count)
	}
	if _, ok := reopened.PendingRollback(); ok {
		t.Fatal("resumed rollback remained pending")
	}
}

func recoveryFixture(t *testing.T, ctx context.Context) (backends.Backend, *control.Store, *topology.Manager, []control.CommitInterval, string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	backend, err := dolt.NewBackend(t.TempDir(), logger, false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.Close)
	store, err := control.Open(backend, replicationConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	manager := topology.New(store)
	configuration := control.ReplicaConfiguration{
		SetName: "rs0", Version: 1, Term: 1, ProtocolVersion: 1, ReplicaSetID: "test",
		Members: []control.MemberConfiguration{
			{MemberID: 0, Host: "source.example:27017", Priority: 1, Votes: 1},
			{MemberID: 1, Host: "dumbo.example:27017", Hidden: true, Priority: 0, Votes: 0},
		},
	}
	if err := manager.InstallConfiguration(configuration, 1); err != nil {
		t.Fatal(err)
	}
	if err := manager.ObserveHeartbeat("source.example:27017", topology.Heartbeat{
		SetName: "rs0", MemberID: 0, State: topology.StatePrimary, Term: 1, PrimaryID: 0,
	}); err != nil {
		t.Fatal(err)
	}
	database, err := backend.Database("recovery")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "events"}); err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection("events")
	if err != nil {
		t.Fatal(err)
	}
	versioned := backend.(backends.VersioningBackend)
	intervals := make([]control.CommitInterval, 0, 3)
	for number := int32(1); number <= 3; number++ {
		document := must.NotFail(types.NewDocument("_id", number))
		if _, err := collection.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{document}}); err != nil {
			t.Fatal(err)
		}
		commit, err := versioned.DumboDBCommit(ctx, &backends.CommitParams{
			DBName: "recovery", Branch: "main", Message: "replication", Author: "Test <test@example.com>",
		})
		if err != nil {
			t.Fatal(err)
		}
		opTime := control.OpTime{Seconds: 100, Increment: uint32(number), Term: 1}
		interval := control.CommitInterval{
			First: opTime, Last: opTime, CommitID: fmt.Sprintf("publication-%d", number),
			Commits: []control.DatabaseCommit{{Database: "recovery", CommitID: commit.CommitID}},
		}
		checkpoint := control.Checkpoint{Fetched: opTime, Buffered: opTime, Written: opTime, Durable: opTime, Applied: opTime}
		if err := store.PublishCommit(interval, checkpoint); err != nil {
			t.Fatal(err)
		}
		intervals = append(intervals, interval)
	}
	if err := manager.MarkInitialSyncComplete(store.Snapshot().Checkpoint); err != nil {
		t.Fatal(err)
	}
	return backend, store, manager, intervals, ""
}

func replicationConfiguration() control.Configuration {
	return control.Configuration{SetName: "rs0", MemberHost: "dumbo.example:27017"}
}

func recoveryDocumentCount(t *testing.T, ctx context.Context, backend backends.Backend) int64 {
	t.Helper()
	database, err := backend.Database("recovery")
	if err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection("events")
	if err != nil {
		t.Fatal(err)
	}
	result, err := collection.Count(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result.Count
}

func oplogPresenceResponse(present bool) *wire.OpMsg {
	batch := wirebson.MakeArray(1)
	if present {
		_ = batch.Add(wirebson.MustDocument("ts", wirebson.Timestamp(1)))
	}
	cursor := wirebson.MustDocument("id", int64(0), "ns", "local.oplog.rs", "firstBatch", batch)
	return wire.MustOpMsg("cursor", cursor, "ok", float64(1))
}

type scriptedSourceClient struct {
	responses []*wire.OpMsg
}

func (c *scriptedSourceClient) Request(context.Context, *wire.OpMsg) (*wire.OpMsg, error) {
	if len(c.responses) == 0 {
		return nil, errors.New("unexpected recovery request")
	}
	response := c.responses[0]
	c.responses = c.responses[1:]
	return response, nil
}

func (c *scriptedSourceClient) Close() error {
	return nil
}
