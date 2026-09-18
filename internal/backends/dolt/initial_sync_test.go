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

package dolt

import (
	"context"
	"log/slog"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestResetInitialSyncDataResetsMainToInitialCommit(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	backend, err := NewBackend(directory, slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	database, err := backend.Database("orders")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "partial_clone"}); err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection("partial_clone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collection.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "source", "mongodb")),
	}}); err != nil {
		t.Fatal(err)
	}
	versioning := backend.(backends.VersioningBackend)
	if _, err := versioning.DumboDBCommit(ctx, &backends.CommitParams{
		DBName: "orders", Branch: "main", Message: "partial initial sync", Author: "test",
	}); err != nil {
		t.Fatal(err)
	}
	customers, err := backend.Database("customers")
	if err != nil {
		t.Fatal(err)
	}
	if err := customers.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "uncommitted_clone"}); err != nil {
		t.Fatal(err)
	}
	controlCollection, err := backend.(backends.ReplicationControlBackend).ReplicationControlCollection()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlCollection.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{
		must.NotFail(types.NewDocument("_id", "control", "marker", "preserved")),
	}}); err != nil {
		t.Fatal(err)
	}
	admin, err := backend.Database("admin")
	if err != nil {
		t.Fatal(err)
	}
	users, err := admin.Collection("system.users")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{
		must.NotFail(types.NewDocument("_id", "source-user")),
	}}); err != nil {
		t.Fatal(err)
	}
	resetter := backend.(backends.InitialSyncResetter)
	if err := resetter.ResetInitialSyncData(ctx); err != nil {
		t.Fatal(err)
	}
	assertCollectionNames(t, ctx, backend, "orders")
	assertCollectionNames(t, ctx, backend, "customers")
	assertCollectionNames(t, ctx, backend, "admin", backends.ReservedReplicationControlName)
	assertControlMarker(t, ctx, backend)
	backend.Close()

	reopened, err := NewBackend(directory, slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertCollectionNames(t, ctx, reopened, "orders")
	assertCollectionNames(t, ctx, reopened, "customers")
	assertCollectionNames(t, ctx, reopened, "admin", backends.ReservedReplicationControlName)
	assertControlMarker(t, ctx, reopened)
}

func assertControlMarker(t *testing.T, ctx context.Context, backend backends.Backend) {
	t.Helper()
	admin, err := backend.Database("admin")
	if err != nil {
		t.Fatal(err)
	}
	collection, err := admin.Collection(backends.ReservedReplicationControlName)
	if err != nil {
		t.Fatal(err)
	}
	result, err := collection.Query(ctx, new(backends.QueryParams))
	if err != nil {
		t.Fatal(err)
	}
	defer result.Iter.Close()
	_, document, err := result.Iter.Next()
	if err != nil {
		t.Fatal(err)
	}
	marker, err := document.Get("marker")
	if err != nil {
		t.Fatal(err)
	}
	if marker != "preserved" {
		t.Fatalf("control marker = %v, want preserved", marker)
	}
}

func assertCollectionNames(t *testing.T, ctx context.Context, backend backends.Backend, databaseName string, want ...string) {
	t.Helper()
	database, err := backend.Database(databaseName)
	if err != nil {
		t.Fatal(err)
	}
	result, err := database.ListCollections(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Collections) != len(want) {
		t.Fatalf("%s collections = %+v, want %v", databaseName, result.Collections, want)
	}
	for index := range want {
		if result.Collections[index].Name != want[index] {
			t.Fatalf("%s collections = %+v, want %v", databaseName, result.Collections, want)
		}
	}
}
