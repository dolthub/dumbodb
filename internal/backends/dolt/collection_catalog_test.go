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
	"os"
	"strings"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

func collInfo(t *testing.T, db backends.Database, name string) backends.CollectionInfo {
	t.Helper()
	res, err := db.ListCollections(context.Background(), &backends.ListCollectionsParams{Name: name})
	if err != nil {
		t.Fatalf("ListCollections(%q): %v", name, err)
	}
	if len(res.Collections) != 1 {
		t.Fatalf("ListCollections(%q): got %d, want 1", name, len(res.Collections))
	}
	return res.Collections[0]
}

// TestCollMetaDurableAcrossRestart verifies a collection's UUID and validator
// survive a backend close and reopen -- they now live in the durable
// __dumbo_catalog__ collection, not an in-memory map (workspace-alp.16).
func TestCollMetaDurableAcrossRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-collmeta-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	logger := slog.Default()

	validator := catMustDoc(types.NewDocument("x", catMustDoc(types.NewDocument("$exists", true))))

	// --- Phase 1: create a collection with a validator ---
	b1, err := NewBackend(dir, logger, false, false, 0, 0)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	db1, err := b1.Database("metadb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	if err := db1.CreateCollection(ctx, &backends.CreateCollectionParams{
		Name: "orders", Validator: validator, ValidationLevel: "strict", ValidationAction: "error",
	}); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	before := collInfo(t, db1, "orders")
	if before.UUID == "" {
		t.Fatal("collection has no UUID before restart")
	}
	b1.Close()

	// --- Phase 2: reopen and confirm UUID + validator survived ---
	b2, err := NewBackend(dir, logger, false, false, 0, 0)
	if err != nil {
		t.Fatalf("NewBackend (reopen): %v", err)
	}
	defer b2.Close()
	db2, err := b2.Database("metadb")
	if err != nil {
		t.Fatalf("Database (reopen): %v", err)
	}

	after := collInfo(t, db2, "orders")
	if after.UUID != before.UUID {
		t.Errorf("UUID not stable across restart: before=%q after=%q", before.UUID, after.UUID)
	}
	if after.Validator == nil {
		t.Error("validator lost across restart")
	}
	if after.ValidationLevel != "strict" || after.ValidationAction != "error" {
		t.Errorf("validation options lost: level=%q action=%q", after.ValidationLevel, after.ValidationAction)
	}
}

