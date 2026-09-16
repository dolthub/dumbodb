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

package catalog

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestCatalogApplierPreservesIdentityAcrossLifecycle(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	backend, err := dolt.NewBackend(dataDir, slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	store := testCatalogStore(t, backend)
	applier, err := NewApplier(backend, store)
	if err != nil {
		t.Fatal(err)
	}
	validator := must.NotFail(types.NewDocument("required", must.NotFail(types.NewDocument("$exists", true))))
	created, err := applier.Create(ctx, "orders", backends.CreateCollectionParams{
		Name: "items", Validator: validator, ValidationLevel: "strict", ValidationAction: "error",
	}, "source-one", catalogOpTime(1))
	if err != nil {
		t.Fatal(err)
	}
	if created.LocalUUID == "" || created.SourceUUID != "source-one" {
		t.Fatalf("created location = %+v", created)
	}
	indexes, err := PreflightIndexes("orders", "items", []*types.Document{
		must.NotFail(types.NewDocument("v", int32(2), "name", "_id_", "key", must.NotFail(types.NewDocument("_id", int32(1))))),
		must.NotFail(types.NewDocument("v", int32(2), "name", "account_1", "key", must.NotFail(types.NewDocument("account", int32(1))), "unique", true, "sparse", true)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := applier.CreateIndexes(ctx, "source-one", indexes); err != nil {
		t.Fatal(err)
	}
	if err := applier.Rename(ctx, "source-one", "orders", "renamed", catalogOpTime(2)); err != nil {
		t.Fatal(err)
	}
	renamed, err := applier.Resolve(ctx, "source-one")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Collection != "renamed" || renamed.LocalUUID != created.LocalUUID {
		t.Fatalf("renamed location = %+v", renamed)
	}
	database, err := backend.Database("orders")
	if err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection("renamed")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := collection.ListIndexes(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Indexes) != 2 || listed.Indexes[1].Name != "account_1" || !listed.Indexes[1].Unique || !listed.Indexes[1].Sparse {
		t.Fatalf("replicated indexes = %+v", listed.Indexes)
	}
	if err := applier.DropIndexes(ctx, "source-one", []string{"account_1"}); err != nil {
		t.Fatal(err)
	}
	if err := applier.CollMod(ctx, "source-one", backends.CollModParams{
		ValidationAction: "warn",
	}); err != nil {
		t.Fatal(err)
	}
	if err := applier.Drop(ctx, "source-one", catalogOpTime(3)); err != nil {
		t.Fatal(err)
	}
	if _, err := applier.Resolve(ctx, "source-one"); !errors.Is(err, ErrSourceUUIDNotFound) {
		t.Fatalf("resolve dropped source error = %v", err)
	}
	second, err := applier.Create(ctx, "orders", backends.CreateCollectionParams{Name: "renamed"}, "source-two", catalogOpTime(4))
	if err != nil {
		t.Fatal(err)
	}
	if second.LocalUUID == created.LocalUUID {
		t.Fatalf("name reuse retained old local UUID %q", second.LocalUUID)
	}
	if second.SourceUUID == created.SourceUUID {
		t.Fatal("name reuse retained old source UUID")
	}
	backend.Close()

	reopened, err := dolt.NewBackend(dataDir, slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, err := Resolve(ctx, reopened, "source-two")
	if err != nil {
		t.Fatal(err)
	}
	if recovered != second {
		t.Fatalf("recovered location = %+v, want %+v", recovered, second)
	}
}

func TestCatalogCollModMaterializesDefaultValidationAction(t *testing.T) {
	ctx := context.Background()
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	applier, err := NewApplier(backend, testCatalogStore(t, backend))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.Create(ctx, "orders", backends.CreateCollectionParams{Name: "items"}, "source-one", catalogOpTime(1)); err != nil {
		t.Fatal(err)
	}
	if err := applier.CollMod(ctx, "source-one", backends.CollModParams{ValidationLevel: "moderate"}); err != nil {
		t.Fatal(err)
	}
	database, err := backend.Database("orders")
	if err != nil {
		t.Fatal(err)
	}
	collections, err := database.ListCollections(ctx, &backends.ListCollectionsParams{Name: "items"})
	if err != nil {
		t.Fatal(err)
	}
	if len(collections.Collections) != 1 || collections.Collections[0].ValidationAction != "error" {
		t.Fatalf("collection metadata = %+v", collections.Collections)
	}
}

func TestResolveRejectsDuplicateSourceUUIDAcrossDatabases(t *testing.T) {
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	ctx := context.Background()
	for _, databaseName := range []string{"one", "two"} {
		database, err := backend.Database(databaseName)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items", SourceUUID: "duplicate"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Resolve(ctx, backend, "duplicate"); !errors.Is(err, ErrSourceUUIDAmbiguous) {
		t.Fatalf("Resolve error = %v", err)
	}
}

func TestCatalogApplierAppliesViewLifecycle(t *testing.T) {
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	applier, err := NewApplier(backend, testCatalogStore(t, backend))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pipeline := must.NotFail(types.NewArray(must.NotFail(types.NewDocument("$match", must.NotFail(types.NewDocument("active", true))))))
	if err := applier.CreateView(ctx, "orders", backends.CreateCollectionParams{
		Name: "active_orders", ViewOn: "items", ViewPipeline: pipeline,
	}); err != nil {
		t.Fatal(err)
	}
	updated := must.NotFail(types.NewArray(must.NotFail(types.NewDocument("$match", must.NotFail(types.NewDocument("active", false))))))
	if err := applier.CollModView(ctx, "orders", backends.CollModParams{
		Name: "active_orders", SetView: true, ViewPipeline: updated, SetViewPipeline: true,
	}); err != nil {
		t.Fatal(err)
	}
	database, err := backend.Database("orders")
	if err != nil {
		t.Fatal(err)
	}
	collections, err := database.ListCollections(ctx, &backends.ListCollectionsParams{Name: "active_orders"})
	if err != nil {
		t.Fatal(err)
	}
	if len(collections.Collections) != 1 || !collections.Collections[0].IsView || collections.Collections[0].ViewPipeline.Len() != 1 {
		t.Fatalf("view metadata = %+v", collections.Collections)
	}
	if err := applier.DropView(ctx, "orders", "active_orders"); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogApplierRepairsCompletedRootMutation(t *testing.T) {
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	store := testCatalogStore(t, backend)
	applier, err := NewApplier(backend, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := backend.Database("orders")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "items", SourceUUID: "source-one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := applier.Create(ctx, "orders", backends.CreateCollectionParams{Name: "items"}, "source-one", catalogOpTime(1)); err != nil {
		t.Fatalf("repair create mapping: %v", err)
	}
	if err := database.RenameCollection(ctx, &backends.RenameCollectionParams{OldName: "items", NewName: "renamed"}); err != nil {
		t.Fatal(err)
	}
	if err := applier.Rename(ctx, "source-one", "orders", "renamed", catalogOpTime(2)); err != nil {
		t.Fatalf("repair rename mapping: %v", err)
	}
	if err := database.DropCollection(ctx, &backends.DropCollectionParams{Name: "renamed"}); err != nil {
		t.Fatal(err)
	}
	if err := applier.Drop(ctx, "source-one", catalogOpTime(3)); err != nil {
		t.Fatalf("repair drop mapping: %v", err)
	}
}

func testCatalogStore(t *testing.T, backend backends.Backend) *control.Store {
	t.Helper()
	store, err := control.Open(backend, control.Configuration{
		SetName: "rs0", MemberHost: "dumbo.example:27017",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func catalogOpTime(increment uint32) control.OpTime {
	return control.OpTime{Seconds: 100, Increment: increment, Term: 8}
}
