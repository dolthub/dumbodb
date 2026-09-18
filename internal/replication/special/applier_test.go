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

package special

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestAuthApplierOwnsDocumentsAndRefusesLocalCollision(t *testing.T) {
	ctx := context.Background()
	backend, store, applier := newSpecialTestApplier(t, nil)
	local := replicatedUserDocument("sales.ada")
	database, err := backend.Database("admin")
	if err != nil {
		t.Fatal(err)
	}
	users, err := database.Collection("system.users")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.InsertAll(ctx, &backends.InsertAllParams{Docs: []*types.Document{local}}); err != nil {
		t.Fatal(err)
	}
	if err := applier.InsertAuthDocument(ctx, UsersNamespace, replicatedUserDocument("sales.ada"), specialOpTime(1)); err == nil {
		t.Fatal("replicated user overwrote a local administrator")
	}
	if _, ok := store.AuthOwnershipFor(UsersNamespace, "sales.ada"); ok {
		t.Fatal("local collision acquired replication ownership")
	}
	key := must.NotFail(types.NewDocument("_id", "sales.ada"))
	if err := applier.DeleteAuthDocument(ctx, UsersNamespace, key, specialOpTime(2)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := findDocumentByID(ctx, users, "sales.ada"); err != nil || !found {
		t.Fatalf("source deletion changed local administrator: found=%v err=%v", found, err)
	}
	ownership, ok := store.AuthOwnershipFor(UsersNamespace, "sales.ada")
	if !ok || !ownership.Dropped {
		t.Fatalf("missing source deletion tombstone = %+v, %v", ownership, ok)
	}
}

func TestAuthApplierPersistsOwnershipAndInvalidatesAuthGeneration(t *testing.T) {
	ctx := context.Background()
	bumps := 0
	_, store, applier := newSpecialTestApplier(t, func() { bumps++ })
	user := replicatedUserDocument("sales.ada")
	if err := applier.InsertAuthDocument(ctx, UsersNamespace, user, specialOpTime(1)); err != nil {
		t.Fatal(err)
	}
	if err := applier.InsertAuthDocument(ctx, UsersNamespace, user, specialOpTime(1)); err != nil {
		t.Fatalf("idempotent auth replay after backend canonicalization: %v", err)
	}
	if bumps != 1 {
		t.Fatalf("auth generation bumps = %d, want 1", bumps)
	}
	updated := user.DeepCopy()
	updated.Set("customData", must.NotFail(types.NewDocument("team", "storage")))
	if err := applier.ReplaceAuthDocument(ctx, UsersNamespace, updated, specialOpTime(2)); err != nil {
		t.Fatal(err)
	}
	if err := applier.DeleteAuthDocument(ctx, UsersNamespace, must.NotFail(types.NewDocument("_id", "sales.ada")), specialOpTime(3)); err != nil {
		t.Fatal(err)
	}
	if bumps != 3 {
		t.Fatalf("auth generation bumps = %d, want 3", bumps)
	}
	ownership, ok := store.AuthOwnershipFor(UsersNamespace, "sales.ada")
	if !ok || !ownership.Dropped || ownership.LastUpdateOpTime != specialOpTime(3) {
		t.Fatalf("deleted ownership = %+v, %v", ownership, ok)
	}
}

func TestMetadataApplierValidatesAndSurvivesControlStoreRestart(t *testing.T) {
	directory := t.TempDir()
	configuration := control.Configuration{SetName: "rs0", MemberHost: "dumbo:27017"}
	backend, err := dolt.NewBackend(directory, slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	store, err := control.Open(backend, configuration)
	if err != nil {
		t.Fatal(err)
	}
	applier, err := NewApplier(backend, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := transactionDocument()
	if err := applier.PutMetadataDocument(TransactionsNamespace, record, specialOpTime(4)); err != nil {
		t.Fatal(err)
	}
	retryImage := retryImageDocument()
	if err := applier.PutMetadataDocument(RetryImagesNamespace, retryImage, specialOpTime(5)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := control.Open(backend, configuration)
	if err != nil {
		t.Fatal(err)
	}
	reopenedApplier, err := NewApplier(backend, reopened, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := must.NotFail(types.NewDocument("_id", must.NotFail(record.Get("_id"))))
	got, err := reopenedApplier.MetadataDocument(TransactionsNamespace, key)
	if err != nil {
		t.Fatal(err)
	}
	if types.Compare(got, record) != types.Equal {
		t.Fatalf("metadata after restart = %v, want %v", got, record)
	}
	retryKey := must.NotFail(types.NewDocument("_id", must.NotFail(retryImage.Get("_id"))))
	gotRetryImage, err := reopenedApplier.MetadataDocument(RetryImagesNamespace, retryKey)
	if err != nil {
		t.Fatal(err)
	}
	if types.Compare(gotRetryImage, retryImage) != types.Equal {
		t.Fatalf("retry image after restart = %v, want %v", gotRetryImage, retryImage)
	}
	invalid := record.DeepCopy()
	invalid.Set("lastWriteDate", "not a date")
	if err := reopenedApplier.PutMetadataDocument(TransactionsNamespace, invalid, specialOpTime(6)); err == nil {
		t.Fatal("invalid transaction record was stored")
	}
	if err := reopenedApplier.ApplyInitialDocument(context.Background(), ChangeStreamPreimagesNamespace, record, specialOpTime(6)); err == nil {
		t.Fatal("change-stream pre-image was silently stored")
	}
}

func newSpecialTestApplier(t *testing.T, bump func()) (backends.Backend, *control.Store, *Applier) {
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
	applier, err := NewApplier(backend, store, bump)
	if err != nil {
		t.Fatal(err)
	}
	return backend, store, applier
}

func replicatedUserDocument(identity string) *types.Document {
	userID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	credential := must.NotFail(types.NewDocument(
		"iterationCount", int32(15000), "salt", "c2FsdA==", "storedKey", "c3RvcmVk", "serverKey", "c2VydmVy",
	))
	return must.NotFail(types.NewDocument(
		"_id", identity, "userId", types.Binary{Subtype: types.BinaryUUID, B: userID[:]},
		"user", "ada", "db", "sales", "credentials", must.NotFail(types.NewDocument("SCRAM-SHA-256", credential)),
		"roles", must.NotFail(types.NewArray(must.NotFail(types.NewDocument("role", "readWrite", "db", "sales")))),
	))
}

func transactionDocument() *types.Document {
	sessionID := uuid.MustParse("87654321-4321-4321-8321-cba987654321")
	return must.NotFail(types.NewDocument(
		"_id", must.NotFail(types.NewDocument(
			"id", types.Binary{Subtype: types.BinaryUUID, B: sessionID[:]},
			"uid", types.Binary{Subtype: types.BinaryGeneric, B: make([]byte, 32)},
		)),
		"txnNum", int64(7),
		"lastWriteOpTime", must.NotFail(types.NewDocument("ts", types.Timestamp(uint64(100)<<32|2), "t", int64(3))),
		"lastWriteDate", time.Unix(100, 0).UTC(),
		"state", "committed",
	))
}

func retryImageDocument() *types.Document {
	record := transactionDocument()
	id, _ := record.Get("_id")
	return must.NotFail(types.NewDocument(
		"_id", id, "txnNum", int64(7), "ts", types.Timestamp(uint64(100)<<32|3),
		"imageKind", "preImage", "image", must.NotFail(types.NewDocument("_id", int32(1), "value", "before")),
	))
}

func specialOpTime(value uint32) control.OpTime {
	return control.OpTime{Seconds: 100, Increment: value, Term: 3}
}
