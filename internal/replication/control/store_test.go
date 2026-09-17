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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

func TestStorePersistsRecoveryState(t *testing.T) {
	dir := t.TempDir()
	configuration := testConfiguration()
	store, err := openTestStore(t, dir, configuration)
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openTestStore(t, dir, configuration)
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
	if got, ok := reopened.CommitFor(opTime(14)); !ok || !equalCommitInterval(got, interval) {
		t.Fatalf("CommitFor = %+v, %v; want %+v, true", got, ok, interval)
	}
	if got, ok := reopened.CollectionMapping(mapping.SourceUUID); !ok || got != mapping {
		t.Fatalf("CollectionMapping = %+v, %v; want %+v, true", got, ok, mapping)
	}
	if got := state.TransactionParts[fragment.Key]; string(got.Payload) != "applyOps" || got.First != fragment.First || got.Last != fragment.Last {
		t.Fatalf("transaction fragment = %+v", got)
	}
}

func TestStorePersistsReplicationDiagnostics(t *testing.T) {
	directory := t.TempDir()
	configuration := testConfiguration()
	store, err := openTestStore(t, directory, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSource("primary.example:27017", 4); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSource("secondary.example:27017", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordFailure(ReplicationFailure{
		Stage: "steady replication", Classification: "timeout", Message: "source timed out", Retryable: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Detach(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openTestStore(t, directory, configuration)
	if err != nil {
		t.Fatal(err)
	}
	state := reopened.Snapshot()
	if state.RetryCount != 1 || len(state.FailureHistory) != 1 {
		t.Fatalf("failure diagnostics = count %d, history %+v", state.RetryCount, state.FailureHistory)
	}
	if len(state.SourceChanges) != 3 || state.SourceChanges[2].Current != "" {
		t.Fatalf("source changes = %+v", state.SourceChanges)
	}
	if state.Lifecycle != LifecycleDetached {
		t.Fatalf("lifecycle = %q, want detached", state.Lifecycle)
	}
}

func TestInitialSyncAttemptLifecycleIsAtomicAndPersistent(t *testing.T) {
	dir := t.TempDir()
	configuration := testConfiguration()
	store, err := openTestStore(t, dir, configuration)
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openTestStore(t, dir, configuration)
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

func TestInitialSyncFailurePersistsUntilReset(t *testing.T) {
	dir := t.TempDir()
	store, err := openTestStore(t, dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	attempt := InitialSyncAttempt{
		ID: "attempt-one", Source: "primary.example:27017", SourceRBID: 4,
		BeginFetch: opTime(1), BeginApply: opTime(1),
	}
	if err := store.BeginInitialSync(attempt); err != nil {
		t.Fatal(err)
	}
	failure := InitialSyncFailure{
		Namespace: "archive.items",
		BSONType:  "JavaScript",
		Message:   "DumboDB does not support BSON type JavaScript while cloning archive.items",
	}
	if err := store.RecordInitialSyncFailure(failure); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTestStore(t, dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().InitialSyncFailure == nil || *reopened.Snapshot().InitialSyncFailure != failure {
		t.Fatalf("recovered initial-sync failure = %+v", reopened.Snapshot().InitialSyncFailure)
	}
	if err := reopened.BeginInitialSync(attempt); err == nil {
		t.Fatal("BeginInitialSync accepted a terminal failure")
	}
	if err := reopened.ResetInitialSync(attempt.ID); err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().InitialSyncFailure != nil {
		t.Fatalf("reset retained initial-sync failure = %+v", reopened.Snapshot().InitialSyncFailure)
	}
}

func TestInitialSyncAttemptRejectsInvalidBoundaries(t *testing.T) {
	store, err := openTestStore(t, t.TempDir(), testConfiguration())
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
	oldCommitLogGeneration := store.Snapshot().CommitLogGeneration
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
	if state.CommitLogGeneration != oldCommitLogGeneration+1 {
		t.Fatalf("reset commit log generation = %d, want %d", state.CommitLogGeneration, oldCommitLogGeneration+1)
	}
}

func TestCollectionMappingRenameDropAndNameReuse(t *testing.T) {
	store, err := openTestStore(t, t.TempDir(), testConfiguration())
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
	store, err := openTestStore(t, t.TempDir(), testConfiguration())
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
	store, err := openTestStore(t, t.TempDir(), testConfiguration())
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
	if err := store.RecordCommit(CommitInterval{First: opTime(7), Last: opTime(7), CommitID: "three"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCommit(CommitInterval{First: opTime(6), Last: opTime(6), CommitID: "out-of-order"}); err == nil {
		t.Fatal("RecordCommit accepted an out-of-order interval")
	}
}

func TestCommitIntervalsUseAdminCollection(t *testing.T) {
	dir := t.TempDir()
	store, err := openTestStore(t, dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	intervals := []CommitInterval{
		{First: opTime(1), Last: opTime(3), CommitID: "one"},
		{First: opTime(4), Last: opTime(8), CommitID: "two"},
		{First: opTime(10), Last: opTime(10), CommitID: "three"},
	}
	for _, interval := range intervals {
		if err := store.RecordCommit(interval); err != nil {
			t.Fatal(err)
		}
	}
	persisted, version, exists, err := store.storage.load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("admin control document does not exist")
	}
	if version != controlFormatVersion {
		t.Fatalf("control format version = %d, want %d", version, controlFormatVersion)
	}
	if len(persisted.CommitIntervals) != 0 {
		t.Fatalf("control document embeds commit intervals = %+v", persisted.CommitIntervals)
	}
	persistedIntervals, err := store.storage.loadCommitIntervals(t.Context(), persisted.CommitLogGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(persistedIntervals, intervals, equalCommitInterval) {
		t.Fatalf("persisted commit intervals = %+v, want %+v", persistedIntervals, intervals)
	}
	if err := store.RecordCommit(intervals[1]); err != nil {
		t.Fatal(err)
	}
	if len(store.CommitIntervals()) != len(intervals) {
		t.Fatal("idempotent replay duplicated a commit interval")
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTestStore(t, dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.CommitFor(opTime(6)); !ok || !equalCommitInterval(got, intervals[1]) {
		t.Fatalf("CommitFor after reopen = %+v, %v; want %+v, true", got, ok, intervals[1])
	}
	if _, ok := reopened.CommitFor(opTime(9)); ok {
		t.Fatal("CommitFor matched an uncovered gap")
	}
}

func TestControlStateUsesOnlyReservedAdminCollection(t *testing.T) {
	directory := t.TempDir()
	backend := testBackend(t, directory)
	store, err := Open(backend, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, ".replication")); !os.IsNotExist(err) {
		t.Fatalf("unexpected replication directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected replication state file: %v", err)
	}
	admin, err := backend.Database("admin")
	if err != nil {
		t.Fatal(err)
	}
	collection, err := admin.Collection(backends.ReservedReplicationControlName)
	if err != nil {
		t.Fatal(err)
	}
	collections, err := admin.ListCollections(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, collection := range collections.Collections {
		if collection.Name == backends.ReservedReplicationControlName {
			found = true
		}
	}
	if !found {
		t.Fatal("replication control collection is not listed")
	}

	if state := readPublicControlState(t, collection); state.InitialSyncPhase != InitialSyncNotStarted {
		t.Fatalf("initial sync phase = %q, want %q", state.InitialSyncPhase, InitialSyncNotStarted)
	}

	document := mustDocument(t, "_id", controlDocumentID, "state", "tampered")
	writes := []struct {
		name  string
		write func() error
	}{
		{"insert", func() error {
			_, err := collection.InsertAll(t.Context(), &backends.InsertAllParams{Docs: []*types.Document{mustDocument(t, "_id", "other")}})
			return err
		}},
		{"update", func() error {
			_, err := collection.UpdateAll(t.Context(), &backends.UpdateAllParams{Docs: []*types.Document{document}})
			return err
		}},
		{"delete", func() error {
			_, err := collection.DeleteAll(t.Context(), &backends.DeleteAllParams{IDs: []any{controlDocumentID}})
			return err
		}},
		{"compact", func() error {
			_, err := collection.Compact(t.Context(), new(backends.CompactParams))
			return err
		}},
		{"create indexes", func() error {
			_, err := collection.CreateIndexes(t.Context(), new(backends.CreateIndexesParams))
			return err
		}},
		{"drop indexes", func() error {
			_, err := collection.DropIndexes(t.Context(), new(backends.DropIndexesParams))
			return err
		}},
		{"create collection", func() error {
			return admin.CreateCollection(t.Context(), &backends.CreateCollectionParams{Name: backends.ReservedReplicationControlName})
		}},
		{"drop collection", func() error {
			return admin.DropCollection(t.Context(), &backends.DropCollectionParams{Name: backends.ReservedReplicationControlName})
		}},
		{"rename collection", func() error {
			return admin.RenameCollection(t.Context(), &backends.RenameCollectionParams{OldName: backends.ReservedReplicationControlName, NewName: "other"})
		}},
		{"rename to collection", func() error {
			return admin.RenameCollection(t.Context(), &backends.RenameCollectionParams{OldName: "other", NewName: backends.ReservedReplicationControlName})
		}},
		{"collMod", func() error {
			return admin.CollMod(t.Context(), &backends.CollModParams{Name: backends.ReservedReplicationControlName})
		}},
	}
	for _, test := range writes {
		t.Run(test.name, func(t *testing.T) {
			if err := test.write(); !backends.ErrorCodeIs(err, backends.ErrorCodeReadOnlyCollection) {
				t.Fatalf("error = %v, want read-only collection", err)
			}
		})
	}

	if err := store.SetInitialSyncPhase(InitialSyncCloning); err != nil {
		t.Fatal(err)
	}
	if state := readPublicControlState(t, collection); state.InitialSyncPhase != InitialSyncCloning {
		t.Fatalf("initial sync phase = %q, want %q", state.InitialSyncPhase, InitialSyncCloning)
	}
}

func readPublicControlState(t *testing.T, collection backends.Collection) State {
	t.Helper()
	document := readPublicControlDocument(t, collection)
	version, err := document.Get("formatVersion")
	if err != nil {
		t.Fatal(err)
	}
	if version != controlFormatVersion {
		t.Fatalf("format version = %v, want %d", version, controlFormatVersion)
	}
	value, err := document.Get("state")
	if err != nil {
		t.Fatal(err)
	}
	stateDocument, ok := value.(*types.Document)
	if !ok {
		t.Fatalf("state type = %T, want *types.Document", value)
	}
	lifecycle, err := stateDocument.Get("lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle == nil {
		t.Fatal("structured state has no lifecycle")
	}
	state, err := decodeStateDocument(stateDocument)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func readPublicControlDocument(t *testing.T, collection backends.Collection) *types.Document {
	t.Helper()
	result, err := collection.Query(t.Context(), new(backends.QueryParams))
	if err != nil {
		t.Fatal(err)
	}
	defer result.Iter.Close()
	for {
		_, document, err := result.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			t.Fatal("control document not found")
		}
		if err != nil {
			t.Fatal(err)
		}
		id, _ := document.Get("_id")
		if id == controlDocumentID {
			return document
		}
	}
}

func mustDocument(t *testing.T, pairs ...any) *types.Document {
	t.Helper()
	document, err := types.NewDocument(pairs...)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestOpenMigratesLegacyBinaryControlState(t *testing.T) {
	directory := t.TempDir()
	backend := testBackend(t, directory)
	configuration := testConfiguration()
	legacyState := newState(configuration)
	legacyState.CurrentSource = "legacy.example:27017"
	data, err := json.Marshal(legacyState)
	if err != nil {
		t.Fatal(err)
	}
	internalCollection, err := backend.(backends.ReplicationControlBackend).ReplicationControlCollection()
	if err != nil {
		t.Fatal(err)
	}
	legacyDocument := mustDocument(t,
		"_id", controlDocumentID,
		"formatVersion", legacyBinaryControlVersion,
		"state", types.Binary{B: data, Subtype: types.BinaryGeneric},
	)
	if _, err := internalCollection.InsertAll(t.Context(), &backends.InsertAllParams{Docs: []*types.Document{legacyDocument}}); err != nil {
		t.Fatal(err)
	}
	store, err := Open(backend, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if source := store.Snapshot().CurrentSource; source != legacyState.CurrentSource {
		t.Fatalf("source = %q, want %q", source, legacyState.CurrentSource)
	}
	if err := store.SetInitialSyncPhase(InitialSyncCloning); err != nil {
		t.Fatal(err)
	}
	admin, err := backend.Database("admin")
	if err != nil {
		t.Fatal(err)
	}
	collection, err := admin.Collection(backends.ReservedReplicationControlName)
	if err != nil {
		t.Fatal(err)
	}
	state := readPublicControlState(t, collection)
	if state.CurrentSource != legacyState.CurrentSource || state.InitialSyncPhase != InitialSyncCloning {
		t.Fatalf("migrated state = %+v", state)
	}
}

func TestOpenMigratesStructuredControlStateWithEmbeddedIntervals(t *testing.T) {
	directory := t.TempDir()
	backend := testBackend(t, directory)
	configuration := testConfiguration()
	legacyState := newState(configuration)
	legacyState.CommitIntervals = []CommitInterval{
		{First: opTime(1), Last: opTime(2), CommitID: "one"},
		{First: opTime(3), Last: opTime(4), CommitID: "two"},
	}
	stateDocument, err := encodeStateDocument(legacyState)
	if err != nil {
		t.Fatal(err)
	}
	internalCollection, err := backend.(backends.ReplicationControlBackend).ReplicationControlCollection()
	if err != nil {
		t.Fatal(err)
	}
	legacyDocument := mustDocument(t,
		"_id", controlDocumentID,
		"formatVersion", legacyStructuredControlVersion,
		"state", stateDocument,
	)
	if _, err := internalCollection.InsertAll(t.Context(), &backends.InsertAllParams{Docs: []*types.Document{legacyDocument}}); err != nil {
		t.Fatal(err)
	}

	store, err := Open(backend, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.CommitIntervals(); !slices.EqualFunc(got, legacyState.CommitIntervals, equalCommitInterval) {
		t.Fatalf("migrated intervals = %+v, want %+v", got, legacyState.CommitIntervals)
	}
	persisted, version, exists, err := store.storage.load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !exists || version != controlFormatVersion {
		t.Fatalf("migrated control document exists = %v, version = %d", exists, version)
	}
	if len(persisted.CommitIntervals) != 0 {
		t.Fatalf("migrated control document embeds intervals = %+v", persisted.CommitIntervals)
	}
	persistedIntervals, err := store.storage.loadCommitIntervals(t.Context(), persisted.CommitLogGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(persistedIntervals, legacyState.CommitIntervals, equalCommitInterval) {
		t.Fatalf("segmented intervals = %+v, want %+v", persistedIntervals, legacyState.CommitIntervals)
	}
}

func TestCommitIntervalsSurviveStorageReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := openTestStore(t, dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	interval := CommitInterval{First: opTime(1), Last: opTime(2), CommitID: "complete"}
	if err := store.RecordCommit(interval); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openTestStore(t, dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.CommitFor(opTime(1)); !ok || !equalCommitInterval(got, interval) {
		t.Fatalf("CommitFor after storage reopen = %+v, %v", got, ok)
	}
}

func TestResetInitialSyncDiscardsInactiveGenerationIntervals(t *testing.T) {
	directory := t.TempDir()
	store, err := openTestStore(t, directory, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	activeGeneration := store.Snapshot().CommitLogGeneration
	if err := store.RecordCommit(CommitInterval{First: opTime(1), Last: opTime(1), CommitID: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := store.storage.appendCommitInterval(t.Context(), activeGeneration+1, CommitInterval{
		First: opTime(2), Last: opTime(2), CommitID: "stale-inactive-generation",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ResetInitialSync(""); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openTestStore(t, directory, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if intervals := reopened.CommitIntervals(); len(intervals) != 0 {
		t.Fatalf("reset retained inactive generation intervals = %+v", intervals)
	}
}

func TestCommitIntervalsAllowFetchProgressResetAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := openTestStore(t, dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	first := CommitInterval{First: opTime(1), Last: opTime(5), CommitID: "first"}
	ahead := opTime(10)
	firstCheckpoint := Checkpoint{
		Fetched: ahead, Buffered: ahead, Written: first.Last, Durable: first.Last, Applied: first.Last,
	}
	if err := store.PublishCommit(first, firstCheckpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResetFetchProgress(); err != nil {
		t.Fatal(err)
	}
	second := CommitInterval{First: opTime(6), Last: opTime(6), CommitID: "second"}
	secondCheckpoint := Checkpoint{
		Fetched: second.Last, Buffered: second.Last, Written: second.Last, Durable: second.Last, Applied: second.Last,
	}
	if err := store.PublishCommit(second, secondCheckpoint); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openTestStore(t, dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Snapshot().Checkpoint; got != secondCheckpoint {
		t.Fatalf("checkpoint after fetch reset = %+v, want %+v", got, secondCheckpoint)
	}
	if intervals := reopened.CommitIntervals(); len(intervals) != 2 || intervals[1].CommitID != second.CommitID {
		t.Fatalf("commit intervals after fetch reset = %+v", intervals)
	}
}

func TestPublishCommitRecoversCheckpointFromStorage(t *testing.T) {
	dir := t.TempDir()
	store, err := openTestStore(t, dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	fetched := opTime(9)
	if err := store.SetCheckpoint(Checkpoint{Fetched: fetched, Buffered: fetched}); err != nil {
		t.Fatal(err)
	}
	interval := CommitInterval{
		First: opTime(1), Last: opTime(5), CommitID: "published",
		Commits: []DatabaseCommit{{Database: "accounts", CommitID: "accounts-five"}, {Database: "orders", CommitID: "orders-five"}},
	}
	checkpoint := Checkpoint{
		Fetched: fetched, Buffered: fetched, Written: interval.Last, Durable: interval.Last, Applied: interval.Last,
	}
	if err := store.PublishCommit(interval, checkpoint); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.CommitForID(interval.CommitID); !ok || !equalCommitInterval(got, interval) {
		t.Fatalf("CommitForID = %+v, %v; want %+v, true", got, ok, interval)
	}
	if got, ok := store.CommitForDatabaseCommit("orders", "orders-five"); !ok || !equalCommitInterval(got, interval) {
		t.Fatalf("CommitForDatabaseCommit = %+v, %v; want %+v, true", got, ok, interval)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTestStore(t, dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Snapshot().Checkpoint; got != checkpoint {
		t.Fatalf("checkpoint recovered from storage = %+v, want %+v", got, checkpoint)
	}
	if got, ok := reopened.CommitForID(interval.CommitID); !ok || !equalCommitInterval(got, interval) {
		t.Fatalf("reopened CommitForID = %+v, %v; want %+v, true", got, ok, interval)
	}
	if err := reopened.PublishCommit(interval, checkpoint); err != nil {
		t.Fatalf("idempotent publication: %v", err)
	}
	if err := reopened.PublishCommit(
		CommitInterval{First: opTime(6), Last: opTime(6), CommitID: interval.CommitID},
		Checkpoint{Fetched: fetched, Buffered: fetched, Written: opTime(6), Durable: opTime(6), Applied: opTime(6)},
	); err == nil {
		t.Fatal("PublishCommit accepted a reused commit ID")
	}
}

func TestPublishCommitCompletesAfterIntervalOnlyCrash(t *testing.T) {
	directory := t.TempDir()
	store, err := openTestStore(t, directory, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	interval := CommitInterval{First: opTime(1), Last: opTime(2), CommitID: "published"}
	checkpoint := Checkpoint{
		Fetched: interval.Last, Buffered: interval.Last, Written: interval.Last,
		Durable: interval.Last, Applied: interval.Last,
	}
	generation := store.Snapshot().CommitLogGeneration
	if err := store.storage.appendCommitInterval(t.Context(), generation, interval); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openTestStore(t, directory, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Snapshot().Checkpoint; got != (Checkpoint{}) {
		t.Fatalf("checkpoint advanced before publication = %+v", got)
	}
	if got, ok := reopened.CommitForID(interval.CommitID); !ok || !equalCommitInterval(got, interval) {
		t.Fatalf("interval after restart = %+v, %v; want %+v, true", got, ok, interval)
	}
	if err := reopened.PublishCommit(interval, checkpoint); err != nil {
		t.Fatal(err)
	}
	if got := reopened.Snapshot().Checkpoint; got != checkpoint {
		t.Fatalf("completed checkpoint = %+v, want %+v", got, checkpoint)
	}
}

func TestOpenRejectsDifferentLifecycleConfiguration(t *testing.T) {
	dir := t.TempDir()
	store, err := openTestStore(t, dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openTestStore(t, dir, Configuration{SetName: "other", MemberHost: "dumbo.example:27017"}); err == nil {
		t.Fatal("Open accepted a different replica set name")
	}
}

func TestStorePersistsInstalledReplicaConfiguration(t *testing.T) {
	dir := t.TempDir()
	configuration := testConfiguration()
	store, err := openTestStore(t, dir, configuration)
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTestStore(t, dir, configuration)
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

func TestInitialSyncDataResetPreservesReplicaIdentity(t *testing.T) {
	directory := t.TempDir()
	configuration := testConfiguration()
	backend := testBackend(t, directory)
	store, err := Open(backend, configuration)
	if err != nil {
		t.Fatal(err)
	}
	replicaConfiguration := ReplicaConfiguration{
		SetName:         configuration.SetName,
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
	resetter := backend.(backends.InitialSyncResetter)
	if err := resetter.ResetInitialSyncData(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(backend, configuration)
	if err != nil {
		t.Fatal(err)
	}
	state := reopened.Snapshot()
	if state.ReplicaConfig == nil || state.ReplicaConfig.Version != replicaConfiguration.Version {
		t.Fatalf("replica configuration = %+v", state.ReplicaConfig)
	}
	if state.Identity == nil || state.Identity.MemberID != 3 || state.Identity.Term != 11 {
		t.Fatalf("identity = %+v", state.Identity)
	}
}

func TestStoreRejectsReplicaConfigurationWithoutSafeMember(t *testing.T) {
	store, err := openTestStore(t, t.TempDir(), testConfiguration())
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

func TestStoreRejectsMalformedReplicaConfiguration(t *testing.T) {
	base := ReplicaConfiguration{
		SetName: "rs0", Version: 2, Term: 1, ProtocolVersion: 1, ReplicaSetID: "set-id",
		Members: []MemberConfiguration{
			{MemberID: 1, Host: "primary.example:27017", Priority: 1, Votes: 1},
			{MemberID: 3, Host: "dumbo.example:27017", Hidden: true, Priority: 0, Votes: 0},
		},
	}
	tests := []struct {
		name   string
		mutate func(*ReplicaConfiguration)
	}{
		{name: "non-positive version", mutate: func(configuration *ReplicaConfiguration) { configuration.Version = 0 }},
		{name: "unsupported protocol", mutate: func(configuration *ReplicaConfiguration) { configuration.ProtocolVersion = 2 }},
		{name: "duplicate member ID", mutate: func(configuration *ReplicaConfiguration) { configuration.Members[1].MemberID = 1 }},
		{name: "duplicate host", mutate: func(configuration *ReplicaConfiguration) {
			configuration.Members[1].Host = configuration.Members[0].Host
		}},
		{name: "invalid votes", mutate: func(configuration *ReplicaConfiguration) { configuration.Members[0].Votes = 2 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := openTestStore(t, t.TempDir(), testConfiguration())
			if err != nil {
				t.Fatal(err)
			}
			configuration := base
			configuration.Members = append([]MemberConfiguration(nil), base.Members...)
			test.mutate(&configuration)
			if err := store.InstallReplicaConfiguration(configuration, 1); err == nil {
				t.Fatalf("InstallReplicaConfiguration accepted %+v", configuration)
			}
		})
	}
}

func testConfiguration() Configuration {
	return Configuration{SetName: "rs0", MemberHost: "dumbo.example:27017"}
}

func opTime(increment uint32) OpTime {
	return OpTime{Seconds: 100, Increment: increment, Term: 2}
}

func BenchmarkRecordCommitAppend(b *testing.B) {
	for _, historyLength := range []int{0, 1_000, 100_000} {
		b.Run(fmt.Sprintf("history_%d", historyLength), func(b *testing.B) {
			store, err := openTestStore(b, b.TempDir(), testConfiguration())
			if err != nil {
				b.Fatal(err)
			}
			intervals := make([]CommitInterval, historyLength, historyLength+b.N)
			for index := range intervals {
				position := uint32(index + 1)
				intervals[index] = CommitInterval{First: opTime(position), Last: opTime(position), CommitID: fmt.Sprintf("seed-%d", index)}
			}
			store.state.CommitIntervals = intervals
			store.rebuildCommitIndexLocked()
			if err := store.persistLocked(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				position := uint32(historyLength + index + 1)
				if err := store.RecordCommit(CommitInterval{
					First: opTime(position), Last: opTime(position), CommitID: fmt.Sprintf("measured-%d", index),
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
