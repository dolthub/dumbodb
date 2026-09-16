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
	"reflect"
	"testing"
)

func TestPendingPublicationSurvivesRestartAndCompletes(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	last := opTime(5)
	checkpoint := Checkpoint{Fetched: last, Buffered: last, Written: last, Durable: last, Applied: last}
	pending := PendingPublication{
		ID: "publication-five", First: opTime(1), Last: last,
		Databases: []string{"accounts", "orders"}, Commits: make(map[string]string), Checkpoint: checkpoint,
	}
	if err := store.BeginPublication(pending); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPublicationReady(pending.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPublicationCommit(pending.ID, "accounts", "accounts-five"); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	recovered, ok := reopened.PendingPublication()
	if !ok || recovered.Commits["accounts"] != "accounts-five" || recovered.Commits["orders"] != "" {
		t.Fatalf("recovered pending publication = %+v, %v", recovered, ok)
	}
	if err := reopened.RecordPublicationCommit(pending.ID, "orders", "orders-five"); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, stateFileName)
	stateBeforeFinalJournal, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	interval := CommitInterval{
		First: pending.First, Last: pending.Last, CommitID: pending.ID,
		Commits: []DatabaseCommit{{Database: "accounts", CommitID: "accounts-five"}, {Database: "orders", CommitID: "orders-five"}},
	}
	if err := reopened.PublishCommit(interval, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.PendingPublication(); ok {
		t.Fatal("completed publication remained pending")
	}
	if got, ok := reopened.CommitForDatabaseCommit("orders", "orders-five"); !ok || !equalCommitInterval(got, interval) {
		t.Fatalf("database commit provenance = %+v, %v", got, ok)
	}
	if err := os.WriteFile(statePath, stateBeforeFinalJournal, 0o600); err != nil {
		t.Fatal(err)
	}
	afterCrash, err := Open(dir, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := afterCrash.PendingPublication(); ok {
		t.Fatal("journal recovery retained a completed pending publication")
	}
	if afterCrash.Snapshot().Checkpoint != checkpoint {
		t.Fatalf("journal recovery checkpoint = %+v, want %+v", afterCrash.Snapshot().Checkpoint, checkpoint)
	}
}

func TestPendingPublicationPreservesConcurrentFetchProgress(t *testing.T) {
	store, err := Open(t.TempDir(), testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	base := opTime(5)
	if err := store.SetCheckpoint(Checkpoint{
		Fetched: base, Buffered: base, Written: base, Durable: base, Applied: base,
	}); err != nil {
		t.Fatal(err)
	}
	published := opTime(6)
	pendingCheckpoint := Checkpoint{
		Fetched: published, Buffered: published, Written: published, Durable: published, Applied: published,
	}
	pending := PendingPublication{
		ID: "publication-six", First: published, Last: published,
		Databases: []string{"orders"}, Commits: make(map[string]string), Checkpoint: pendingCheckpoint,
	}
	if err := store.BeginPublication(pending); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPublicationReady(pending.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPublicationCommit(pending.ID, "orders", "orders-six"); err != nil {
		t.Fatal(err)
	}
	fetched := opTime(10)
	if err := store.SetCheckpoint(Checkpoint{
		Fetched: fetched, Buffered: fetched, Written: base, Durable: base, Applied: base,
	}); err != nil {
		t.Fatal(err)
	}
	interval := CommitInterval{
		First: published, Last: published, CommitID: pending.ID,
		Commits: []DatabaseCommit{{Database: "orders", CommitID: "orders-six"}},
	}
	if err := store.PublishCommit(interval, pendingCheckpoint); err != nil {
		t.Fatal(err)
	}
	checkpoint := store.Snapshot().Checkpoint
	if checkpoint.Fetched != fetched || checkpoint.Buffered != fetched || checkpoint.Written != published ||
		checkpoint.Durable != published || checkpoint.Applied != published {
		t.Fatalf("published checkpoint = %+v", checkpoint)
	}
	if _, ok := store.PendingPublication(); ok {
		t.Fatal("completed publication remained pending")
	}
}

func TestPendingPublicationRejectsIncompleteOrDifferentCompletion(t *testing.T) {
	store, err := Open(t.TempDir(), testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	last := opTime(3)
	checkpoint := Checkpoint{Fetched: last, Buffered: last, Written: last, Durable: last, Applied: last}
	pending := PendingPublication{
		ID: "publication", First: opTime(1), Last: last,
		Databases: []string{"orders"}, Commits: make(map[string]string), Checkpoint: checkpoint,
	}
	if err := store.BeginPublication(pending); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPublicationReady(pending.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishCommit(CommitInterval{First: pending.First, Last: last, CommitID: pending.ID}, checkpoint); err == nil {
		t.Fatal("PublishCommit completed without the database commit")
	}
	if err := store.RecordPublicationCommit(pending.ID, "other", "commit"); err == nil {
		t.Fatal("RecordPublicationCommit accepted an unrelated database")
	}
}

func TestApplyingPublicationCanAbortButReadyPublicationCannot(t *testing.T) {
	store, err := Open(t.TempDir(), testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	last := opTime(3)
	checkpoint := Checkpoint{Fetched: last, Buffered: last, Written: last, Durable: last, Applied: last}
	pending := PendingPublication{
		ID: "applying", First: last, Last: last,
		Databases: []string{"orders"}, Commits: make(map[string]string), Checkpoint: checkpoint,
	}
	if err := store.BeginPublication(pending); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPublicationCommit(pending.ID, "orders", "commit"); err == nil {
		t.Fatal("applying publication accepted a commit")
	}
	if err := store.AbortPublication(pending.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginPublication(pending); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPublicationReady(pending.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.AbortPublication(pending.ID); err == nil {
		t.Fatal("ready publication was aborted")
	}
}

func TestAbortingPublicationRestoresPreApplyControlStateAfterRestart(t *testing.T) {
	directory := t.TempDir()
	configuration := testConfiguration()
	store, err := Open(directory, configuration)
	if err != nil {
		t.Fatal(err)
	}
	mapping := CollectionMapping{
		SourceUUID: "source-original", Database: "orders", Collection: "items", LocalUUID: "local-original",
		CreateOpTime: opTime(1), LastUpdateOpTime: opTime(1),
	}
	if err := store.PutCollectionMapping(mapping); err != nil {
		t.Fatal(err)
	}
	fragment := TransactionFragment{Key: "transaction", First: opTime(1), Last: opTime(1), Payload: []byte{1, 2, 3}}
	if err := store.PutTransactionFragment(fragment); err != nil {
		t.Fatal(err)
	}
	ownership := AuthOwnership{
		Namespace: "admin.system.users", Identity: "orders.reader", Owner: "rs0", LastUpdateOpTime: opTime(1),
	}
	if err := store.PutAuthOwnership(ownership); err != nil {
		t.Fatal(err)
	}
	metadata := ReplicationMetadataRecord{
		Kind: MetadataTransaction, Namespace: "config.transactions", Key: "session",
		Document: []byte{4, 5, 6}, LastUpdateOpTime: opTime(1),
	}
	if err := store.PutReplicationMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	want := store.Snapshot()

	position := opTime(2)
	pending := PendingPublication{
		ID: "applying-control-state", First: position, Last: position,
		Databases: []string{"orders"}, Commits: make(map[string]string),
		Checkpoint: Checkpoint{Fetched: position, Buffered: position, Written: position, Durable: position, Applied: position},
	}
	if err := store.BeginPublication(pending); err != nil {
		t.Fatal(err)
	}
	if err := store.RenameCollectionMapping(mapping.SourceUUID, "orders", "items", "orders", "renamed", position); err != nil {
		t.Fatal(err)
	}
	if err := store.PutCollectionMapping(CollectionMapping{
		SourceUUID: "source-created", Database: "orders", Collection: "created", LocalUUID: "local-created",
		CreateOpTime: position, LastUpdateOpTime: position,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTransactionFragment(fragment.Key); err != nil {
		t.Fatal(err)
	}
	if err := store.DropAuthOwnership(ownership.Namespace, ownership.Identity, ownership.Owner, position); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteReplicationMetadata(metadata.Kind, metadata.Key, position); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(directory, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.AbortPublication(pending.ID); err != nil {
		t.Fatal(err)
	}
	got := reopened.Snapshot()
	if got.PendingPublication != nil {
		t.Fatal("aborted publication remained pending")
	}
	if !reflect.DeepEqual(got.CollectionMappings, want.CollectionMappings) ||
		!reflect.DeepEqual(got.TransactionParts, want.TransactionParts) ||
		!reflect.DeepEqual(got.AuthOwnership, want.AuthOwnership) ||
		!reflect.DeepEqual(got.ReplicationMetadata, want.ReplicationMetadata) {
		t.Fatalf("control state after abort = %+v, want %+v", got, want)
	}
}
