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
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"

	// Blank import: registers FilterDocument as the partial filter matcher
	// at init, the same way the production registry/dolt path wires it.
	// Without this the reloaded MatchesPartialFilter closure has no predicate
	// to call.
	_ "github.com/dolthub/dumbodb/internal/handler/common"
)

// TestPartialIndexUniquenessSurvivesRestart verifies the do-10do fix: a partial
// unique index reloaded from the chunk store still rejects duplicates *only*
// for documents matching the filter.
//
// Before the fix, MatchesPartialFilter was a Go closure that vanished on
// restart — partial unique indexes silently became full unique indexes (or
// vice versa, depending on call site) once reloaded.
func TestPartialIndexUniquenessSurvivesRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-idx-partial-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	logger := slog.Default()

	// partialFilterExpression: { status: "active" }
	pfe := must.NotFail(types.NewDocument("status", "active"))

	// --- Phase 1: create partial unique index, insert non-conflicting docs ---
	b1, err := NewBackend(dir, logger, false)
	if err != nil {
		t.Fatalf("NewBackend (open): %v", err)
	}
	db1, err := b1.Database("idxdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	coll1, err := db1.Collection("users")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}

	// Seed doc so the collection physically exists before CreateIndexes.
	seed := must.NotFail(types.NewDocument("_id", int32(0), "email", "seed@x.com", "status", "inactive"))
	if _, err := coll1.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{seed}}); err != nil {
		t.Fatalf("InsertAll seed: %v", err)
	}

	idx := backends.IndexInfo{
		Name:                    "email_partial",
		Key:                     []backends.IndexKeyPair{{Field: "email"}},
		Unique:                  true,
		PartialFilterExpression: pfe,
		MatchesPartialFilter: func(doc *types.Document) (bool, error) {
			return backends.MatchPartialFilter(doc, pfe)
		},
	}
	if _, err := coll1.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{idx},
	}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}

	// {_id:1, email:"a@x.com", status:"active"} — indexed (matches filter).
	// {_id:2, email:"a@x.com", status:"inactive"} — not indexed; same email is fine.
	docs := []*types.Document{
		must.NotFail(types.NewDocument("_id", int32(1), "email", "a@x.com", "status", "active")),
		must.NotFail(types.NewDocument("_id", int32(2), "email", "a@x.com", "status", "inactive")),
	}
	if _, err := coll1.InsertAll(ctx, &backends.InsertAllParams{Docs: docs}); err != nil {
		t.Fatalf("InsertAll phase1: %v", err)
	}

	b1.Close()

	// --- Phase 2: reopen and verify uniqueness is enforced exactly for matching docs ---
	b2, err := NewBackend(dir, logger, false)
	if err != nil {
		t.Fatalf("NewBackend (reopen): %v", err)
	}
	defer b2.Close()
	db2, err := b2.Database("idxdb")
	if err != nil {
		t.Fatalf("Database (reopen): %v", err)
	}
	coll2, err := db2.Collection("users")
	if err != nil {
		t.Fatalf("Collection (reopen): %v", err)
	}

	// Sanity: the reloaded index has both PartialFilterExpression and a
	// non-nil MatchesPartialFilter. A nil closure was the original bug.
	listRes, err := coll2.ListIndexes(ctx, &backends.ListIndexesParams{})
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
	var got *backends.IndexInfo
	for i := range listRes.Indexes {
		if listRes.Indexes[i].Name == "email_partial" {
			got = &listRes.Indexes[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("partial index not present after reopen")
	}
	if got.PartialFilterExpression == nil {
		t.Fatalf("PartialFilterExpression is nil after reopen")
	}
	if got.MatchesPartialFilter == nil {
		t.Fatalf("MatchesPartialFilter is nil after reopen — the do-10do bug")
	}

	// Inserting a third active doc with the same email must fail: the
	// existing active doc (_id:1) still occupies the partial index slot.
	conflict := must.NotFail(types.NewDocument("_id", int32(3), "email", "a@x.com", "status", "active"))
	_, err = coll2.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{conflict}})
	if err == nil {
		t.Fatalf("expected duplicate-key error for matching doc, got nil")
	}
	var berr *backends.Error
	if !errors.As(err, &berr) || berr.Code() != backends.ErrorCodeInsertDuplicateID {
		t.Fatalf("expected ErrorCodeInsertDuplicateID, got %v", err)
	}

	// Inserting an inactive doc with the same email must succeed: filter
	// excludes it from the partial index, so no uniqueness conflict.
	nonMatching := must.NotFail(types.NewDocument("_id", int32(4), "email", "a@x.com", "status", "inactive"))
	if _, err := coll2.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{nonMatching}}); err != nil {
		t.Fatalf("InsertAll non-matching: unexpected error %v", err)
	}
}
