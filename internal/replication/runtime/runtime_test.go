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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/initialsync"
	"github.com/dolthub/dumbodb/internal/replication/oplog"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestTerminalInitialSyncFailureClassifiesUnsupportedBSON(t *testing.T) {
	unsupported := &initialsync.UnsupportedBSONTypeError{
		Namespace: "archive.items",
		BSONType:  "JavaScript",
	}
	failure, terminal := terminalInitialSyncFailure(fmt.Errorf("initial sync: %w", unsupported))
	if !terminal || failure.Namespace != unsupported.Namespace || failure.BSONType != unsupported.BSONType || failure.Message != unsupported.Error() {
		t.Fatalf("terminal failure = %+v, %v", failure, terminal)
	}
	if failure, terminal := terminalInitialSyncFailure(errors.New("connection reset")); terminal || failure != (control.InitialSyncFailure{}) {
		t.Fatalf("transient failure = %+v, %v", failure, terminal)
	}
}

func TestApplyNoopAdvancesCheckpointWithoutCommit(t *testing.T) {
	ctx := context.Background()
	replicationRuntime, backend, store := newRecoveryRuntime(t)
	recoveryCollection(t, ctx, backend)
	commitRecoveryDatabase(t, ctx, backend, "base")
	versioned := backend.(backends.VersioningBackend)
	before, err := versioned.DumboDBLog(ctx, &backends.LogParams{DBName: "recovery", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	document := must.NotFail(types.NewDocument(
		"ts", types.Timestamp(uint64(100)<<32|1),
		"t", int64(2),
		"op", "n",
		"ns", "",
		"o", must.NotFail(types.NewDocument("msg", "periodic noop")),
	))
	entry, err := oplog.ParseEntry(document)
	if err != nil {
		t.Fatal(err)
	}
	replicationRuntime.counterOperationKinds = func(oplog.Entry) ([]string, error) {
		return nil, errors.New("injected counter failure")
	}
	if err := replicationRuntime.manager.AdvanceFetched(entry.OpTime, entry.OpTime); err != nil {
		t.Fatal(err)
	}
	applier, _, _, err := replicationRuntime.appliers()
	if err != nil {
		t.Fatal(err)
	}
	if err := replicationRuntime.applyEntry(ctx, applier, entry); err != nil {
		t.Fatal(err)
	}
	after, err := versioned.DumboDBLog(ctx, &backends.LogParams{DBName: "recovery", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Commits) != len(before.Commits) {
		t.Fatalf("noop changed commit count from %d to %d", len(before.Commits), len(after.Commits))
	}
	checkpoint := store.Snapshot().Checkpoint
	if checkpoint.Written != entry.OpTime || checkpoint.Durable != entry.OpTime || checkpoint.Applied != entry.OpTime {
		t.Fatalf("noop checkpoint = %+v, want applied %+v", checkpoint, entry.OpTime)
	}
	if _, ok := store.CommitFor(entry.OpTime); ok {
		t.Fatal("noop created commit provenance")
	}
	runtimeStatus := replicationRuntime.manager.Snapshot().Runtime
	if runtimeStatus.AppliedOperations != 1 || runtimeStatus.PublishedCommits != 0 {
		t.Fatalf("noop runtime counters = %+v", runtimeStatus)
	}
}

func TestRecoverPublicationDiscardsIncompleteApply(t *testing.T) {
	ctx := context.Background()
	runtime, backend, store := newRecoveryRuntime(t)
	collection := recoveryCollection(t, ctx, backend)
	insertRecoveryDocument(t, ctx, collection, 1)
	commitRecoveryDatabase(t, ctx, backend, "base")
	insertRecoveryDocument(t, ctx, collection, 2)

	position := control.OpTime{Seconds: 100, Increment: 2, Term: 1}
	checkpoint := control.Checkpoint{
		Fetched: position, Buffered: position, Written: position, Durable: position, Applied: position,
	}
	publicationID, err := runtime.publisher.Begin(position, position, []string{"recovery"}, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	mapping := control.CollectionMapping{
		SourceUUID: "created-during-apply", Database: "recovery", Collection: "events", LocalUUID: "local-created",
		CreateOpTime: position, LastUpdateOpTime: position,
	}
	if err := store.PutCollectionMapping(mapping); err != nil {
		t.Fatal(err)
	}
	if err := runtime.recoverPublication(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.PendingPublication(); ok {
		t.Fatal("incomplete publication remained pending")
	}
	if count := recoveryCount(t, ctx, collection); count != 1 {
		t.Fatalf("document count after recovering %s = %d, want 1", publicationID, count)
	}
	if _, ok := store.CollectionMapping(mapping.SourceUUID); ok {
		t.Fatal("control mapping from incomplete publication survived recovery")
	}
}

func TestRecoverPublicationCompletesReadyApply(t *testing.T) {
	ctx := context.Background()
	runtime, backend, store := newRecoveryRuntime(t)
	collection := recoveryCollection(t, ctx, backend)
	insertRecoveryDocument(t, ctx, collection, 1)
	commitRecoveryDatabase(t, ctx, backend, "base")
	insertRecoveryDocument(t, ctx, collection, 2)

	position := control.OpTime{Seconds: 100, Increment: 2, Term: 1}
	checkpoint := control.Checkpoint{
		Fetched: position, Buffered: position, Written: position, Durable: position, Applied: position,
	}
	publicationID, err := runtime.publisher.Begin(position, position, []string{"recovery"}, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.publisher.MarkReady(publicationID); err != nil {
		t.Fatal(err)
	}
	if err := runtime.recoverPublication(ctx); err != nil {
		t.Fatal(err)
	}
	if count := recoveryCount(t, ctx, collection); count != 2 {
		t.Fatalf("document count after ready recovery = %d, want 2", count)
	}
	if interval, ok := store.CommitFor(position); !ok || interval.CommitID != publicationID {
		t.Fatalf("recovered commit interval = %+v, %v", interval, ok)
	}
}

func newRecoveryRuntime(t *testing.T) (*Runtime, backends.Backend, *control.Store) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	backend, err := dolt.NewBackend(t.TempDir(), logger, false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.Close)
	store, err := control.Open(backend, control.Configuration{SetName: "rs0", MemberHost: "dumbo.example:27017"})
	if err != nil {
		t.Fatal(err)
	}
	manager := topology.New(store)
	runtime, err := New(backend, store, manager, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, backend, store
}

func recoveryCollection(t *testing.T, ctx context.Context, backend backends.Backend) backends.Collection {
	t.Helper()
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
	return collection
}

func insertRecoveryDocument(t *testing.T, ctx context.Context, collection backends.Collection, id int32) {
	t.Helper()
	document := must.NotFail(types.NewDocument("_id", id))
	if _, err := collection.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{document}}); err != nil {
		t.Fatal(err)
	}
}

func commitRecoveryDatabase(t *testing.T, ctx context.Context, backend backends.Backend, message string) {
	t.Helper()
	versioned := backend.(backends.VersioningBackend)
	if _, err := versioned.DumboDBCommit(ctx, &backends.CommitParams{
		DBName: "recovery", Branch: "main", Message: message, Author: "Test <test@example.com>",
	}); err != nil {
		t.Fatal(err)
	}
}

func recoveryCount(t *testing.T, ctx context.Context, collection backends.Collection) int64 {
	t.Helper()
	result, err := collection.Count(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result.Count
}
