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

func TestRollbackTruncatesCommitJournalAndSurvivesRestart(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	intervals := []CommitInterval{
		{First: opTime(1), Last: opTime(1), CommitID: "publication-one", Commits: []DatabaseCommit{{Database: "db", CommitID: "db-one"}}},
		{First: opTime(2), Last: opTime(2), CommitID: "publication-two", Commits: []DatabaseCommit{{Database: "db", CommitID: "db-two"}}},
		{First: opTime(3), Last: opTime(3), CommitID: "publication-three", Commits: []DatabaseCommit{{Database: "db", CommitID: "db-three"}}},
	}
	for _, interval := range intervals {
		checkpoint := Checkpoint{
			Fetched: interval.Last, Buffered: interval.Last, Written: interval.Last,
			Durable: interval.Last, Applied: interval.Last,
		}
		if err := store.PublishCommit(interval, checkpoint); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutCollectionMapping(CollectionMapping{
		SourceUUID: "retained", Database: "db", Collection: "retained", LocalUUID: "local-retained",
		CreateOpTime: opTime(1), LastUpdateOpTime: opTime(1),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutCollectionMapping(CollectionMapping{
		SourceUUID: "abandoned", Database: "db", Collection: "abandoned", LocalUUID: "local-abandoned",
		CreateOpTime: opTime(3), LastUpdateOpTime: opTime(3),
	}); err != nil {
		t.Fatal(err)
	}
	attempt := RollbackAttempt{
		ID: "rollback-one", Source: "source.example:27017", SourceRBID: 4,
		CommitID: intervals[1].CommitID, OpTime: intervals[1].Last, AuditBranch: "mongo-rollback-test",
		Databases: []RollbackDatabase{{Database: "db", CommitID: "db-two"}},
	}
	if err := store.BeginRollback(attempt); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := store.RollbackTo(attempt.CommitID, attempt.Source, attempt.SourceRBID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Applied != intervals[1].Last || len(store.CommitIntervals()) != 2 {
		t.Fatalf("rollback checkpoint = %+v, intervals = %+v", checkpoint, store.CommitIntervals())
	}
	if _, ok := store.PendingRollback(); ok {
		t.Fatal("completed rollback remained pending")
	}
	if _, ok := store.CommitFor(intervals[2].Last); ok {
		t.Fatal("abandoned interval remained in active provenance")
	}
	if _, ok := store.CollectionMapping("retained"); !ok {
		t.Fatal("mapping present at common point was removed")
	}
	if _, ok := store.CollectionMapping("abandoned"); ok {
		t.Fatal("mapping created after common point was retained")
	}

	reopened, err := Open(directory, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Checkpoint != checkpoint || len(reopened.CommitIntervals()) != 2 {
		t.Fatalf("reopened rollback state = %+v", reopened.Snapshot())
	}
}

func TestRollbackSafetyRejectsControlStateChangedAfterCommonPoint(t *testing.T) {
	store, err := Open(t.TempDir(), testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	mapping := CollectionMapping{
		SourceUUID: "source", Database: "db", Collection: "events", LocalUUID: "local",
		CreateOpTime: opTime(1), LastUpdateOpTime: opTime(3),
	}
	if err := store.PutCollectionMapping(mapping); err != nil {
		t.Fatal(err)
	}
	if store.CanRollbackTo(opTime(2)) {
		t.Fatal("rollback accepted a collection mapping changed after the common point")
	}
	if !store.CanRollbackTo(opTime(3)) {
		t.Fatal("rollback rejected current collection mapping state")
	}
}

func TestRollbackResumesAfterJournalTruncationBeforeStateUpdate(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	intervals := []CommitInterval{
		{First: opTime(1), Last: opTime(1), CommitID: "publication-one", Commits: []DatabaseCommit{{Database: "db", CommitID: "db-one"}}},
		{First: opTime(2), Last: opTime(2), CommitID: "publication-two", Commits: []DatabaseCommit{{Database: "db", CommitID: "db-two"}}},
		{First: opTime(3), Last: opTime(3), CommitID: "publication-three", Commits: []DatabaseCommit{{Database: "db", CommitID: "db-three"}}},
	}
	for _, interval := range intervals {
		checkpoint := Checkpoint{
			Fetched: interval.Last, Buffered: interval.Last, Written: interval.Last,
			Durable: interval.Last, Applied: interval.Last,
		}
		if err := store.PublishCommit(interval, checkpoint); err != nil {
			t.Fatal(err)
		}
	}
	attempt := RollbackAttempt{
		ID: "rollback-interrupted", Source: "source.example:27017", SourceRBID: 7,
		CommitID: intervals[1].CommitID, OpTime: intervals[1].Last, AuditBranch: "mongo-rollback-interrupted",
		Databases: []RollbackDatabase{{Database: "db", CommitID: "db-two"}},
	}
	if err := store.BeginRollback(attempt); err != nil {
		t.Fatal(err)
	}
	rollbackCheckpoint := Checkpoint{
		Fetched: attempt.OpTime, Buffered: attempt.OpTime, Written: attempt.OpTime,
		Durable: attempt.OpTime, Applied: attempt.OpTime,
	}
	store.mu.Lock()
	err = store.replaceCommitLogWithCheckpointLocked(store.state.CommitLogGeneration, intervals[:2], rollbackCheckpoint)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(directory, testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	if pending, ok := reopened.PendingRollback(); !ok || pending.ID != attempt.ID {
		t.Fatalf("pending rollback after interrupted publication = %+v, %v", pending, ok)
	}
	if len(reopened.CommitIntervals()) != 2 {
		t.Fatalf("reopened intervals = %+v, want two", reopened.CommitIntervals())
	}
	checkpoint, err := reopened.RollbackTo(attempt.CommitID, attempt.Source, attempt.SourceRBID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint != rollbackCheckpoint {
		t.Fatalf("completed checkpoint = %+v, want %+v", checkpoint, rollbackCheckpoint)
	}
}
