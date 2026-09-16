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
	"time"

	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/replication/catalog"
	"github.com/dolthub/dumbodb/internal/replication/control"
	replicationspecial "github.com/dolthub/dumbodb/internal/replication/special"
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

func TestApplierRoutesReplicatedAuthAndInvalidatesGeneration(t *testing.T) {
	ctx := context.Background()
	bumps := 0
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	store, err := control.Open(backend, control.Configuration{SetName: "rs0", MemberHost: "dumbo:27017"})
	if err != nil {
		t.Fatal(err)
	}
	applier, err := NewApplierWithAuthGeneration(backend, store, func() { bumps++ })
	if err != nil {
		t.Fatal(err)
	}
	user := replicatedAuthUser()
	if err := applier.Apply(ctx, makeOplogEntry(t, 1, "i", replicationspecial.UsersNamespace, "", user, nil)); err != nil {
		t.Fatal(err)
	}
	diff := must.NotFail(types.NewDocument("i", must.NotFail(types.NewDocument(
		"customData", must.NotFail(types.NewDocument("team", "storage")),
	))))
	update := must.NotFail(types.NewDocument("$v", int32(2), "diff", diff))
	key := must.NotFail(types.NewDocument("_id", "sales.ada"))
	if err := applier.Apply(ctx, makeOplogEntry(t, 2, "u", replicationspecial.UsersNamespace, "", update, key)); err != nil {
		t.Fatal(err)
	}
	stored, err := applier.special.AuthDocument(ctx, replicationspecial.UsersNamespace, key)
	if err != nil {
		t.Fatal(err)
	}
	if customData, _ := stored.Get("customData"); customData == nil {
		t.Fatal("auth update was not routed to the replicated identity")
	}
	if err := applier.Apply(ctx, makeOplogEntry(t, 3, "d", replicationspecial.UsersNamespace, "", key, nil)); err != nil {
		t.Fatal(err)
	}
	if bumps != 3 {
		t.Fatalf("auth generation bumps = %d, want 3", bumps)
	}
	if ownership, ok := store.AuthOwnershipFor(replicationspecial.UsersNamespace, "sales.ada"); !ok || !ownership.Dropped {
		t.Fatalf("deleted auth ownership = %+v, %v", ownership, ok)
	}
}

