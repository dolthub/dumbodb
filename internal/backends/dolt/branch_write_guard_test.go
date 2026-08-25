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
	"fmt"
	"strings"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

func branchWriteInsertParams(doc *types.Document) *backends.InsertAllParams {
	return &backends.InsertAllParams{Docs: []*types.Document{doc}}
}

func TestNonexistentBranchMutationPathsAreRejected(t *testing.T) {
	ctx := context.Background()
	backend := newTestBackend(t)
	insertDoc(t, backend, "testdb", "nodes", mustDoc(t, "_id", "base", "value", int32(1)))
	commitDB(t, backend, "testdb", "base")

	tests := []struct {
		name   string
		mutate func(database backends.Database, collection backends.Collection) error
	}{
		{
			name: "update",
			mutate: func(_ backends.Database, collection backends.Collection) error {
				_, err := collection.UpdateAll(ctx, &backends.UpdateAllParams{
					Docs: []*types.Document{mustDoc(t, "_id", "base", "value", int32(2))},
				})
				return err
			},
		},
		{
			name: "delete",
			mutate: func(_ backends.Database, collection backends.Collection) error {
				_, err := collection.DeleteAll(ctx, &backends.DeleteAllParams{IDs: []any{"base"}})
				return err
			},
		},
		{
			name: "create_collection",
			mutate: func(database backends.Database, _ backends.Collection) error {
				return database.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "created"})
			},
		},
		{
			name: "create_index",
			mutate: func(_ backends.Database, collection backends.Collection) error {
				_, err := collection.CreateIndexes(ctx, &backends.CreateIndexesParams{
					Indexes: []backends.IndexInfo{{
						Name: "by_value",
						Key:  []backends.IndexKeyPair{{Field: "value"}},
					}},
				})
				return err
			},
		},
	}

	state, err := backend.getOrOpenDB(ctx, "testdb", false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			branch := fmt.Sprintf("neverMade%d", index)
			database, err := backend.Database("testdb@" + branch)
			if err != nil {
				t.Fatalf("Database: %v", err)
			}
			collection, err := database.Collection("nodes")
			if err != nil {
				t.Fatalf("Collection: %v", err)
			}

			err = test.mutate(database, collection)
			if err == nil {
				t.Fatal("mutation on nonexistent branch succeeded")
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("rootish %q: not found", branch)) {
				t.Fatalf("error = %q, want missing rootish", err)
			}

			branchDataset, err := state.datasDB.GetDataset(ctx, "refs/heads/"+branch)
			if err != nil {
				t.Fatalf("GetDataset(branch): %v", err)
			}
			if branchDataset.HasHead() {
				t.Fatal("mutation created branch ref")
			}
			workingSetDataset, err := state.datasDB.GetDataset(ctx, workingSetForBranch(branch))
			if err != nil {
				t.Fatalf("GetDataset(working set): %v", err)
			}
			if workingSetDataset.HasHead() {
				t.Fatal("mutation created working-set ref")
			}
		})
	}
}

func TestInsertOnNonexistentBranchDoesNotCreateWorkingSet(t *testing.T) {
	ctx := context.Background()
	backend := newTestBackend(t)
	insertDoc(t, backend, "testdb", "nodes", mustDoc(t, "_id", "base"))
	commitDB(t, backend, "testdb", "base")

	state, err := backend.getOrOpenDB(ctx, "testdb", false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	branchDataset, err := state.datasDB.GetDataset(ctx, "refs/heads/neverMade")
	if err != nil {
		t.Fatalf("GetDataset(branch before): %v", err)
	}
	if branchDataset.HasHead() {
		t.Fatal("branch exists before insert")
	}

	database, err := backend.Database("testdb@neverMade")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	collection, err := database.Collection("nodes")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	_, err = collection.InsertAll(ctx, branchWriteInsertParams(mustDoc(t, "_id", "ghost")))
	if err == nil {
		t.Fatal("InsertAll on nonexistent branch succeeded")
	}
	if !strings.Contains(err.Error(), "rootish \"neverMade\": not found") {
		t.Fatalf("error = %q, want missing rootish", err)
	}

	branchDataset, err = state.datasDB.GetDataset(ctx, "refs/heads/neverMade")
	if err != nil {
		t.Fatalf("GetDataset(branch after): %v", err)
	}
	if branchDataset.HasHead() {
		t.Fatal("insert created branch ref")
	}
	workingSetDataset, err := state.datasDB.GetDataset(ctx, workingSetForBranch("neverMade"))
	if err != nil {
		t.Fatalf("GetDataset(working set): %v", err)
	}
	if workingSetDataset.HasHead() {
		t.Fatal("insert created working-set ref")
	}
}

func TestFreshMainAcceptsFirstWrite(t *testing.T) {
	ctx := context.Background()
	backend := newTestBackend(t)

	database, err := backend.Database("freshdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	collection, err := database.Collection("nodes")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	if _, err = collection.InsertAll(ctx, branchWriteInsertParams(mustDoc(t, "_id", "first"))); err != nil {
		t.Fatalf("InsertAll: %v", err)
	}

	state, err := backend.getOrOpenDB(ctx, "freshdb", false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	if state == nil {
		t.Fatal("freshdb was not created")
	}
	if !mainDS(t, state).HasHead() {
		t.Fatal("main has no HEAD after first write")
	}
	if got := countDocs(t, backend, "freshdb", "nodes"); got != 1 {
		t.Fatalf("document count = %d, want 1", got)
	}
}
