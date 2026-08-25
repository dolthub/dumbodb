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
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

func branchWriteInsertParams(doc *types.Document) *backends.InsertAllParams {
	return &backends.InsertAllParams{Docs: []*types.Document{doc}}
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
