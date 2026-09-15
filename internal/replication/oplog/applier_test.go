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
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/replication/catalog"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestApplierCRUDBySourceUUID(t *testing.T) {
	ctx := context.Background()
	backend, applier, sourceUUID := newTestApplier(t)
	catalogApplier, err := catalog.NewApplier(backend, applier.store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalogApplier.Create(ctx, "orders", backends.CreateCollectionParams{Name: "items"}, sourceUUID, applierOpTime(1)); err != nil {
		t.Fatal(err)
	}

	inserted := must.NotFail(types.NewDocument("_id", int32(7), "nested", must.NotFail(types.NewDocument("a", int32(1))), "obsolete", true))
	if err := applier.Apply(ctx, makeOplogEntry(t, 2, "i", "stale.name", sourceUUID, inserted, nil)); err != nil {
		t.Fatal(err)
	}
	if err := applier.Apply(ctx, makeOplogEntry(t, 2, "i", "stale.name", sourceUUID, inserted, nil)); err != nil {
		t.Fatalf("idempotent insert: %v", err)
	}

	diff := must.NotFail(types.NewDocument(
		"d", must.NotFail(types.NewDocument("obsolete", false)),
		"snested", must.NotFail(types.NewDocument("u", must.NotFail(types.NewDocument("a", int32(2))))),
	))
	update := must.NotFail(types.NewDocument("$v", int32(2), "diff", diff))
	key := must.NotFail(types.NewDocument("_id", int32(7)))
	if err := applier.Apply(ctx, makeOplogEntry(t, 3, "u", "orders.items", sourceUUID, update, key)); err != nil {
		t.Fatal(err)
	}
	want := must.NotFail(types.NewDocument("_id", int32(7), "nested", must.NotFail(types.NewDocument("a", int32(2)))))
	assertStoredDocument(t, ctx, backend, "orders", "items", want)

	replacement := must.NotFail(types.NewDocument("_id", int32(7), "replacement", true))
	retryable := makeOplogDocument(4, "u", "orders.items", sourceUUID, replacement, key)
	retryable.Set("lsid", must.NotFail(types.NewDocument("id", uuidBinary(uuid.MustParse("12345678-1234-4234-9234-123456789abc")))))
	retryable.Set("txnNumber", int64(9))
	entry, err := ParseEntry(retryable)
	if err != nil {
		t.Fatal(err)
	}
	if err := applier.Apply(ctx, entry); err != nil {
		t.Fatal(err)
	}
	assertStoredDocument(t, ctx, backend, "orders", "items", replacement)

	if err := applier.Apply(ctx, makeOplogEntry(t, 5, "d", "orders.items", sourceUUID, key, nil)); err != nil {
		t.Fatal(err)
	}
	if err := applier.Apply(ctx, makeOplogEntry(t, 5, "d", "orders.items", sourceUUID, key, nil)); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	assertCollectionCount(t, ctx, backend, "orders", "items", 0)
}

