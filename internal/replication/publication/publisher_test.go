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

package publication

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/topology"
)

func TestPublisherResumesPartialMultiDatabasePublication(t *testing.T) {
	dir := t.TempDir()
	store := openPublicationStore(t, dir)
	manager := topology.New(store)
	backend := newFakeCommitBackend()
	backend.failDatabase = "orders"
	publisher, err := NewPublisher(backend, store, manager)
	if err != nil {
		t.Fatal(err)
	}
	first, last, checkpoint := publicationPositions()
	if err := publisher.Publish(context.Background(), first, last, []string{"orders", "accounts", "orders"}, checkpoint); err == nil {
		t.Fatal("publication unexpectedly succeeded")
	}
	pending, ok := store.PendingPublication()
	if !ok || pending.Commits["accounts"] == "" || pending.Commits["orders"] != "" {
		t.Fatalf("partial publication = %+v, %v", pending, ok)
	}

	reopened := openPublicationStore(t, dir)
	recoveredManager := topology.New(reopened)
	backend.failDatabase = ""
	recoveredPublisher, err := NewPublisher(backend, reopened, recoveredManager)
	if err != nil {
		t.Fatal(err)
	}
	if err := recoveredPublisher.Publish(context.Background(), first, last, []string{"accounts", "orders"}, checkpoint); err != nil {
		t.Fatal(err)
	}
	if backend.commitCalls["accounts"] != 1 || backend.commitCalls["orders"] != 1 {
		t.Fatalf("commit calls = %+v, want one per database", backend.commitCalls)
	}
	interval, ok := reopened.CommitFor(last)
	if !ok || len(interval.Commits) != 2 {
		t.Fatalf("published interval = %+v, %v", interval, ok)
	}
	if _, ok := reopened.PendingPublication(); ok {
		t.Fatal("publication remained pending")
	}
	if got, ok := reopened.CommitForDatabaseCommit("accounts", "accounts-commit-1"); !ok || got.CommitID != interval.CommitID {
		t.Fatalf("accounts commit provenance = %+v, %v", got, ok)
	}
}

func TestPublisherRecoversAmbiguousCommitResultFromHead(t *testing.T) {
	store := openPublicationStore(t, t.TempDir())
	manager := topology.New(store)
	backend := newFakeCommitBackend()
	backend.ambiguousDatabase = "orders"
	publisher, err := NewPublisher(backend, store, manager)
	if err != nil {
		t.Fatal(err)
	}
	first, last, checkpoint := publicationPositions()
	if err := publisher.Publish(context.Background(), first, last, []string{"orders"}, checkpoint); err == nil {
		t.Fatal("ambiguous publication unexpectedly succeeded")
	}
	backend.ambiguousDatabase = ""
	if err := publisher.Publish(context.Background(), first, last, []string{"orders"}, checkpoint); err != nil {
		t.Fatal(err)
	}
	if backend.commitCalls["orders"] != 1 {
		t.Fatalf("orders commit calls = %d, want 1", backend.commitCalls["orders"])
	}
}

func publicationPositions() (control.OpTime, control.OpTime, control.Checkpoint) {
	first := control.OpTime{Seconds: 100, Increment: 1, Term: 3}
	last := control.OpTime{Seconds: 100, Increment: 5, Term: 3}
	fetched := control.OpTime{Seconds: 100, Increment: 8, Term: 3}
	return first, last, control.Checkpoint{
		Fetched: fetched, Buffered: fetched, Written: last, Durable: last, Applied: last,
	}
}

func openPublicationStore(t *testing.T, dir string) *control.Store {
	t.Helper()
	store, err := control.Open(dir, control.Configuration{SetName: "rs0", MemberHost: "dumbo.example:27017"})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type fakeCommitBackend struct {
	heads             map[string]backends.CommitInfo
	commitCalls       map[string]int
	failDatabase      string
	ambiguousDatabase string
}

func newFakeCommitBackend() *fakeCommitBackend {
	return &fakeCommitBackend{heads: make(map[string]backends.CommitInfo), commitCalls: make(map[string]int)}
}

func (b *fakeCommitBackend) DumboDBCommit(_ context.Context, params *backends.CommitParams) (*backends.CommitResult, error) {
	if params.DBName == b.failDatabase {
		return nil, errors.New("injected commit failure")
	}
	b.commitCalls[params.DBName]++
	commitID := fmt.Sprintf("%s-commit-%d", params.DBName, b.commitCalls[params.DBName])
	b.heads[params.DBName] = backends.CommitInfo{CommitID: commitID, Message: params.Message}
	if params.DBName == b.ambiguousDatabase {
		return nil, errors.New("injected ambiguous result")
	}
	return &backends.CommitResult{CommitID: commitID}, nil
}

func (b *fakeCommitBackend) DumboDBLog(_ context.Context, params *backends.LogParams) (*backends.LogResult, error) {
	if head, ok := b.heads[params.DBName]; ok {
		return &backends.LogResult{Commits: []backends.CommitInfo{head}}, nil
	}
	return &backends.LogResult{}, nil
}