// TestCatalogHiddenFromListCollections verifies the internal catalog collection
// is never surfaced by ListCollections.
func TestCatalogHiddenFromListCollections(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-catalog-hidden-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	b, err := NewBackend(dir, slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer b.Close()
	db, err := b.Database("metadb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	if err := db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "orders"}); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	res, err := db.ListCollections(ctx, &backends.ListCollectionsParams{})
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	for _, c := range res.Collections {
		if c.Name == reservedCatalogName {
			t.Fatalf("internal catalog %q leaked into ListCollections", reservedCatalogName)
		}
	}
}

func catMustDoc[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestCatalogMergeCarriesMetadata verifies that a collection created (with a
// validator) on a branch brings its metadata along when merged into a branch
// that never had it (workspace-alp.16, merge correctness).
func TestCatalogMergeCarriesMetadata(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()
	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, "mmdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	insertDocForTest(t, ctx, b, "mmdb", 1) // base collection "col"
	commitBranch(t, b, "mmdb", "main", "seed")
	branchFrom(t, b, "mmdb", "main", "feature")

	featDB, err := b.Database("mmdb@feature")
	if err != nil {
		t.Fatalf("Database(feature): %v", err)
	}
	validator := catMustDoc(types.NewDocument("k", catMustDoc(types.NewDocument("$exists", true))))
	if err := featDB.CreateCollection(ctx, &backends.CreateCollectionParams{
		Name: "orders", Validator: validator, ValidationLevel: "strict",
	}); err != nil {
		t.Fatalf("CreateCollection(feature): %v", err)
	}
	commitBranch(t, b, "mmdb", "feature", "add orders + validator")

	if _, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: "mmdb", Into: "main", From: "feature", Message: "merge", Author: "t <t@e>",
	}); err != nil {
		t.Fatalf("DumboDBMerge: %v", err)
	}

	mainDB, err := b.Database("mmdb")
	if err != nil {
		t.Fatalf("Database(main): %v", err)
	}
	ci := collInfo(t, mainDB, "orders")
	if ci.UUID == "" || ci.Validator == nil || ci.ValidationLevel != "strict" {
		t.Fatalf("merged collection lost its metadata: %+v", ci)
	}
}

// TestCatalogMergeConflictRefused verifies that a metadata change diverging on
// both branches is NOT silently dropped: the merge is refused loudly, naming the
// owning collection while never exposing the internal __dumbo_catalog__ name.
// (Interim behavior; workspace-xhm makes it a resolvable conflict instead.)
func TestCatalogMergeConflictRefused(t *testing.T) {
	b, dir := newBackendForTest(t)
	defer os.RemoveAll(dir)
	defer b.Close()
	ctx := context.Background()
	if _, err := b.getOrOpenDB(ctx, "mcdb", true); err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	insertDocForTest(t, ctx, b, "mcdb", 1)

	mainDB, err := b.Database("mcdb")
	if err != nil {
		t.Fatalf("Database(main): %v", err)
	}
	if err := mainDB.CreateCollection(ctx, &backends.CreateCollectionParams{Name: "orders", ValidationLevel: "off"}); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	commitBranch(t, b, "mcdb", "main", "seed + orders")
	branchFrom(t, b, "mcdb", "main", "feature")

	featDB, err := b.Database("mcdb@feature")
	if err != nil {
		t.Fatalf("Database(feature): %v", err)
	}
	if err := featDB.CollMod(ctx, &backends.CollModParams{Name: "orders", ValidationLevel: "moderate"}); err != nil {
		t.Fatalf("CollMod(feature): %v", err)
	}
	commitBranch(t, b, "mcdb", "feature", "orders -> moderate")

	if err := mainDB.CollMod(ctx, &backends.CollModParams{Name: "orders", ValidationLevel: "strict"}); err != nil {
		t.Fatalf("CollMod(main): %v", err)
	}
	commitBranch(t, b, "mcdb", "main", "orders -> strict")

	// A divergent metadata change on both branches must NOT be silently dropped.
	// Until the metadata conflict-resolution workflow lands (workspace-xhm), the
	// merge is refused loudly, naming the owning collection and never exposing
	// the internal catalog.
	_, err = b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: "mcdb", Into: "main", From: "feature", Message: "merge", Author: "t <t@e>",
	})
	if err == nil {
		t.Fatal("divergent metadata merge must be refused, not silently completed")
	}
	msg := err.Error()
	if !strings.Contains(msg, "orders") {
		t.Fatalf("refusal must name the owning collection 'orders': %v", msg)
	}
	if strings.Contains(msg, reservedCatalogName) {
		t.Fatalf("refusal must NOT expose the internal catalog name: %v", msg)
	}

	// Nothing was dropped: both sides keep their own value (the merge did not
	// apply). main stays strict; feature stays moderate.
	if ci := collInfo(t, mainDB, "orders"); ci.ValidationLevel != "strict" {
		t.Fatalf("main's validationLevel must be intact after a refused merge, got %q", ci.ValidationLevel)
	}
	if ci := collInfo(t, featDB, "orders"); ci.ValidationLevel != "moderate" {
		t.Fatalf("feature's validationLevel must be intact after a refused merge, got %q", ci.ValidationLevel)
	}
}

// TestCatalogNameRejected verifies the internal catalog collection cannot be
// created or accessed by name through the public API (workspace-alp.16 (1)),
// while DumboDB's own low-level catalog writes (exercised by the durability
// tests) still succeed.
func TestCatalogNameRejected(t *testing.T) {
	dir, err := os.MkdirTemp("", "dolt-catalog-reject-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	b, err := NewBackend(dir, slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer b.Close()
	db, err := b.Database("metadb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	if err := db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: reservedCatalogName}); err == nil {
		t.Fatal("creating a collection named as the catalog must be rejected")
	} else if !backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
		t.Errorf("create catalog name: want CollectionNameIsInvalid, got %v", err)
	}

	if _, err := db.Collection(reservedCatalogName); err == nil {
		t.Fatal("accessing the catalog collection by name must be rejected")
	} else if !backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
		t.Errorf("access catalog name: want CollectionNameIsInvalid, got %v", err)
	}
}
