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
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/replication/control"
)

var topologyControlBackends = struct {
	sync.Mutex
	byDirectory map[string]backends.Backend
}{byDirectory: make(map[string]backends.Backend)}

func TestManagerInstallsConfigurationAndRecoversState(t *testing.T) {
	dir := t.TempDir()
	store := openControlStore(t, dir)
	manager := New(store)
	changes := 0
	manager.SetChangeListener(func() { changes++ })
	configuration := testReplicaConfiguration()
	if err := manager.InstallConfiguration(configuration, 8); err != nil {
		t.Fatal(err)
	}
	state := manager.Snapshot()
	if state.State != StateStartup2 || state.MemberID != 3 || state.Term != 8 || state.Configuration.Version != 4 {
		t.Fatalf("installed state = %+v", state)
	}
	if changes != 1 {
		t.Fatalf("topology changes = %d, want 1", changes)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openControlStore(t, dir)
	recovered := New(reopened).Snapshot()
	if recovered.State != StateStartup2 || recovered.MemberID != 3 || recovered.Configuration == nil || recovered.Configuration.ReplicaSetID != "set-id" {
		t.Fatalf("recovered state = %+v", recovered)
	}
}

func TestManagerTracksPrimaryAndReplacesSource(t *testing.T) {
	store := openControlStore(t, t.TempDir())
	manager := New(store)
	configuration := testReplicaConfiguration()
	if err := manager.InstallConfiguration(configuration, 8); err != nil {
		t.Fatal(err)
	}
	observedAt := time.Unix(100, 0)
	if err := manager.ObserveHeartbeat("secondary.example:27017", Heartbeat{
		SetName:    "rs0",
		MemberID:   2,
		State:      StateSecondary,
		Term:       8,
		PrimaryID:  -1,
		Applied:    testOpTime(20),
		ObservedAt: observedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.ObserveHeartbeat("primary.example:27017", Heartbeat{
		SetName:    "rs0",
		MemberID:   1,
		State:      StatePrimary,
		Term:       9,
		PrimaryID:  1,
		Applied:    testOpTime(22),
		Committed:  testOpTime(18),
		ObservedAt: observedAt,
	}); err != nil {
		t.Fatal(err)
	}
	state := manager.Snapshot()
	if state.PrimaryHost != "primary.example:27017" || state.SyncSource != "primary.example:27017" || state.Term != 9 || state.LastCommitted != testOpTime(18) {
		t.Fatalf("primary state = %+v", state)
	}
	if err := manager.ObserveSourceRBID("primary.example:27017", 12); err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkMemberDown(1); err != nil {
		t.Fatal(err)
	}
	state = manager.Snapshot()
	if state.PrimaryID != -1 || state.PrimaryHost != "" || state.SyncSource != "secondary.example:27017" || state.RBID != 0 || state.LastCommitted != (control.OpTime{}) {
		t.Fatalf("replacement state = %+v", state)
	}
	if err := manager.ObserveSourceRBID("secondary.example:27017", 7); err != nil {
		t.Fatal(err)
	}
	if err := manager.ObserveSourceRBID("secondary.example:27017", 8); !errors.Is(err, ErrSourceRollbackIDChanged) {
		t.Fatalf("rollback ID change error = %v", err)
	}
	if persisted := store.Snapshot(); persisted.CurrentSource != "secondary.example:27017" {
		t.Fatalf("persisted source = %q", persisted.CurrentSource)
	}
}

func TestManagerTracksInboundMemberContact(t *testing.T) {
	store := openControlStore(t, t.TempDir())
	manager := New(store)
	if err := manager.InstallConfiguration(testReplicaConfiguration(), 8); err != nil {
		t.Fatal(err)
	}
	if err := manager.ObserveMemberContact("secondary.example:27017", 2, 9, 1); err != nil {
		t.Fatal(err)
	}
	state := manager.Snapshot()
	member := state.Members[2]
	if !member.Healthy || member.State != StateUnknown || member.Host != "secondary.example:27017" {
		t.Fatalf("member contact = %+v", member)
	}
	if state.Term != 9 || state.PrimaryID != 1 || state.PrimaryHost != "primary.example:27017" {
		t.Fatalf("topology after contact = %+v", state)
	}
}

func TestManagerTransitionsToSecondaryAfterInitialSync(t *testing.T) {
	dir := t.TempDir()
	store := openControlStore(t, dir)
	manager := New(store)
	if err := manager.InstallConfiguration(testReplicaConfiguration(), 8); err != nil {
		t.Fatal(err)
	}
	checkpoint := control.Checkpoint{
		Fetched:  testOpTime(10),
		Buffered: testOpTime(10),
		Written:  testOpTime(10),
		Durable:  testOpTime(10),
		Applied:  testOpTime(10),
	}
	if err := manager.MarkInitialSyncComplete(checkpoint); err != nil {
		t.Fatal(err)
	}
	if state := manager.Snapshot(); state.State != StateSecondary || state.Checkpoint != checkpoint {
		t.Fatalf("secondary state = %+v", state)
	}
	if err := manager.MarkContinuityLost(false); err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkSteady(); err != nil {
		t.Fatal(err)
	}
	if state := manager.Snapshot(); state.State != StateSecondary {
		t.Fatalf("restored steady state = %+v", state)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if recovered := New(openControlStore(t, dir)).Snapshot(); recovered.State != StateSecondary || recovered.Checkpoint != checkpoint {
		t.Fatalf("recovered secondary state = %+v", recovered)
	}
}

func TestManagerPersistsTerminalInitialSyncFailure(t *testing.T) {
	dir := t.TempDir()
	store := openControlStore(t, dir)
	manager := New(store)
	if err := manager.InstallConfiguration(testReplicaConfiguration(), 8); err != nil {
		t.Fatal(err)
	}
	failure := control.InitialSyncFailure{
		Namespace: "archive.items",
		BSONType:  "JavaScript",
		Message:   "DumboDB does not support BSON type JavaScript while cloning archive.items",
	}
	if err := manager.MarkInitialSyncFailed(failure); err != nil {
		t.Fatal(err)
	}
	state := manager.Snapshot()
	if state.State != StateRecovering || state.InitialSyncFailure == nil || *state.InitialSyncFailure != failure {
		t.Fatalf("failed topology state = %+v", state)
	}
	state.InitialSyncFailure.Message = "changed"
	if manager.Snapshot().InitialSyncFailure.Message != failure.Message {
		t.Fatal("Snapshot returned mutable initial-sync failure state")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered := New(openControlStore(t, dir)).Snapshot()
	if recovered.State != StateRecovering || recovered.InitialSyncFailure == nil || *recovered.InitialSyncFailure != failure {
		t.Fatalf("recovered failed topology state = %+v", recovered)
	}
}

func TestManagerPublishesCommitBeforeNotifyingProgress(t *testing.T) {
	dir := t.TempDir()
	store := openControlStore(t, dir)
	manager := New(store)
	fetched := testOpTime(8)
	if err := manager.AdvanceFetched(fetched, fetched); err != nil {
		t.Fatal(err)
	}
	interval := control.CommitInterval{First: testOpTime(1), Last: testOpTime(5), CommitID: "commit-five"}
	checkpoint := control.Checkpoint{
		Fetched: fetched, Buffered: fetched, Written: interval.Last, Durable: interval.Last, Applied: interval.Last,
	}
	notifications := 0
	manager.SetProgressListener(func() {
		notifications++
		if got, ok := store.CommitForID(interval.CommitID); !ok || !reflect.DeepEqual(got, interval) {
			t.Fatalf("notification preceded durable provenance: %+v, %v", got, ok)
		}
	})
	if err := manager.PublishCommit(interval, checkpoint); err != nil {
		t.Fatal(err)
	}
	if notifications != 1 {
		t.Fatalf("progress notifications = %d, want 1", notifications)
	}
	if state := manager.Snapshot(); state.Checkpoint != checkpoint {
		t.Fatalf("manager checkpoint = %+v, want %+v", state.Checkpoint, checkpoint)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if recovered := New(openControlStore(t, dir)).Snapshot(); recovered.Checkpoint != checkpoint {
		t.Fatalf("recovered checkpoint = %+v, want %+v", recovered.Checkpoint, checkpoint)
	}
}

func TestManagerPublishCommitPreservesConcurrentFetchProgress(t *testing.T) {
	store := openControlStore(t, t.TempDir())
	base := testOpTime(5)
	fetched := testOpTime(10)
	if err := store.SetCheckpoint(control.Checkpoint{
		Fetched: fetched, Buffered: fetched, Written: base, Durable: base, Applied: base,
	}); err != nil {
		t.Fatal(err)
	}
	manager := New(store)
	published := testOpTime(6)
	checkpoint := control.Checkpoint{
		Fetched: published, Buffered: published, Written: published, Durable: published, Applied: published,
	}
	interval := control.CommitInterval{First: published, Last: published, CommitID: "publication-six"}
	if err := manager.PublishCommit(interval, checkpoint); err != nil {
		t.Fatal(err)
	}
	checkpoint = manager.Snapshot().Checkpoint
	if checkpoint.Fetched != fetched || checkpoint.Buffered != fetched || checkpoint.Written != published ||
		checkpoint.Durable != published || checkpoint.Applied != published {
		t.Fatalf("manager checkpoint = %+v", checkpoint)
	}
}

func TestManagerRejectsStaleOrConflictingConfiguration(t *testing.T) {
	manager := New(openControlStore(t, t.TempDir()))
	configuration := testReplicaConfiguration()
	if err := manager.InstallConfiguration(configuration, 8); err != nil {
		t.Fatal(err)
	}
	stale := configuration
	stale.Version--
	if err := manager.InstallConfiguration(stale, 8); err == nil {
		t.Fatal("InstallConfiguration accepted a stale version")
	}
	conflicting := configuration
	conflicting.ReplicaSetID = "different-set-id"
	if err := manager.InstallConfiguration(conflicting, 8); err == nil {
		t.Fatal("InstallConfiguration accepted a conflicting replica set ID")
	}
	if err := manager.InstallConfiguration(configuration, 8); err != nil {
		t.Fatalf("InstallConfiguration rejected an unchanged configuration: %v", err)
	}
}

func openControlStore(t *testing.T, dir string) *control.Store {
	t.Helper()
	topologyControlBackends.Lock()
	backend := topologyControlBackends.byDirectory[dir]
	if backend == nil {
		var err error
		backend, err = dolt.NewBackend(dir, slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
		if err != nil {
			topologyControlBackends.Unlock()
			t.Fatal(err)
		}
		topologyControlBackends.byDirectory[dir] = backend
		t.Cleanup(func() {
			topologyControlBackends.Lock()
			delete(topologyControlBackends.byDirectory, dir)
			topologyControlBackends.Unlock()
			backend.Close()
		})
	}
	topologyControlBackends.Unlock()
	store, err := control.Open(backend, control.Configuration{SetName: "rs0", MemberHost: "dumbo.example:27017"})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testReplicaConfiguration() control.ReplicaConfiguration {
	return control.ReplicaConfiguration{
		SetName:         "rs0",
		Version:         4,
		Term:            3,
		ProtocolVersion: 1,
		ReplicaSetID:    "set-id",
		Members: []control.MemberConfiguration{
			{MemberID: 1, Host: "primary.example:27017", Priority: 1, Votes: 1},
			{MemberID: 2, Host: "secondary.example:27017", Priority: 1, Votes: 1},
			{MemberID: 3, Host: "dumbo.example:27017", Hidden: true, Priority: 0, Votes: 0},
		},
	}
}

func testOpTime(increment uint32) control.OpTime {
	return control.OpTime{Seconds: 100, Increment: increment, Term: 8}
}