func TestApplierStoresConfigMetadataWithoutCreatingConfigDatabase(t *testing.T) {
	ctx := context.Background()
	backend, applier, _ := newTestApplier(t)
	transaction := replicatedTransactionRecord()
	if err := applier.Apply(ctx, makeOplogEntry(t, 1, "i", replicationspecial.TransactionsNamespace, "", transaction, nil)); err != nil {
		t.Fatal(err)
	}
	transactionKey := must.NotFail(types.NewDocument("_id", must.NotFail(transaction.Get("_id"))))
	stateDiff := must.NotFail(types.NewDocument("u", must.NotFail(types.NewDocument("state", "aborted"))))
	update := must.NotFail(types.NewDocument("$v", int32(2), "diff", stateDiff))
	if err := applier.Apply(ctx, makeOplogEntry(t, 2, "u", replicationspecial.TransactionsNamespace, "", update, transactionKey)); err != nil {
		t.Fatal(err)
	}
	stored, err := applier.special.MetadataDocument(replicationspecial.TransactionsNamespace, transactionKey)
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := stored.Get("state"); state != "aborted" {
		t.Fatalf("transaction state = %v, want aborted", state)
	}

	retryImage := replicatedRetryImageRecord()
	if err := applier.Apply(ctx, makeOplogEntry(t, 3, "i", replicationspecial.RetryImagesNamespace, "", retryImage, nil)); err != nil {
		t.Fatal(err)
	}
	databases, err := backend.ListDatabases(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, database := range databases.Databases {
		if database.Name == "config" {
			t.Fatal("replication metadata materialized the reserved config database")
		}
	}
	if err := applier.Apply(ctx, makeOplogEntry(t, 4, "i", replicationspecial.ChangeStreamPreimagesNamespace, "", retryImage, nil)); err == nil {
		t.Fatal("unsupported change-stream pre-image was silently applied")
	}
	reservedCommand := must.NotFail(types.NewDocument("dropDatabase", int32(1)))
	if err := applier.Apply(ctx, makeOplogEntry(t, 5, "c", "config.$cmd", "", reservedCommand, nil)); !errors.Is(err, replicationspecial.ErrUnsupportedSpecialNamespace) {
		t.Fatalf("reserved config command error = %v", err)
	}
	ignoredUUID := "87654321-4321-4321-8321-cba987654321"
	createIndexBuilds := must.NotFail(types.NewDocument("create", "system.indexBuilds"))
	if err := applier.Apply(ctx, makeOplogEntry(t, 6, "c", "config.$cmd", ignoredUUID, createIndexBuilds, nil)); err != nil {
		t.Fatalf("creating source-local index build metadata: %v", err)
	}
	indexBuild := must.NotFail(types.NewDocument("_id", "build"))
	if err := applier.Apply(ctx, makeOplogEntry(t, 7, "i", replicationspecial.IndexBuildsNamespace, ignoredUUID, indexBuild, nil)); err != nil {
		t.Fatalf("applying source-local index build metadata: %v", err)
	}
	databases, err = backend.ListDatabases(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, database := range databases.Databases {
		if database.Name == "config" {
			t.Fatal("source-local index build metadata created the config database")
		}
	}
}

func TestApplierSpecialStateContinuesAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dataDirectory := t.TempDir()
	configuration := control.Configuration{SetName: "rs0", MemberHost: "dumbo:27017"}
	backend, err := dolt.NewBackend(dataDirectory, slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	store, err := control.Open(backend, configuration)
	if err != nil {
		t.Fatal(err)
	}
	applier, err := NewApplier(backend, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := applier.Apply(ctx, makeOplogEntry(t, 1, "i", replicationspecial.UsersNamespace, "", replicatedAuthUser(), nil)); err != nil {
		t.Fatal(err)
	}
	transaction := replicatedTransactionRecord()
	if err := applier.Apply(ctx, makeOplogEntry(t, 2, "i", replicationspecial.TransactionsNamespace, "", transaction, nil)); err != nil {
		t.Fatal(err)
	}
	backend.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedBackend, err := dolt.NewBackend(dataDirectory, slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedBackend.Close()
	reopenedStore, err := control.Open(reopenedBackend, configuration)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewApplier(reopenedBackend, reopenedStore)
	if err != nil {
		t.Fatal(err)
	}
	userKey := must.NotFail(types.NewDocument("_id", "sales.ada"))
	userDiff := must.NotFail(types.NewDocument("i", must.NotFail(types.NewDocument(
		"customData", must.NotFail(types.NewDocument("restart", true)),
	))))
	if err := reopened.Apply(ctx, makeOplogEntry(t, 3, "u", replicationspecial.UsersNamespace, "",
		must.NotFail(types.NewDocument("$v", int32(2), "diff", userDiff)), userKey)); err != nil {
		t.Fatal(err)
	}
	user, err := reopened.special.AuthDocument(ctx, replicationspecial.UsersNamespace, userKey)
	if err != nil {
		t.Fatal(err)
	}
	if customData, _ := user.Get("customData"); customData == nil {
		t.Fatal("restarted applier did not recover auth ownership")
	}
	transactionKey := must.NotFail(types.NewDocument("_id", must.NotFail(transaction.Get("_id"))))
	if _, err := reopened.special.MetadataDocument(replicationspecial.TransactionsNamespace, transactionKey); err != nil {
		t.Fatalf("restarted applier did not recover transaction metadata: %v", err)
	}
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
	indexBuild := must.NotFail(types.NewDocument(
		"v", int32(2), "key", must.NotFail(types.NewDocument("status", int32(1))), "name", "status_1",
	))
	startIndexBuild := must.NotFail(types.NewDocument(
		"startIndexBuild", "items", "indexBuildUUID", uuidBinary(uuid.New()),
		"indexes", must.NotFail(types.NewArray(indexBuild)),
	))
	if err := applier.Apply(ctx, makeOplogEntry(t, 3, "c", "orders.$cmd", sourceUUID, startIndexBuild, nil)); err != nil {
		t.Fatal(err)
	}
	commitIndexBuild := must.NotFail(types.NewDocument(
		"commitIndexBuild", "items", "indexBuildUUID", must.NotFail(startIndexBuild.Get("indexBuildUUID")),
		"indexes", must.NotFail(types.NewArray(indexBuild)),
	))
	if err := applier.Apply(ctx, makeOplogEntry(t, 4, "c", "orders.$cmd", sourceUUID, commitIndexBuild, nil)); err != nil {
		t.Fatal(err)
	}
	rename := must.NotFail(types.NewDocument("renameCollection", "orders.items", "to", "orders.renamed", "stayTemp", false))
	if err := applier.Apply(ctx, makeOplogEntry(t, 5, "c", "orders.$cmd", sourceUUID, rename, nil)); err != nil {
		t.Fatal(err)
	}
	dropIndex := must.NotFail(types.NewDocument("dropIndexes", "renamed", "index", "account_1"))
	if err := applier.Apply(ctx, makeOplogEntry(t, 6, "c", "orders.$cmd", sourceUUID, dropIndex, nil)); err != nil {
		t.Fatal(err)
	}
	drop := must.NotFail(types.NewDocument("drop", "renamed"))
	if err := applier.Apply(ctx, makeOplogEntry(t, 7, "c", "orders.$cmd", sourceUUID, drop, nil)); err != nil {
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

func TestApplierCreatesCollectionsInsideApplyOps(t *testing.T) {
	ctx := context.Background()
	backend, applier, _ := newTestApplier(t)
	sourceUUID := "87654321-4321-4321-8321-cba987654321"
	create := embeddedOperation("c", "orders.$cmd", sourceUUID,
		must.NotFail(types.NewDocument(
			"create", "events",
			"idIndex", must.NotFail(types.NewDocument(
				"key", must.NotFail(types.NewDocument("_id", int32(1))), "name", "_id_", "v", int32(2),
			)),
		)), nil,
	)
	document := must.NotFail(types.NewDocument("_id", int32(1), "event", "created"))
	insert := embeddedOperation("i", "orders.events", sourceUUID, document, document)
	entry := makeTransactionEntry(t, 12, 23, nullOpTimeDocument(), false, false, create, insert)
	if err := applier.Apply(ctx, entry); err != nil {
		t.Fatal(err)
	}
	assertStoredDocument(t, ctx, backend, "orders", "events", document)
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

func TestApplierUsesMainAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	configuration := control.Configuration{SetName: "rs0", MemberHost: "dumbo:27017"}
	backend, err := dolt.NewBackend(dataDir, slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	store, err := control.Open(backend, configuration)
	if err != nil {
		t.Fatal(err)
	}
	applier, err := NewApplier(backend, store)
	if err != nil {
		t.Fatal(err)
	}
	sourceUUID := "12345678-1234-4234-9234-123456789abc"
	createTestCollection(t, ctx, backend, applier, sourceUUID, "orders", "items")
	document := must.NotFail(types.NewDocument("_id", int32(1), "value", "history"))
	if err := applier.Apply(ctx, makeOplogEntry(t, 2, "i", "orders.items", sourceUUID, document, nil)); err != nil {
		t.Fatal(err)
	}
	assertStoredDocument(t, ctx, backend, "orders", "items", document)
	secondUUID := "87654321-4321-4321-8321-cba987654321"
	createTestCollection(t, ctx, backend, applier, secondUUID, "customers", "profiles")
	secondDocument := must.NotFail(types.NewDocument("_id", int32(2), "value", "second database"))
	if err := applier.Apply(ctx, makeOplogEntry(t, 3, "i", "customers.profiles", secondUUID, secondDocument, nil)); err != nil {
		t.Fatal(err)
	}
	assertStoredDocument(t, ctx, backend, "customers", "profiles", secondDocument)
	backend.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedBackend, err := dolt.NewBackend(dataDir, slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedBackend.Close()
	reopenedStore, err := control.Open(reopenedBackend, configuration)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewApplier(reopenedBackend, reopenedStore)
	if err != nil {
		t.Fatal(err)
	}
	location, err := reopened.catalog.Resolve(ctx, sourceUUID)
	if err != nil {
		t.Fatal(err)
	}
	if location.Database != "orders" || location.Collection != "items" {
		t.Fatalf("reopened location = %+v", location)
	}
	assertStoredDocument(t, ctx, reopenedBackend, "orders", "items", document)
	secondLocation, err := reopened.catalog.Resolve(ctx, secondUUID)
	if err != nil {
		t.Fatal(err)
	}
	if secondLocation.Database != "customers" || secondLocation.Collection != "profiles" {
		t.Fatalf("reopened second location = %+v", secondLocation)
	}
	assertStoredDocument(t, ctx, reopenedBackend, "customers", "profiles", secondDocument)
	dropDatabase := must.NotFail(types.NewDocument("dropDatabase", int32(1)))
	if err := reopened.Apply(ctx, makeOplogEntry(t, 4, "c", "orders.$cmd", "", dropDatabase, nil)); err != nil {
		t.Fatal(err)
	}
	mainDatabase, err := reopenedBackend.Database("orders")
	if err != nil {
		t.Fatal(err)
	}
	mainCollections, err := mainDatabase.ListCollections(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(mainCollections.Collections) != 0 {
		t.Fatalf("replicated database survived dropDatabase: %+v", mainCollections.Collections)
	}
	assertStoredDocument(t, ctx, reopenedBackend, "customers", "profiles", secondDocument)
}

func newTestApplier(t *testing.T) (backends.Backend, *Applier, string) {
	t.Helper()
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.Close)
	store, err := control.Open(backend, control.Configuration{SetName: "rs0", MemberHost: "dumbo:27017"})
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

func replicatedAuthUser() *types.Document {
	credential := must.NotFail(types.NewDocument(
		"iterationCount", int32(15000), "salt", "c2FsdA==", "storedKey", "c3RvcmVk", "serverKey", "c2VydmVy",
	))
	return must.NotFail(types.NewDocument(
		"_id", "sales.ada", "userId", uuidBinary(uuid.MustParse("12345678-1234-4234-9234-123456789abc")),
		"user", "ada", "db", "sales", "credentials", must.NotFail(types.NewDocument("SCRAM-SHA-256", credential)),
		"roles", must.NotFail(types.NewArray(must.NotFail(types.NewDocument("role", "readWrite", "db", "sales")))),
	))
}

func replicatedTransactionRecord() *types.Document {
	return must.NotFail(types.NewDocument(
		"_id", replicatedSessionID(),
		"txnNum", int64(7),
		"lastWriteOpTime", must.NotFail(types.NewDocument("ts", types.Timestamp(uint64(100)<<32|2), "t", int64(8))),
		"lastWriteDate", time.Unix(100, 0).UTC(),
		"state", "committed",
	))
}

func replicatedRetryImageRecord() *types.Document {
	return must.NotFail(types.NewDocument(
		"_id", replicatedSessionID(),
		"txnNum", int64(7), "ts", types.Timestamp(uint64(100)<<32|3), "imageKind", "postImage",
		"image", must.NotFail(types.NewDocument("_id", int32(1), "value", "after")), "invalidated", false,
	))
}

func replicatedSessionID() *types.Document {
	return must.NotFail(types.NewDocument(
		"id", uuidBinary(uuid.MustParse("87654321-4321-4321-8321-cba987654321")),
		"uid", types.Binary{Subtype: types.BinaryGeneric, B: make([]byte, 32)},
	))
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
