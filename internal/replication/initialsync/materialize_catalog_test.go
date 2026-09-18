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
	"strings"
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
	var progress []MaterializationProgress
	result, err := MaterializeOrdinaryCatalog(ctx, client, catalogApplier, databases,
		control.OpTime{Seconds: 100, Increment: 2, Term: 8}, LoaderLimits{Documents: 10, Bytes: 4096},
		func(update MaterializationProgress) { progress = append(progress, update) })
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Collections) != 2 || len(result.SpecialDatabases) != 2 {
		t.Fatalf("catalog materialization = %+v", result)
	}
	if result.SpecialDatabases[0].Name != "admin" || result.SpecialDatabases[1].Name != "config" {
		t.Fatalf("special databases = %+v", result.SpecialDatabases)
	}
	assertMainCollectionCount(t, ctx, backend, "accounts", "customers", 1)
	assertMainCollectionCount(t, ctx, backend, "orders", "items", 1)
	if len(client.requests) != 2 {
		t.Fatalf("clone request count = %d, want 2", len(client.requests))
	}
	assertCommand(t, client.requests[0], "find", "accounts")
	assertCommand(t, client.requests[1], "find", "orders")
	if len(progress) == 0 {
		t.Fatal("catalog materialization did not report progress")
	}
	last := progress[len(progress)-1]
	if last.CollectionsTotal != 2 || last.CollectionsCompleted != 2 || last.Documents != 2 || last.CurrentNamespace != "" {
		t.Fatalf("final materialization progress = %+v", last)
	}
	activeNamespaceReported := false
	for _, update := range progress {
		if update.CurrentNamespace == "accounts.customers" && update.Documents == 1 {
			activeNamespaceReported = true
		}
	}
	if !activeNamespaceReported {
		t.Fatalf("materialization progress did not report active namespace: %+v", progress)
	}
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
	}, control.OpTime{Seconds: 100, Increment: 2, Term: 8}, LoaderLimits{Documents: 10, Bytes: 4096}, nil)
	if err == nil {
		t.Fatal("expected unsupported catalog error")
	}
	databases, listErr := backend.ListDatabases(ctx, nil)
	if listErr != nil {
		t.Fatal(listErr)
	}
	for _, database := range databases.Databases {
		if database.Name == "accounts" || database.Name == "logs" {
			t.Fatalf("preflight failure created database %q", database.Name)
		}
	}
}

func TestMaterializeOrdinaryCatalogRejectsUnsupportedCatalogKinds(t *testing.T) {
	tests := []struct {
		name       string
		collection Collection
		want       string
	}{
		{
			name: "view",
			collection: Collection{Name: "active_orders", Type: "view", Options: must.NotFail(types.NewDocument(
				"viewOn", "orders", "pipeline", must.NotFail(types.NewArray()),
			))},
			want: "replication rejected sales.active_orders option \"viewOn\": view replication is unsupported",
		},
		{
			name: "time series",
			collection: Collection{Name: "samples", Type: "timeseries", Options: must.NotFail(types.NewDocument(
				"timeseries", must.NotFail(types.NewDocument("timeField", "at", "granularity", "seconds")),
			))},
			want: "replication rejected sales.samples option \"timeseries\": time-series replication is unsupported",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			backend, catalogApplier := newMaterializeCatalogTestApplier(t)
			_, err := MaterializeOrdinaryCatalog(ctx, &boundaryClient{}, catalogApplier, []Database{
				{Name: "sales", Collections: []Collection{test.collection}},
			}, control.OpTime{Seconds: 100, Increment: 2, Term: 8}, LoaderLimits{Documents: 10, Bytes: 4096}, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			databases, listErr := backend.ListDatabases(ctx, nil)
			if listErr != nil {
				t.Fatal(listErr)
			}
			for _, database := range databases.Databases {
				if database.Name == "sales" {
					t.Fatal("unsupported catalog kind created sales database")
				}
			}
		})
	}
}

func newMaterializeCatalogTestApplier(t *testing.T) (backends.Backend, *catalog.Applier) {
	t.Helper()
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.Close)
	store, err := control.Open(backend, control.Configuration{SetName: "rs0", MemberHost: "dumbo:27017"})
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

func assertMainCollectionCount(t *testing.T, ctx context.Context, backend backends.Backend, databaseName, collectionName string, want int) {
	t.Helper()
	database, err := backend.Database(databaseName)
	if err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection(collectionName)
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentCount(t, ctx, collection, want)
}
