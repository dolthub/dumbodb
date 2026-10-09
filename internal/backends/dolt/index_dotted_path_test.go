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
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func newDottedPathTestCollection(t *testing.T, idx backends.IndexInfo) backends.Collection {
	t.Helper()
	ctx := context.Background()
	b, err := NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	db, err := b.Database("idxdb")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	c, err := db.Collection("tests")
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	seed := must.NotFail(types.NewDocument("_id", int32(0)))
	if _, err := c.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{seed}}); err != nil {
		t.Fatalf("InsertAll seed: %v", err)
	}
	if _, err := c.CreateIndexes(ctx, &backends.CreateIndexesParams{Indexes: []backends.IndexInfo{idx}}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}
	return c
}

func insertDottedPathDoc(c backends.Collection, doc *types.Document) error {
	_, err := c.InsertAll(context.Background(), &backends.InsertAllParams{Docs: []*types.Document{doc}})
	return err
}

// dolthub/dumbodb#112
func TestUniqueIndexCompoundNestedObjectID(t *testing.T) {
	c := newDottedPathTestCollection(t, backends.IndexInfo{
		Name:   "domain_method",
		Unique: true,
		Key:    []backends.IndexKeyPair{{Field: "discoveryDomain.ID"}, {Field: "methodKey"}},
	})
	oid := func(b byte) types.ObjectID {
		var o types.ObjectID
		o[11] = b
		return o
	}
	doc := func(id int32, domain types.ObjectID) *types.Document {
		return must.NotFail(types.NewDocument(
			"_id", id,
			"discoveryDomain", must.NotFail(types.NewDocument("ID", domain)),
			"methodKey", "same-method",
		))
	}

	if err := insertDottedPathDoc(c, doc(1, oid(1))); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insertDottedPathDoc(c, doc(2, oid(2))); err != nil {
		t.Fatalf("distinct nested value must not collide: %v", err)
	}

	err := insertDottedPathDoc(c, doc(3, oid(1)))
	var be *backends.Error
	if !errors.As(err, &be) || be.Code() != backends.ErrorCodeInsertDuplicateID {
		t.Fatalf("repeated nested value must collide, got %v", err)
	}
	gotID, _ := be.DupKey().Get("discoveryDomain.ID")
	if gotID != oid(1) {
		t.Fatalf("dup key discoveryDomain.ID = %v, want %v", gotID, oid(1))
	}
}

func TestUniqueIndexDottedPathThroughArray(t *testing.T) {
	c := newDottedPathTestCollection(t, backends.IndexInfo{
		Name:   "items_sku",
		Unique: true,
		Key:    []backends.IndexKeyPair{{Field: "items.sku"}},
	})
	doc := func(id int32, skus ...string) *types.Document {
		items := types.MakeArray(len(skus))
		for _, s := range skus {
			items.Append(must.NotFail(types.NewDocument("sku", s)))
		}
		return must.NotFail(types.NewDocument("_id", id, "items", items))
	}

	if err := insertDottedPathDoc(c, doc(1, "a", "b")); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insertDottedPathDoc(c, doc(2, "c", "d")); err != nil {
		t.Fatalf("disjoint skus must not collide: %v", err)
	}
	if err := insertDottedPathDoc(c, doc(3, "x", "b")); !backends.ErrorCodeIs(err, backends.ErrorCodeInsertDuplicateID) {
		t.Fatalf("shared sku must collide, got %v", err)
	}
}

