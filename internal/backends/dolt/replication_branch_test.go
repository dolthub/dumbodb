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

func TestReplicationBranchStartsAndResetsAtInitialCommit(t *testing.T) {
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
	if err := database.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "main_only"}); err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection("main_only")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collection.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "source", "dumbodb")),
	}}); err != nil {
		t.Fatal(err)
	}
	versioning := backend.(backends.VersioningBackend)
	if _, err := versioning.DumboDBCommit(ctx, &backends.CommitParams{
		DBName: "orders", Branch: "main", Message: "main data", Author: "test",
	}); err != nil {
		t.Fatal(err)
	}
	replication := backend.(backends.ReplicationBranchBackend)
	if err := replication.EnsureReplicationBranch(ctx, "orders", "mongo-history"); err != nil {
		t.Fatal(err)
	}
	assertCollectionNames(t, ctx, backend, "orders@mongo-history")

	replicationDatabase, err := backend.Database("orders@mongo-history")
	if err != nil {
		t.Fatal(err)
	}
	if err := replicationDatabase.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "partial_clone"}); err != nil {
		t.Fatal(err)
	}
	if err := replication.ResetReplicationBranch(ctx, "orders", "mongo-history"); err != nil {
		t.Fatal(err)
	}
	assertCollectionNames(t, ctx, backend, "orders@mongo-history")
	assertCollectionNames(t, ctx, backend, "orders", "main_only")
	backend.Close()

	reopened, err := NewBackend(directory, slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertCollectionNames(t, ctx, reopened, "orders@mongo-history")
	assertCollectionNames(t, ctx, reopened, "orders", "main_only")
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