func TestApplierCatalogCommands(t *testing.T) {
	ctx := context.Background()
	_, applier, sourceUUID := newTestApplier(t)
	idIndex := must.NotFail(types.NewDocument("v", int32(2), "key", must.NotFail(types.NewDocument("_id", int32(1))), "name", "_id_"))
	create := must.NotFail(types.NewDocument(
		"create", "items",
		"validator", must.NotFail(types.NewDocument("active", must.NotFail(types.NewDocument("$type", "bool")))),
		"idIndex", idIndex,
	))
	if err := applier.Apply(ctx, makeOplogEntry(t, 1, "c", "orders.$cmd", sourceUUID, create, nil)); err != nil {
		t.Fatal(err)
	}
	createIndex := must.NotFail(types.NewDocument(
		"createIndexes", "items", "v", int32(2), "key", must.NotFail(types.NewDocument("account", int32(1))), "name", "account_1", "unique", true,
	))
	if err := applier.Apply(ctx, makeOplogEntry(t, 2, "c", "orders.$cmd", sourceUUID, createIndex, nil)); err != nil {
		t.Fatal(err)
	}
	rename := must.NotFail(types.NewDocument("renameCollection", "orders.items", "to", "orders.renamed", "stayTemp", false))
	if err := applier.Apply(ctx, makeOplogEntry(t, 3, "c", "orders.$cmd", sourceUUID, rename, nil)); err != nil {
		t.Fatal(err)
	}
	dropIndex := must.NotFail(types.NewDocument("dropIndexes", "renamed", "index", "account_1"))
	if err := applier.Apply(ctx, makeOplogEntry(t, 4, "c", "orders.$cmd", sourceUUID, dropIndex, nil)); err != nil {
		t.Fatal(err)
	}
	drop := must.NotFail(types.NewDocument("drop", "renamed"))
	if err := applier.Apply(ctx, makeOplogEntry(t, 5, "c", "orders.$cmd", sourceUUID, drop, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := applier.catalog.Resolve(ctx, sourceUUID); !errors.Is(err, catalog.ErrSourceUUIDNotFound) {
		t.Fatalf("resolved dropped collection: %v", err)
	}
}

func TestApplierAssemblesTransactionsWithAtomicVisibility(t *testing.T) {
	ctx := context.Background()
	backend, applier, sourceUUID := newTestApplier(t)
	eventsUUID := "87654321-4321-4321-8321-cba987654321"
	createTestCollection(t, ctx, backend, applier, sourceUUID, "orders", "items")
	createTestCollection(t, ctx, backend, applier, eventsUUID, "orders", "events")
	insertTestDocuments(t, ctx, backend, "orders", "items",
		must.NotFail(types.NewDocument("_id", int32(1), "value", int32(0))),
	)

	partialOperation := embeddedOperation("u", "orders.items", sourceUUID,
		must.NotFail(types.NewDocument("_id", int32(1), "value", int32(1))),
		must.NotFail(types.NewDocument("_id", int32(1))),
	)
	partial := makeTransactionEntry(t, 10, 22, nullOpTimeDocument(), true, false, partialOperation)
	if err := applier.Apply(ctx, partial); err != nil {
		t.Fatal(err)
	}
	assertStoredDocument(t, ctx, backend, "orders", "items", must.NotFail(types.NewDocument("_id", int32(1), "value", int32(0))))
	assertCollectionCount(t, ctx, backend, "orders", "events", 0)

	terminalOperation := embeddedOperation("i", "orders.events", eventsUUID,
		must.NotFail(types.NewDocument("_id", int32(2), "event", "committed")),
		nil,
	)
	terminal := makeTransactionEntry(t, 11, 22, opTimeDocument(partial.OpTime), false, false, terminalOperation)
	if err := applier.Apply(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	assertStoredDocument(t, ctx, backend, "orders", "items", must.NotFail(types.NewDocument("_id", int32(1), "value", int32(1))))
	assertStoredDocument(t, ctx, backend, "orders", "events", must.NotFail(types.NewDocument("_id", int32(2), "event", "committed")))
	if len(applier.store.Snapshot().TransactionParts) != 0 {
		t.Fatal("committed transaction fragments were retained")
	}
}

func TestApplierPreparedCommitAndAbort(t *testing.T) {
	ctx := context.Background()
	backend, applier, sourceUUID := newTestApplier(t)
	createTestCollection(t, ctx, backend, applier, sourceUUID, "orders", "items")
	insertTestDocuments(t, ctx, backend, "orders", "items", must.NotFail(types.NewDocument("_id", int32(1), "value", int32(0))))

	preparedOperation := embeddedOperation("u", "orders.items", sourceUUID,
		must.NotFail(types.NewDocument("_id", int32(1), "value", int32(9))),
		must.NotFail(types.NewDocument("_id", int32(1))),
	)
	prepare := makeTransactionEntry(t, 20, 30, nullOpTimeDocument(), false, true, preparedOperation)
	if err := applier.Apply(ctx, prepare); err != nil {
		t.Fatal(err)
	}
	assertStoredDocument(t, ctx, backend, "orders", "items", must.NotFail(types.NewDocument("_id", int32(1), "value", int32(0))))
	commit := makeTransactionCommandEntry(t, 21, 30, "commitTransaction", opTimeDocument(prepare.OpTime))
	if err := applier.Apply(ctx, commit); err != nil {
		t.Fatal(err)
	}
	assertStoredDocument(t, ctx, backend, "orders", "items", must.NotFail(types.NewDocument("_id", int32(1), "value", int32(9))))

	abortedOperation := embeddedOperation("u", "orders.items", sourceUUID,
		must.NotFail(types.NewDocument("_id", int32(1), "value", int32(99))),
		must.NotFail(types.NewDocument("_id", int32(1))),
	)
	abortedPrepare := makeTransactionEntry(t, 22, 31, nullOpTimeDocument(), false, true, abortedOperation)
	if err := applier.Apply(ctx, abortedPrepare); err != nil {
		t.Fatal(err)
	}
	abort := makeTransactionCommandEntry(t, 23, 31, "abortTransaction", opTimeDocument(abortedPrepare.OpTime))
	if err := applier.Apply(ctx, abort); err != nil {
		t.Fatal(err)
	}
	assertStoredDocument(t, ctx, backend, "orders", "items", must.NotFail(types.NewDocument("_id", int32(1), "value", int32(9))))
}

func TestApplierRollsBackFailedApplyOps(t *testing.T) {
	ctx := context.Background()
	backend, applier, sourceUUID := newTestApplier(t)
	createTestCollection(t, ctx, backend, applier, sourceUUID, "orders", "items")
	insertTestDocuments(t, ctx, backend, "orders", "items", must.NotFail(types.NewDocument("_id", int32(1), "value", int32(0))))

	valid := embeddedOperation("u", "orders.items", sourceUUID,
		must.NotFail(types.NewDocument("_id", int32(1), "value", int32(1))),
		must.NotFail(types.NewDocument("_id", int32(1))),
	)
	missing := embeddedOperation("u", "orders.items", sourceUUID,
		must.NotFail(types.NewDocument("_id", int32(2), "value", int32(2))),
		must.NotFail(types.NewDocument("_id", int32(2))),
	)
	entry := makeTransactionEntry(t, 30, 40, nullOpTimeDocument(), false, false, valid, missing)
	if err := applier.Apply(ctx, entry); err == nil {
		t.Fatal("applyOps with missing update target succeeded")
	}
	assertStoredDocument(t, ctx, backend, "orders", "items", must.NotFail(types.NewDocument("_id", int32(1), "value", int32(0))))
}

func newTestApplier(t *testing.T) (backends.Backend, *Applier, string) {
	t.Helper()
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.Close)
	store, err := control.Open(t.TempDir(), control.Configuration{SetName: "rs0", Branch: "mongo", MemberHost: "dumbo:27017"})
	if err != nil {
		t.Fatal(err)
	}
	applier, err := NewApplier(backend, store)
	if err != nil {
		t.Fatal(err)
	}
	return backend, applier, "12345678-1234-4234-9234-123456789abc"
}

func makeOplogEntry(t *testing.T, increment uint32, kind, namespace, sourceUUID string, object, object2 *types.Document) Entry {
	t.Helper()
	document := makeOplogDocument(increment, kind, namespace, sourceUUID, object, object2)
	entry, err := ParseEntry(document)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func makeOplogDocument(increment uint32, kind, namespace, sourceUUID string, object, object2 *types.Document) *types.Document {
	document := must.NotFail(types.NewDocument(
		"ts", types.Timestamp(uint64(100)<<32|uint64(increment)), "t", int64(8), "op", kind, "ns", namespace, "o", object,
	))
	if sourceUUID != "" {
		document.Set("ui", uuidBinary(uuid.MustParse(sourceUUID)))
	}
	if object2 != nil {
		document.Set("o2", object2)
	}
	return document
}

func makeTransactionEntry(t *testing.T, increment uint32, txnNumber int64, previous *types.Document, partial, prepare bool, operations ...*types.Document) Entry {
	t.Helper()
	array := must.NotFail(types.NewArray(documentValues(operations)...))
	command := must.NotFail(types.NewDocument("applyOps", array))
	if partial {
		command.Set("partialTxn", true)
	}
	if prepare {
		command.Set("prepare", true)
	}
	document := makeOplogDocument(increment, "c", "admin.$cmd", "", command, nil)
	document.Set("lsid", transactionLSID())
	document.Set("txnNumber", txnNumber)
	document.Set("prevOpTime", previous)
	entry, err := ParseEntry(document)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func makeTransactionCommandEntry(t *testing.T, increment uint32, txnNumber int64, commandName string, previous *types.Document) Entry {
	t.Helper()
	command := must.NotFail(types.NewDocument(commandName, int32(1)))
	document := makeOplogDocument(increment, "c", "admin.$cmd", "", command, nil)
	document.Set("lsid", transactionLSID())
	document.Set("txnNumber", txnNumber)
	document.Set("prevOpTime", previous)
	entry, err := ParseEntry(document)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func embeddedOperation(kind, namespace, sourceUUID string, object, object2 *types.Document) *types.Document {
	document := must.NotFail(types.NewDocument("op", kind, "ns", namespace, "ui", uuidBinary(uuid.MustParse(sourceUUID)), "o", object))
	if object2 != nil {
		document.Set("o2", object2)
	}
	return document
}

func transactionLSID() *types.Document {
	return must.NotFail(types.NewDocument("id", uuidBinary(uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"))))
}

func nullOpTimeDocument() *types.Document {
	return must.NotFail(types.NewDocument("ts", types.Timestamp(0), "t", int64(-1)))
}

func opTimeDocument(opTime control.OpTime) *types.Document {
	return must.NotFail(types.NewDocument("ts", types.Timestamp(uint64(opTime.Seconds)<<32|uint64(opTime.Increment)), "t", opTime.Term))
}

func documentValues(documents []*types.Document) []any {
	values := make([]any, len(documents))
	for i, document := range documents {
		values[i] = document
	}
	return values
}

func createTestCollection(t *testing.T, ctx context.Context, backend backends.Backend, applier *Applier, sourceUUID, databaseName, collectionName string) {
	t.Helper()
	catalogApplier, err := catalog.NewApplier(backend, applier.store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalogApplier.Create(ctx, databaseName, backends.CreateCollectionParams{Name: collectionName}, sourceUUID, applierOpTime(1)); err != nil {
		t.Fatal(err)
	}
}

func insertTestDocuments(t *testing.T, ctx context.Context, backend backends.Backend, databaseName, collectionName string, documents ...*types.Document) {
	t.Helper()
	database, err := backend.Database(databaseName)
	if err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection(collectionName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collection.InsertAll(ctx, &backends.InsertAllParams{Docs: documents}); err != nil {
		t.Fatal(err)
	}
}

func uuidBinary(value uuid.UUID) types.Binary {
	return types.Binary{Subtype: types.BinaryUUID, B: value[:]}
}

func applierOpTime(increment uint32) control.OpTime {
	return control.OpTime{Seconds: 100, Increment: increment, Term: 8}
}

func assertStoredDocument(t *testing.T, ctx context.Context, backend backends.Backend, databaseName, collectionName string, want *types.Document) {
	t.Helper()
	database, err := backend.Database(databaseName)
	if err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection(collectionName)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := findDocumentByID(ctx, collection, must.NotFail(want.Get("_id")))
	if err != nil {
		t.Fatal(err)
	}
	if !found || types.Compare(got, want) != types.Equal {
		t.Fatalf("stored document = %v, want %v", got, want)
	}
}

func assertCollectionCount(t *testing.T, ctx context.Context, backend backends.Backend, databaseName, collectionName string, want int) {
	t.Helper()
	database, err := backend.Database(databaseName)
	if err != nil {
		t.Fatal(err)
	}
	collection, err := database.Collection(collectionName)
	if err != nil {
		t.Fatal(err)
	}
	result, err := collection.Query(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Iter.Close()
	count := 0
	for {
		_, _, err := result.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != want {
		t.Fatalf("collection count = %d, want %d", count, want)
	}
}
