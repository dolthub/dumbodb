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

package control

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStorePersistsRecoveryState(t *testing.T) {
	dir := t.TempDir()
	configuration := testConfiguration()
	store, err := Open(dir, configuration)
	if err != nil {
		t.Fatal(err)
	}

	identity := Identity{ReplicaSetID: "set-id", MemberID: 3, ConfigVersion: 7, ConfigTerm: 4, Term: 9}
	member := MemberConfiguration{MemberID: 3, Host: configuration.MemberHost, Hidden: true, Priority: 0, Votes: 0}
	if err := store.InstallMember(identity, member); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSource("primary.example:27017", 18); err != nil {
		t.Fatal(err)
	}
	if err := store.SetInitialSyncPhase(InitialSyncApplying); err != nil {
		t.Fatal(err)
	}
	checkpoint := Checkpoint{
		Fetched:  opTime(20),
		Buffered: opTime(19),
		Written:  opTime(18),
		Durable:  opTime(17),
		Applied:  opTime(16),
	}
	if err := store.SetCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	interval := CommitInterval{First: opTime(10), Last: opTime(16), CommitID: "abc123"}
	if err := store.RecordCommit(interval); err != nil {
		t.Fatal(err)
	}
	mapping := CollectionMapping{
		SourceUUID:       "source-uuid",
		Database:         "orders",
		Collection:       "items",
		LocalUUID:        "local-uuid",
		CreateOpTime:     opTime(8),
		LastUpdateOpTime: opTime(8),
	}
	if err := store.PutCollectionMapping(mapping); err != nil {
		t.Fatal(err)
	}
	fragment := TransactionFragment{Key: "session-1/4", First: opTime(12), Last: opTime(15), Payload: []byte("applyOps")}
	if err := store.PutTransactionFragment(fragment); err != nil {
		t.Fatal(err)
	}
	if err := store.Detach(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	state := reopened.Snapshot()
	if state.Lifecycle != LifecycleDetached || state.CurrentSource != "" || state.CurrentRBID != 18 {
		t.Fatalf("unexpected lifecycle state: %+v", state)
	}
	if state.Identity == nil || *state.Identity != identity {
		t.Fatalf("identity = %+v, want %+v", state.Identity, identity)
	}
	if state.Checkpoint != checkpoint || state.InitialSyncPhase != InitialSyncApplying {
		t.Fatalf("checkpoint state = %+v", state)
	}
	if got, ok := reopened.CommitFor(opTime(14)); !ok || got != interval {
		t.Fatalf("CommitFor = %+v, %v; want %+v, true", got, ok, interval)
	}
	if got, ok := reopened.CollectionMapping(mapping.SourceUUID); !ok || got != mapping {
		t.Fatalf("CollectionMapping = %+v, %v; want %+v, true", got, ok, mapping)
	}
	if got := state.TransactionParts[fragment.Key]; string(got.Payload) != "applyOps" || got.First != fragment.First || got.Last != fragment.Last {
		t.Fatalf("transaction fragment = %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
}

func TestInitialSyncAttemptLifecycleIsAtomicAndPersistent(t *testing.T) {
	dir := t.TempDir()
	configuration := testConfiguration()
	store, err := Open(dir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	beginFetch := OpTime{Seconds: 100, Increment: 3, Term: 8}
	beginApply := OpTime{Seconds: 100, Increment: 5, Term: 8}
	attempt := InitialSyncAttempt{
		ID: "attempt-one", Source: "mongo.example:27017", SourceRBID: 11,
		SourceInitialSyncID: "source-generation", BeginFetch: beginFetch, BeginApply: beginApply,
	}
	if err := store.BeginInitialSync(attempt); err != nil {
		t.Fatal(err)
	}
	stop := OpTime{Seconds: 101, Increment: 9, Term: 8}
	if err := store.SetInitialSyncStop(attempt.ID, stop); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	state := reopened.Snapshot()
	wantAttempt := InitialSyncAttempt{
		ID: "attempt-one", Source: "mongo.example:27017", SourceRBID: 11,
		SourceInitialSyncID: "source-generation", BeginFetch: beginFetch, BeginApply: beginApply, Stop: stop,
	}
	if state.InitialSyncPhase != InitialSyncApplying || state.InitialSyncAttempt == nil || *state.InitialSyncAttempt != wantAttempt {
		t.Fatalf("reopened initial sync state = %+v", state)
	}
	checkpoint := Checkpoint{Fetched: stop, Buffered: stop, Written: stop, Durable: stop, Applied: stop}
	if err := reopened.CompleteInitialSync(attempt.ID, checkpoint, 12); err == nil {
		t.Fatal("completion accepted a changed source rollback ID")
	}
	if reopened.Snapshot().InitialSyncPhase != InitialSyncApplying {
		t.Fatal("failed completion changed initial sync phase")
	}
	if err := reopened.CompleteInitialSync(attempt.ID, checkpoint, 11); err != nil {
		t.Fatal(err)
	}
	completed := reopened.Snapshot()
	if completed.InitialSyncPhase != InitialSyncComplete || completed.Checkpoint != checkpoint || completed.CurrentRBID != 11 || completed.CurrentSource != attempt.Source {
		t.Fatalf("completed initial sync state = %+v", completed)
	}
}

func TestInitialSyncAttemptRejectsInvalidBoundaries(t *testing.T) {
	store, err := Open(t.TempDir(), testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	later := OpTime{Seconds: 100, Increment: 5, Term: 8}
	earlier := OpTime{Seconds: 100, Increment: 3, Term: 8}
	if err := store.BeginInitialSync(InitialSyncAttempt{
		ID: "bad", Source: "mongo.example:27017", SourceRBID: 1, BeginFetch: later, BeginApply: earlier,
	}); err == nil {
		t.Fatal("accepted begin-apply before begin-fetch")
	}
	if err := store.BeginInitialSync(InitialSyncAttempt{
		ID: "good", Source: "mongo.example:27017", SourceRBID: 1, BeginFetch: earlier, BeginApply: later,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetInitialSyncStop("good", earlier); err == nil {
		t.Fatal("accepted stop before begin-apply")
	}
	if err := store.ResetInitialSync("other"); err == nil {
		t.Fatal("reset a different initial sync attempt")
	}
	if err := store.SetSource("mongo.example:27017", 9); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCheckpoint(Checkpoint{Fetched: later, Buffered: later}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCommit(CommitInterval{First: earlier, Last: later, CommitID: "partial"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutCollectionMapping(CollectionMapping{
		SourceUUID: "source", Database: "orders", Collection: "items", LocalUUID: "local", CreateOpTime: earlier,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutTransactionFragment(TransactionFragment{Key: "transaction", First: earlier, Last: later, Payload: []byte("partial")}); err != nil {
		t.Fatal(err)
	}
	if err := store.ResetInitialSync("good"); err != nil {
		t.Fatal(err)
	}
	state := store.Snapshot()
	if state.InitialSyncPhase != InitialSyncNotStarted || state.InitialSyncAttempt != nil || state.Checkpoint != (Checkpoint{}) ||
		state.CurrentRBID != 0 || len(state.CommitIntervals) != 0 || len(state.CollectionMappings) != 0 || len(state.TransactionParts) != 0 {
		t.Fatalf("reset initial sync state = %+v", state)
	}
}

func TestCollectionMappingRenameDropAndNameReuse(t *testing.T) {
	store, err := Open(t.TempDir(), testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	first := CollectionMapping{
		SourceUUID: "source-one", Database: "orders", Collection: "items", LocalUUID: "local-one", CreateOpTime: opTime(1),
	}
	if err := store.PutCollectionMapping(first); err != nil {
		t.Fatal(err)
	}
	if err := store.PutCollectionMapping(CollectionMapping{
		SourceUUID: "source-two", Database: "orders", Collection: "items", LocalUUID: "local-two", CreateOpTime: opTime(2),
	}); err == nil {
		t.Fatal("PutCollectionMapping accepted an active namespace collision")
	}
	if err := store.RenameCollectionMapping("source-one", "orders", "items", "orders", "renamed", opTime(3)); err != nil {
		t.Fatal(err)
	}
	if mapping, ok := store.ActiveCollectionMapping("source-one"); !ok || mapping.Collection != "renamed" || mapping.LocalUUID != "local-one" {
		t.Fatalf("renamed mapping = %+v, %v", mapping, ok)
	}
	if err := store.DropCollectionMapping("source-one", opTime(4)); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.ActiveCollectionMapping("source-one"); ok {
		t.Fatal("dropped mapping remained active")
	}
	second := CollectionMapping{
		SourceUUID: "source-two", Database: "orders", Collection: "renamed", LocalUUID: "local-two", CreateOpTime: opTime(5),
	}
	if err := store.PutCollectionMapping(second); err != nil {
		t.Fatalf("name reuse after drop: %v", err)
	}
	if err := store.RenameCollectionMapping("source-one", "orders", "renamed", "orders", "wrong", opTime(6)); err == nil {
		t.Fatal("RenameCollectionMapping revived a dropped UUID")
	}
}

func TestStoreRejectsWrongIdentityAndMemberConfiguration(t *testing.T) {
	store, err := Open(t.TempDir(), testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{ReplicaSetID: "set-id", MemberID: 3, ConfigVersion: 1, ConfigTerm: 1, Term: 1}
	valid := MemberConfiguration{MemberID: 3, Host: "dumbo.example:27017", Hidden: true, Priority: 0, Votes: 0}
	if err := store.InstallMember(identity, valid); err != nil {
		t.Fatal(err)
	}

	invalidMembers := []MemberConfiguration{
		{MemberID: 3, Host: valid.Host, Hidden: false, Priority: 0, Votes: 0},
		{MemberID: 3, Host: valid.Host, Hidden: true, Priority: 1, Votes: 0},
		{MemberID: 3, Host: valid.Host, Hidden: true, Priority: 0, Votes: 1},
		{MemberID: 3, Host: "other.example:27017", Hidden: true, Priority: 0, Votes: 0},
	}
	for _, member := range invalidMembers {
		if err := store.InstallMember(identity, member); err == nil {
			t.Fatalf("InstallMember(%+v) succeeded", member)
		}
	}
	if err := store.InstallMember(Identity{ReplicaSetID: "other-set", MemberID: 3, ConfigVersion: 2, ConfigTerm: 1, Term: 2}, valid); err == nil {
		t.Fatal("InstallMember accepted a different replica set ID")
	}
	if err := store.InstallMember(Identity{ReplicaSetID: "set-id", MemberID: 3, ConfigVersion: 0, ConfigTerm: 1, Term: 2}, valid); err == nil {
		t.Fatal("InstallMember accepted a stale configuration")
	}
}

func TestStoreRejectsCheckpointAndCommitRegression(t *testing.T) {
	store, err := Open(t.TempDir(), testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCheckpoint(Checkpoint{Fetched: opTime(4), Buffered: opTime(3), Written: opTime(2), Durable: opTime(1), Applied: opTime(1)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCheckpoint(Checkpoint{Fetched: opTime(5), Buffered: opTime(4), Written: opTime(5), Durable: opTime(3), Applied: opTime(2)}); err == nil {
		t.Fatal("SetCheckpoint accepted unordered positions")
	}
	if err := store.SetCheckpoint(Checkpoint{Fetched: opTime(3), Buffered: opTime(3), Written: opTime(2), Durable: opTime(1), Applied: opTime(1)}); err == nil {
		t.Fatal("SetCheckpoint accepted a regression")
	}
	checkpoint, err := store.ResetFetchProgress()
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Fetched != opTime(2) || checkpoint.Buffered != opTime(2) || checkpoint.Written != opTime(2) || checkpoint.Durable != opTime(1) {
		t.Fatalf("reset checkpoint = %+v", checkpoint)
	}
	if err := store.RecordCommit(CommitInterval{First: opTime(1), Last: opTime(3), CommitID: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCommit(CommitInterval{First: opTime(3), Last: opTime(4), CommitID: "overlap"}); err == nil {
		t.Fatal("RecordCommit accepted an overlapping interval")
	}
	if err := store.RecordCommit(CommitInterval{First: opTime(4), Last: opTime(6), CommitID: "two"}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsDifferentLifecycleConfiguration(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, testConfiguration()); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Configuration{SetName: "other", MemberHost: "dumbo.example:27017"}); err == nil {
		t.Fatal("Open accepted a different replica set name")
	}
}

func TestStorePersistsInstalledReplicaConfiguration(t *testing.T) {
	dir := t.TempDir()
	configuration := testConfiguration()
	store, err := Open(dir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	replicaConfiguration := ReplicaConfiguration{
		SetName:         "rs0",
		Version:         9,
		Term:            4,
		ProtocolVersion: 1,
		ReplicaSetID:    "set-id",
		Members: []MemberConfiguration{
			{MemberID: 1, Host: "primary.example:27017", Priority: 1, Votes: 1},
			{MemberID: 3, Host: configuration.MemberHost, Hidden: true, Priority: 0, Votes: 0},
		},
	}
	if err := store.InstallReplicaConfiguration(replicaConfiguration, 11); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	state := reopened.Snapshot()
	if state.ReplicaConfig == nil || state.ReplicaConfig.Version != 9 || len(state.ReplicaConfig.Members) != 2 {
		t.Fatalf("replica configuration = %+v", state.ReplicaConfig)
	}
	if state.Identity == nil || state.Identity.MemberID != 3 || state.Identity.Term != 11 {
		t.Fatalf("identity = %+v", state.Identity)
	}
}

func TestStoreRejectsReplicaConfigurationWithoutSafeMember(t *testing.T) {
	store, err := Open(t.TempDir(), testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	configuration := ReplicaConfiguration{
		SetName:      "rs0",
		Version:      1,
		Term:         1,
		ReplicaSetID: "set-id",
		Members: []MemberConfiguration{
			{MemberID: 3, Host: "dumbo.example:27017", Hidden: true, Priority: 1, Votes: 0},
		},
	}
	if err := store.InstallReplicaConfiguration(configuration, 1); err == nil {
		t.Fatal("InstallReplicaConfiguration accepted an electable member")
	}
	configuration.Members[0] = MemberConfiguration{MemberID: 4, Host: "other.example:27017", Hidden: true}
	if err := store.InstallReplicaConfiguration(configuration, 1); err == nil {
		t.Fatal("InstallReplicaConfiguration accepted a configuration without this member")
	}
}

func testConfiguration() Configuration {
	return Configuration{SetName: "rs0", MemberHost: "dumbo.example:27017"}
}

func opTime(increment uint32) OpTime {
	return OpTime{Seconds: 100, Increment: increment, Term: 2}
}
