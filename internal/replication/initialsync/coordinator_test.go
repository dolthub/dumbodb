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

package initialsync

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/FerretDB/wire"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/replication/catalog"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/oplog"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestCoordinatorRunsCloneAndConcurrentCatchUpThroughStop(t *testing.T) {
	ctx := context.Background()
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	existingDatabase, err := backend.Database("orders")
	if err != nil {
		t.Fatal(err)
	}
	if err := existingDatabase.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "standalone_data"}); err != nil {
		t.Fatal(err)
	}
	store, err := control.Open(t.TempDir(), control.Configuration{
		SetName: "rs0", MemberHost: "dumbo.example:27017",
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := initialSyncTestManager(t, store)
	catalogApplier, err := catalog.NewApplier(backend, store)
	if err != nil {
		t.Fatal(err)
	}
	buffer := must.NotFail(oplog.NewBuffer(oplog.BufferLimits{Entries: 10, Bytes: 4096}))
	fetcher := &coordinatorFetcher{buffer: buffer, manager: manager, expected: catchUpOpTime(1)}
	entryApplier := &recordingEntryApplier{}
	publisher := &recordingInitialSyncPublisher{}
	sourceUUID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	databases := must.NotFail(types.NewArray(must.NotFail(types.NewDocument("name", "orders"))))
	collectionInfo := must.NotFail(types.NewDocument(
		"name", "items", "type", "collection", "options", must.NotFail(types.NewDocument()),
		"info", must.NotFail(types.NewDocument("readOnly", false, "uuid", types.Binary{Subtype: types.BinaryUUID, B: sourceUUID[:]})),
	))
	index := must.NotFail(types.NewDocument("v", int32(2), "key", must.NotFail(types.NewDocument("_id", int32(1))), "name", "_id_"))
	client := &boundaryClient{responses: []*wire.OpMsg{
		responseMessage(t, must.NotFail(types.NewDocument("rbid", int32(17), "ok", float64(1)))),
		cursorResponse(t, oplogBoundaryDocument(1, 8)),
		cursorResponse(t),
		cursorResponse(t, oplogBoundaryDocument(2, 8)),
		cursorResponse(t),
		responseMessage(t, must.NotFail(types.NewDocument("databases", databases, "ok", float64(1)))),
		cursorResponse(t, collectionInfo),
		cursorResponse(t, index),
		cloneCursorResponse(t, "firstBatch", 0, "orders.items", must.NotFail(types.NewDocument("$recordId", int64(1))),
			must.NotFail(types.NewDocument("_id", int32(7), "sku", "book")),
		),
		cursorResponse(t, oplogBoundaryDocument(4, 8)),
		responseMessage(t, must.NotFail(types.NewDocument("rbid", int32(17), "ok", float64(1)))),
	}}
	coordinator, err := NewCoordinator(CoordinatorOptions{
		Source: "primary.example:27017", Client: client, Store: store, Manager: manager,
		Fetcher: fetcher, Buffer: buffer, Resetter: backend.(backends.InitialSyncResetter),
		Catalog: catalogApplier, Applier: entryApplier, Publisher: publisher,
		LoaderLimits: LoaderLimits{Documents: 10, Bytes: 4096}, CatchUpLimits: CatchUpLimits{Entries: 2, Bytes: 4096},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempt.BeginFetch != catchUpOpTime(1) || result.Attempt.BeginApply != catchUpOpTime(2) || result.Attempt.Stop != catchUpOpTime(4) {
		t.Fatalf("initial sync boundaries = %+v", result.Attempt)
	}
	if result.CatchUp.Applied != 2 || len(entryApplier.entries) != 2 || entryApplier.entries[0].OpTime != catchUpOpTime(3) {
		t.Fatalf("catch-up = %+v, entries = %+v", result.CatchUp, entryApplier.entries)
	}
	state := store.Snapshot()
	if state.InitialSyncPhase != control.InitialSyncComplete || state.Checkpoint.Applied != catchUpOpTime(4) || state.CurrentRBID != 17 {
		t.Fatalf("completed control state = %+v", state)
	}
	if manager.Snapshot().State != topology.StateSecondary {
		t.Fatalf("member state = %s", manager.Snapshot().State)
	}
	if len(publisher.publications) != 1 || publisher.publications[0].Checkpoint.Applied != catchUpOpTime(4) {
		t.Fatalf("publications = %+v", publisher.publications)
	}
	assertMainCollectionCount(t, ctx, backend, "orders", "items", 1)
	collections, err := existingDatabase.ListCollections(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(collections.Collections) != 1 || collections.Collections[0].Name != "items" {
		t.Fatalf("initial sync collections = %+v, want only items", collections.Collections)
	}
	if err := coordinator.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	resetState := store.Snapshot()
	if resetState.InitialSyncPhase != control.InitialSyncNotStarted || resetState.InitialSyncAttempt != nil ||
		len(resetState.CollectionMappings) != 0 || resetState.Checkpoint != (control.Checkpoint{}) {
		t.Fatalf("reset control state = %+v", resetState)
	}
	if manager.Snapshot().State != topology.StateStartup2 {
		t.Fatalf("member state after reset = %s", manager.Snapshot().State)
	}
	resetDatabase, err := backend.Database("orders")
	if err != nil {
		t.Fatal(err)
	}
	resetCollections, err := resetDatabase.ListCollections(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resetCollections.Collections) != 0 {
		t.Fatalf("reset replica collections = %+v", resetCollections.Collections)
	}
}

func TestCoordinatorPreservesDataUntilSourceIsValidated(t *testing.T) {
	ctx := context.Background()
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	database, err := backend.Database("orders")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "standalone_data"}); err != nil {
		t.Fatal(err)
	}
	store, err := control.Open(t.TempDir(), control.Configuration{SetName: "rs0", MemberHost: "dumbo.example:27017"})
	if err != nil {
		t.Fatal(err)
	}
	catalogApplier, err := catalog.NewApplier(backend, store)
	if err != nil {
		t.Fatal(err)
	}
	buffer := must.NotFail(oplog.NewBuffer(oplog.BufferLimits{Entries: 10, Bytes: 4096}))
	coordinator, err := NewCoordinator(CoordinatorOptions{
		Source: "primary.example:27017", Client: failingInitialSyncClient{}, Store: store,
		Manager: initialSyncTestManager(t, store), Fetcher: &coordinatorFetcher{}, Buffer: buffer,
		Resetter: backend.(backends.InitialSyncResetter), Catalog: catalogApplier,
		Applier: &recordingEntryApplier{}, Publisher: &recordingInitialSyncPublisher{},
		LoaderLimits: LoaderLimits{Documents: 10, Bytes: 4096}, CatchUpLimits: CatchUpLimits{Entries: 2, Bytes: 4096},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Run(ctx); err == nil {
		t.Fatal("initial sync accepted an invalid source")
	}
	collections, err := database.ListCollections(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(collections.Collections) != 1 || collections.Collections[0].Name != "standalone_data" {
		t.Fatalf("failed source validation changed main: %+v", collections.Collections)
	}
}

type failingInitialSyncClient struct{}

func (failingInitialSyncClient) Request(context.Context, *wire.OpMsg) (*wire.OpMsg, error) {
	return nil, errors.New("source unavailable")
}

type recordingInitialSyncPublisher struct {
	publications []InitialSyncPublication
}

func (p *recordingInitialSyncPublisher) PublishInitialSync(_ context.Context, publication InitialSyncPublication) error {
	p.publications = append(p.publications, publication)
	return nil
}

type coordinatorFetcher struct {
	buffer   *oplog.Buffer
	manager  *topology.Manager
	expected control.OpTime
}

func (f *coordinatorFetcher) FetchFrom(ctx context.Context, position control.OpTime) error {
	if position != f.expected {
		return errors.New("unexpected initial sync fetch position")
	}
	for increment := uint32(2); increment <= 4; increment++ {
		entry := catchUpEntry(increment)
		if err := f.buffer.TryAppend(entry); err != nil {
			return err
		}
		if err := f.manager.AdvanceFetched(entry.OpTime, entry.OpTime); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func initialSyncTestManager(t *testing.T, store *control.Store) *topology.Manager {
	t.Helper()
	manager := topology.New(store)
	configuration := control.ReplicaConfiguration{
		SetName: "rs0", Version: 4, Term: 3, ProtocolVersion: 1, ReplicaSetID: "0102030405060708090a0b0c",
		Members: []control.MemberConfiguration{
			{MemberID: 1, Host: "primary.example:27017", Priority: 1, Votes: 1},
			{MemberID: 3, Host: "dumbo.example:27017", Hidden: true, Priority: 0, Votes: 0},
		},
	}
	if err := manager.InstallConfiguration(configuration, 8); err != nil {
		t.Fatal(err)
	}
	if err := manager.ObserveHeartbeat("primary.example:27017", topology.Heartbeat{
		SetName: "rs0", MemberID: 1, State: topology.StatePrimary, Term: 8, PrimaryID: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return manager
}
