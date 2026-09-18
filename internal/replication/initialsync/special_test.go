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

package initialsync

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/FerretDB/wire"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/replication/control"
	replicationspecial "github.com/dolthub/dumbodb/internal/replication/special"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestMaterializeSpecialDatabasesTranslatesAuthAndMetadata(t *testing.T) {
	ctx := context.Background()
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	store, err := control.Open(backend, control.Configuration{SetName: "rs0", MemberHost: "dumbo:27017"})
	if err != nil {
		t.Fatal(err)
	}
	bumps := 0
	applier, err := replicationspecial.NewApplier(backend, store, func() { bumps++ })
	if err != nil {
		t.Fatal(err)
	}
	userUUID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	roleUUID := uuid.MustParse("23456781-2341-4342-8342-234567819abc")
	transactionUUID := uuid.MustParse("87654321-4321-4321-8321-cba987654321")
	user := initialSyncUserDocument()
	role := initialSyncRoleDocument()
	transaction := initialSyncTransactionDocument()
	client := &boundaryClient{responses: []*wire.OpMsg{
		cloneCursorResponse(t, "firstBatch", 0, "admin.system.users", must.NotFail(types.NewDocument("$recordId", int64(1))), user),
		cloneCursorResponse(t, "firstBatch", 0, "admin.system.roles", must.NotFail(types.NewDocument("$recordId", int64(1))), role),
		cloneCursorResponse(t, "firstBatch", 0, "config.transactions", must.NotFail(types.NewDocument("$recordId", int64(1))), transaction),
	}}
	databases := []Database{
		{Name: "admin", Special: true, Collections: []Collection{
			{Name: "system.users", SourceUUID: userUUID.String(), UUIDBinary: uuidBinaryForInitialSync(userUUID)},
			{Name: "system.roles", SourceUUID: roleUUID.String(), UUIDBinary: uuidBinaryForInitialSync(roleUUID)},
			{Name: "system.version"},
		}},
		{Name: "config", Special: true, Collections: []Collection{
			{Name: "transactions", SourceUUID: transactionUUID.String(), UUIDBinary: uuidBinaryForInitialSync(transactionUUID)},
			{Name: "system.sessions"},
		}},
	}
	opTime := control.OpTime{Seconds: 100, Increment: 4, Term: 8}
	if err := MaterializeSpecialDatabases(ctx, client, applier, databases, opTime); err != nil {
		t.Fatal(err)
	}
	if bumps != 2 {
		t.Fatalf("auth generation bumps = %d, want 2", bumps)
	}
	key := must.NotFail(types.NewDocument("_id", "sales.ada"))
	gotUser, err := applier.AuthDocument(ctx, replicationspecial.UsersNamespace, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"_id", "user", "db"} {
		got, _ := gotUser.Get(field)
		want, _ := user.Get(field)
		if types.Compare(got, want) != types.Equal {
			t.Fatalf("initial-sync user %s = %v, want %v", field, got, want)
		}
	}
	roleKey := must.NotFail(types.NewDocument("_id", "sales.auditor"))
	gotRole, err := applier.AuthDocument(ctx, replicationspecial.RolesNamespace, roleKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"_id", "role", "db"} {
		got, _ := gotRole.Get(field)
		want, _ := role.Get(field)
		if types.Compare(got, want) != types.Equal {
			t.Fatalf("initial-sync role %s = %v, want %v", field, got, want)
		}
	}
	transactionKey := must.NotFail(types.NewDocument("_id", must.NotFail(transaction.Get("_id"))))
	gotTransaction, err := applier.MetadataDocument(replicationspecial.TransactionsNamespace, transactionKey)
	if err != nil {
		t.Fatal(err)
	}
	if types.Compare(gotTransaction, transaction) != types.Equal {
		t.Fatalf("initial-sync transaction = %v, want %v", gotTransaction, transaction)
	}
	if len(client.requests) != 3 {
		t.Fatalf("clone requests = %d, want 3", len(client.requests))
	}
	databasesResult, err := backend.ListDatabases(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, database := range databasesResult.Databases {
		if database.Name == "config" {
			t.Fatal("special materialization created a config database")
		}
	}
}

func TestMaterializeSpecialDatabasesPreflightsUnsupportedNamespace(t *testing.T) {
	backend, err := dolt.NewBackend(t.TempDir(), slog.Default(), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	store, err := control.Open(backend, control.Configuration{SetName: "rs0", MemberHost: "dumbo:27017"})
	if err != nil {
		t.Fatal(err)
	}
	applier, err := replicationspecial.NewApplier(backend, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = MaterializeSpecialDatabases(context.Background(), &boundaryClient{}, applier, []Database{
		{Name: "config", Special: true, Collections: []Collection{{Name: "unknown"}}},
	}, control.OpTime{Seconds: 100, Increment: 4, Term: 8})
	if !errors.Is(err, replicationspecial.ErrUnsupportedSpecialNamespace) {
		t.Fatalf("unsupported special namespace error = %v", err)
	}
}

func initialSyncUserDocument() *types.Document {
	credential := must.NotFail(types.NewDocument(
		"iterationCount", int32(15000), "salt", "c2FsdA==", "storedKey", "c3RvcmVk", "serverKey", "c2VydmVy",
	))
	userID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	return must.NotFail(types.NewDocument(
		"_id", "sales.ada", "userId", uuidBinaryForInitialSync(userID), "user", "ada", "db", "sales",
		"credentials", must.NotFail(types.NewDocument("SCRAM-SHA-256", credential)),
		"roles", must.NotFail(types.NewArray(must.NotFail(types.NewDocument("role", "readWrite", "db", "sales")))),
	))
}

func initialSyncTransactionDocument() *types.Document {
	sessionID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	return must.NotFail(types.NewDocument(
		"_id", must.NotFail(types.NewDocument(
			"id", uuidBinaryForInitialSync(sessionID),
			"uid", types.Binary{Subtype: types.BinaryGeneric, B: make([]byte, 32)},
		)),
		"txnNum", int64(9),
		"lastWriteOpTime", must.NotFail(types.NewDocument("ts", types.Timestamp(uint64(100)<<32|3), "t", int64(8))),
		"lastWriteDate", time.Unix(100, 0).UTC(),
		"state", "prepared",
	))
}

func initialSyncRoleDocument() *types.Document {
	return must.NotFail(types.NewDocument(
		"_id", "sales.auditor", "role", "auditor", "db", "sales",
		"privileges", must.NotFail(types.NewArray()), "roles", must.NotFail(types.NewArray()),
	))
}

func uuidBinaryForInitialSync(value uuid.UUID) types.Binary {
	return types.Binary{Subtype: types.BinaryUUID, B: value[:]}
}
