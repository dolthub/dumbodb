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

import "testing"

func TestSpecialReplicationStateSurvivesRestartAndReset(t *testing.T) {
	directory := t.TempDir()
	configuration := testConfiguration()
	store, err := openTestStore(t, directory, configuration)
	if err != nil {
		t.Fatal(err)
	}
	auth := AuthOwnership{
		Namespace: "admin.system.users", Identity: "sales.ada", Owner: "rs0", LastUpdateOpTime: opTime(4),
	}
	if err := store.PutAuthOwnership(auth); err != nil {
		t.Fatal(err)
	}
	metadata := ReplicationMetadataRecord{
		Kind: MetadataTransaction, Namespace: "config.transactions", Key: "session-key",
		Document: []byte{1, 2, 3}, LastUpdateOpTime: opTime(5),
	}
	if err := store.PutReplicationMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openTestStore(t, directory, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.AuthOwnershipFor(auth.Namespace, auth.Identity); !ok || got != auth {
		t.Fatalf("auth ownership after restart = %+v, %v", got, ok)
	}
	gotMetadata, ok := reopened.ReplicationMetadataRecordFor(metadata.Kind, metadata.Key)
	if !ok || !metadataRecordsEqual(gotMetadata, metadata) {
		t.Fatalf("metadata after restart = %+v, %v", gotMetadata, ok)
	}
	if err := reopened.DropAuthOwnership(auth.Namespace, auth.Identity, auth.Owner, opTime(6)); err != nil {
		t.Fatal(err)
	}
	if err := reopened.DeleteReplicationMetadata(metadata.Kind, metadata.Key, opTime(6)); err != nil {
		t.Fatal(err)
	}
	if err := reopened.ResetInitialSync(""); err != nil {
		t.Fatal(err)
	}
	state := reopened.Snapshot()
	if len(state.AuthOwnership) != 0 || len(state.ReplicationMetadata) != 0 {
		t.Fatalf("special state survived reset: auth=%v metadata=%v", state.AuthOwnership, state.ReplicationMetadata)
	}
}

func TestSpecialReplicationStateRejectsOwnerAndHistoryConflicts(t *testing.T) {
	store, err := openTestStore(t, t.TempDir(), testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	record := AuthOwnership{
		Namespace: "admin.system.users", Identity: "sales.ada", Owner: "rs0", LastUpdateOpTime: opTime(4),
	}
	if err := store.PutAuthOwnership(record); err != nil {
		t.Fatal(err)
	}
	wrongOwner := record
	wrongOwner.Owner = "other"
	wrongOwner.LastUpdateOpTime = opTime(5)
	if err := store.PutAuthOwnership(wrongOwner); err == nil {
		t.Fatal("ownership transfer succeeded")
	}
	stale := record
	stale.LastUpdateOpTime = opTime(3)
	if err := store.PutAuthOwnership(stale); err == nil {
		t.Fatal("stale ownership update succeeded")
	}
}

func TestSpecialReplicationDeletesCreateTombstonesForInitialSyncRaces(t *testing.T) {
	store, err := openTestStore(t, t.TempDir(), testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DropAuthOwnership("admin.system.users", "sales.ada", "rs0", opTime(4)); err != nil {
		t.Fatal(err)
	}
	if record, ok := store.AuthOwnershipFor("admin.system.users", "sales.ada"); !ok || !record.Dropped {
		t.Fatalf("auth tombstone = %+v, %v", record, ok)
	}
	if err := store.DeleteReplicationMetadata(MetadataTransaction, "session-key", opTime(4)); err != nil {
		t.Fatal(err)
	}
	if record, ok := store.ReplicationMetadataRecordFor(MetadataTransaction, "session-key"); !ok || !record.Deleted {
		t.Fatalf("metadata tombstone = %+v, %v", record, ok)
	}
}
