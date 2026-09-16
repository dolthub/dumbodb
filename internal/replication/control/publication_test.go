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
	if err := store.PublishCommit(CommitInterval{First: pending.First, Last: last, CommitID: pending.ID}, checkpoint); err == nil {
		t.Fatal("PublishCommit completed without the database commit")
	}
	if err := store.RecordPublicationCommit(pending.ID, "other", "commit"); err == nil {
		t.Fatal("RecordPublicationCommit accepted an unrelated database")
	}
}