func TestIndexFieldValue(t *testing.T) {
	doc := must.NotFail(types.NewDocument(
		"a", must.NotFail(types.NewDocument("b", int32(1))),
		"arr", must.NotFail(types.NewArray(
			must.NotFail(types.NewDocument("v", must.NotFail(types.NewArray(int32(1), int32(2))))),
			must.NotFail(types.NewDocument("v", int32(3))),
		)),
	))
	if got := indexFieldValue(doc, "a.b"); got != int32(1) {
		t.Errorf("a.b = %v, want 1", got)
	}
	if got := indexFieldValue(doc, "a.missing"); got != types.Null {
		t.Errorf("a.missing = %v, want null", got)
	}
	got, ok := indexFieldValue(doc, "arr.v").(*types.Array)
	if !ok || got.Len() != 3 {
		t.Fatalf("arr.v = %v, want flattened [1 2 3]", got)
	}
}

func nestedDoc(id int32, inner *types.Document) *types.Document {
	return must.NotFail(types.NewDocument("_id", id, "a", inner))
}

func nestedB(v any) *types.Document {
	return must.NotFail(types.NewDocument("b", v))
}

// The update path must both remove the old nested key and probe the new one.
func TestUniqueIndexDottedPathUpdate(t *testing.T) {
	ctx := context.Background()
	c := newDottedPathTestCollection(t, backends.IndexInfo{
		Name:   "a_b",
		Unique: true,
		Key:    []backends.IndexKeyPair{{Field: "a.b"}},
	})
	update := func(doc *types.Document) error {
		_, err := c.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{doc}})
		return err
	}

	if err := insertDottedPathDoc(c, nestedDoc(1, nestedB(int32(1)))); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if err := insertDottedPathDoc(c, nestedDoc(2, nestedB(int32(2)))); err != nil {
		t.Fatalf("insert 2: %v", err)
	}
	if err := update(nestedDoc(2, nestedB(int32(1)))); !backends.ErrorCodeIs(err, backends.ErrorCodeInsertDuplicateID) {
		t.Fatalf("update onto taken nested value must collide, got %v", err)
	}
	if err := update(nestedDoc(1, nestedB(int32(5)))); err != nil {
		t.Fatalf("update 1 away: %v", err)
	}
	if err := update(nestedDoc(2, nestedB(int32(1)))); err != nil {
		t.Fatalf("nested value freed by update must be reusable: %v", err)
	}
}

// The delete path must remove the entry keyed on the nested value.
func TestUniqueIndexDottedPathDeleteFreesKey(t *testing.T) {
	ctx := context.Background()
	c := newDottedPathTestCollection(t, backends.IndexInfo{
		Name:   "a_b",
		Unique: true,
		Key:    []backends.IndexKeyPair{{Field: "a.b"}},
	})
	if err := insertDottedPathDoc(c, nestedDoc(1, nestedB("x"))); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if _, err := c.DeleteAll(ctx, &backends.DeleteAllParams{IDs: []any{int32(1)}}); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if err := insertDottedPathDoc(c, nestedDoc(2, nestedB("x"))); err != nil {
		t.Fatalf("nested value freed by delete must be reusable: %v", err)
	}
}

// A sparse unique index skips docs missing the nested path but still enforces
// uniqueness on docs that have it.
func TestUniqueSparseIndexDottedPath(t *testing.T) {
	c := newDottedPathTestCollection(t, backends.IndexInfo{
		Name:   "a_b",
		Unique: true,
		Sparse: true,
		Key:    []backends.IndexKeyPair{{Field: "a.b"}},
	})
	other := must.NotFail(types.NewDocument("c", int32(1)))
	if err := insertDottedPathDoc(c, nestedDoc(1, other)); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if err := insertDottedPathDoc(c, nestedDoc(2, other)); err != nil {
		t.Fatalf("second doc missing a.b must not collide under sparse: %v", err)
	}
	if err := insertDottedPathDoc(c, nestedDoc(3, nestedB(int32(7)))); err != nil {
		t.Fatalf("insert 3: %v", err)
	}
	if err := insertDottedPathDoc(c, nestedDoc(4, nestedB(int32(7)))); !backends.ErrorCodeIs(err, backends.ErrorCodeInsertDuplicateID) {
		t.Fatalf("repeated present nested value must collide, got %v", err)
	}
}
