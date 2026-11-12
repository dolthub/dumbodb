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

package oplog

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/replication/idlookup"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// queryHookBackend runs onQuery before every Query on collection
// queryCollection.
type queryHookBackend struct {
	backends.Backend
	queryCollection string
	onQuery         func(*backends.QueryParams)
}

func (b *queryHookBackend) Database(name string) (backends.Database, error) {
	database, err := b.Backend.Database(name)
	if err != nil {
		return nil, err
	}
	return &queryHookDatabase{Database: database, backend: b}, nil
}

type queryHookDatabase struct {
	backends.Database
	backend *queryHookBackend
}

func (d *queryHookDatabase) Collection(name string) (backends.Collection, error) {
	collection, err := d.Database.Collection(name)
	if err != nil || name != d.backend.queryCollection {
		return collection, err
	}
	return &queryHookCollection{Collection: collection, backend: d.backend}, nil
}

type queryHookCollection struct {
	backends.Collection
	backend *queryHookBackend
}

func (c *queryHookCollection) Query(ctx context.Context, params *backends.QueryParams) (*backends.QueryResult, error) {
	c.backend.onQuery(params)
	return c.Collection.Query(ctx, params)
}

func newQueryHookApplier(t *testing.T, onQuery func(*backends.QueryParams)) (backends.Backend, *Applier, string) {
	t.Helper()
	ctx := context.Background()
	backend, applier, sourceUUID := newTestApplier(t)
	createTestCollection(t, ctx, backend, applier, sourceUUID, "orders", "items")
	for i := int32(0); i < 3; i++ {
		insertTestDocuments(t, ctx, backend, "orders", "items", must.NotFail(types.NewDocument("_id", i, "v", "x")))
	}
	hooked, err := NewApplier(&queryHookBackend{Backend: backend, queryCollection: "items", onQuery: onQuery}, applier.store)
	if err != nil {
		t.Fatal(err)
	}
	return backend, hooked, sourceUUID
}

func TestApplierRejectsIDsMongodWouldNotStore(t *testing.T) {
	ctx := context.Background()
	backend, applier, sourceUUID := newQueryHookApplier(t, func(params *backends.QueryParams) {
		t.Errorf("rejected _id reached Query with filter %v", params.Filter)
	})
	ids := []any{
		must.NotFail(types.NewDocument("$in", must.NotFail(types.NewArray(must.NotFail(types.NewDocument("", int32(1))))))),
		must.NotFail(types.NewDocument("$gt", types.MinKey)),
		must.NotFail(types.NewDocument("$regex", "^x")),
		must.NotFail(types.NewDocument("$where", "true")),
		must.NotFail(types.NewDocument("a", must.NotFail(types.NewDocument("$ne", int32(1))))),
		must.NotFail(types.NewArray(int32(1))),
		types.Regex{Pattern: "^x"},
	}
	increment := uint32(10)
	for _, id := range ids {
		key := must.NotFail(types.NewDocument("_id", id))
		update := must.NotFail(types.NewDocument("$v", int32(2), "diff", must.NotFail(types.NewDocument("u", must.NotFail(types.NewDocument("v", "hit"))))))
		entries := []Entry{
			makeOplogEntry(t, increment, "i", "orders.items", sourceUUID, must.NotFail(types.NewDocument("_id", id, "v", "y")), nil),
			makeOplogEntry(t, increment+1, "u", "orders.items", sourceUUID, update, key),
			makeOplogEntry(t, increment+2, "d", "orders.items", sourceUUID, key, nil),
		}
		increment += 3
		for _, entry := range entries {
			err := applier.Apply(ctx, entry)
			if err == nil || !strings.Contains(err.Error(), "invalid replicated _id") {
				t.Errorf("%s with _id %v: err = %v, want invalid replicated _id", entry.Operation, id, err)
			}
		}
	}
	assertCollectionCount(t, ctx, backend, "orders", "items", 3)
}

func TestApplierLooksUpIDsAsLiteralValues(t *testing.T) {
	ctx := context.Background()
	var filters []*types.Document
	backend, applier, sourceUUID := newQueryHookApplier(t, func(params *backends.QueryParams) {
		filters = append(filters, params.Filter)
	})
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	ids := []any{
		types.Null,
		doc("$ref", "people", "$id", int32(1)),
		doc("$ref", "x", "$id", doc("$gt", int32(1))),
		doc("$ref", "x", "$id", doc("a", doc("$b", int32(1)))),
		doc("$ref", "x", "$id", doc("$gt", int32(2)), "$db", "d"),
		doc("$ref", "x", "$id", must.NotFail(types.NewArray(doc("$x", int32(1))))),
	}
	items := must.NotFail(must.NotFail(backend.Database("orders")).Collection("items"))
	increment := uint32(10)
	for _, id := range ids {
		key := doc("_id", id)
		if err := applier.Apply(ctx, makeOplogEntry(t, increment, "i", "orders.items", sourceUUID, doc("_id", id, "v", "a"), nil)); err != nil {
			t.Fatalf("insert _id %v: %v", id, err)
		}
		update := doc("$v", int32(2), "diff", doc("u", doc("v", "b")))
		if err := applier.Apply(ctx, makeOplogEntry(t, increment+1, "u", "orders.items", sourceUUID, update, key)); err != nil {
			t.Fatalf("update _id %v: %v", id, err)
		}
		stored, found, err := idlookup.FindDocument(ctx, items, id)
		if err != nil || !found || types.Compare(stored, doc("_id", id, "v", "b")) != types.Equal {
			t.Fatalf("stored document for _id %v = %v, found %v, err %v", id, stored, found, err)
		}
		if err := applier.Apply(ctx, makeOplogEntry(t, increment+2, "d", "orders.items", sourceUUID, key, nil)); err != nil {
			t.Fatalf("delete _id %v: %v", id, err)
		}
		assertCollectionCount(t, ctx, backend, "orders", "items", 3)
		increment += 3
	}
	if len(filters) == 0 {
		t.Fatal("no _id lookup reached Query")
	}
	for _, filter := range filters {
		condition, ok := must.NotFail(filter.Get("_id")).(*types.Document)
		if !ok || condition.Len() != 1 || condition.Keys()[0] != "$eq" {
			t.Fatalf("lookup filter = %v, want {_id: {$eq: <id>}}", filter)
		}
	}
}

func TestApplierReturnsPanicAsError(t *testing.T) {
	ctx := context.Background()
	_, applier, sourceUUID := newQueryHookApplier(t, func(*backends.QueryParams) {
		panic("injected query panic")
	})
	key := must.NotFail(types.NewDocument("_id", int32(1)))
	entry := makeOplogEntry(t, 10, "d", "orders.items", sourceUUID, key, nil)
	err := applier.Apply(ctx, entry)
	var panicErr *ApplyPanicError
	if !errors.As(err, &panicErr) {
		t.Fatalf("Apply error = %v, want *ApplyPanicError", err)
	}
	if panicErr.OpTime != entry.OpTime || panicErr.Namespace != "orders.items" || panicErr.Operation != "d" ||
		panicErr.Value != "injected query panic" || len(panicErr.Stack) == 0 {
		t.Fatalf("panic error = %+v", panicErr)
	}
}
