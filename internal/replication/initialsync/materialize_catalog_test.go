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
	"log/slog"
	"testing"

	"github.com/FerretDB/wire"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/replication/catalog"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestMaterializeOrdinaryCatalogClonesMultipleDatabasesAndReportsSpecialDatabases(t *testing.T) {
	ctx := context.Background()
	backend, catalogApplier := newMaterializeCatalogTestApplier(t)
	firstUUID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	secondUUID := uuid.MustParse("87654321-4321-4321-8321-cba987654321")
	client := &boundaryClient{responses: []*wire.OpMsg{
		cloneCursorResponse(t, "firstBatch", 0, "accounts.customers", must.NotFail(types.NewDocument("$recordId", int64(1))),
			must.NotFail(types.NewDocument("_id", int32(1), "name", "Ada")),
		),
		cloneCursorResponse(t, "firstBatch", 0, "orders.items", must.NotFail(types.NewDocument("$recordId", int64(1))),
			must.NotFail(types.NewDocument("_id", int32(7), "sku", "book")),
		),
	}}
	databases := []Database{
		{Name: "admin", Special: true, Collections: []Collection{{Name: "system.users"}}},
		{Name: "accounts", Collections: []Collection{cloneTestCollection("customers", firstUUID)}},
		{Name: "config", Special: true, Collections: []Collection{{Name: "transactions"}}},
		{Name: "orders", Collections: []Collection{cloneTestCollection("items", secondUUID)}},
	}
	result, err := MaterializeOrdinaryCatalog(ctx, client, catalogApplier, databases,
		control.OpTime{Seconds: 100, Increment: 2, Term: 8}, LoaderLimits{Documents: 10, Bytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Collections) != 2 || len(result.SpecialDatabases) != 2 {
		t.Fatalf("catalog materialization = %+v", result)
	}
	if result.SpecialDatabases[0].Name != "admin" || result.SpecialDatabases[1].Name != "config" {
		t.Fatalf("special databases = %+v", result.SpecialDatabases)
	}
	assertBranchCollectionCount(t, ctx, backend, "accounts", "customers", 1)
	assertBranchCollectionCount(t, ctx, backend, "orders", "items", 1)
	if len(client.requests) != 2 {
		t.Fatalf("clone request count = %d, want 2", len(client.requests))
	}
	assertCommand(t, client.requests[0], "find", "accounts")
	assertCommand(t, client.requests[1], "find", "orders")
}

func TestMaterializeOrdinaryCatalogPreflightsBeforeMutation(t *testing.T) {
	ctx := context.Background()
	backend, catalogApplier := newMaterializeCatalogTestApplier(t)
	firstUUID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	secondUUID := uuid.MustParse("87654321-4321-4321-8321-cba987654321")
	unsupported := cloneTestCollection("events", secondUUID)
	unsupported.Options = must.NotFail(types.NewDocument("capped", true, "size", int64(1024)))
	_, err := MaterializeOrdinaryCatalog(ctx, &boundaryClient{}, catalogApplier, []Database{
		{Name: "accounts", Collections: []Collection{cloneTestCollection("customers", firstUUID)}},
		{Name: "logs", Collections: []Collection{unsupported}},
	}, control.OpTime{Seconds: 100, Increment: 2, Term: 8}, LoaderLimits{Documents: 10, Bytes: 4096})
	if err == nil {
		t.Fatal("expected unsupported catalog error")
	}
	branchBackend := backend.(backends.ReplicationBranchBackend)
	exists, branchErr := branchBackend.ReplicationBranchExists(ctx, "accounts", "mongo-history")
	if branchErr != nil {
		t.Fatal(branchErr)
	}
	if exists {
		t.Fatal("preflight failure created the first database branch")
	}
}

func newMaterializeCatalogTestApplier(t *testing.T) (backends.Backend, *catalog.Applier) {
	t.Helper()
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.Close)
	store, err := control.Open(t.TempDir(), control.Configuration{SetName: "rs0", Branch: "mongo-history", MemberHost: "dumbo:27017"})
	if err != nil {
		t.Fatal(err)
	}
	catalogApplier, err := catalog.NewApplier(backend, store)
	if err != nil {
		t.Fatal(err)
	}
	return backend, catalogApplier
}

func cloneTestCollection(name string, sourceUUID uuid.UUID) Collection {
	return Collection{
		Name: name, SourceUUID: sourceUUID.String(), UUIDBinary: types.Binary{Subtype: types.BinaryUUID, B: sourceUUID[:]},
		Options: must.NotFail(types.NewDocument()),
		Indexes: []*types.Document{
			must.NotFail(types.NewDocument("v", int32(2), "key", must.NotFail(types.NewDocument("_id", int32(1))), "name", "_id_")),
		},
	}
}

func assertBranchCollectionCount(t *testing.T, ctx context.Context, backend backends.Backend, databaseName, collectionName string, want int) {
	t.Helper()
	database, err := backend.Database(databaseName + "@mongo-history")
	if err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection(collectionName)
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentCount(t, ctx, collection, want)
}
