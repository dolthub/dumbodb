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
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestTranslateUserPreservesSupportedMongoDBAuthDocument(t *testing.T) {
	userID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	credential := must.NotFail(types.NewDocument(
		"iterationCount", int32(15000), "salt", "c2FsdA==", "storedKey", "c3RvcmVk", "serverKey", "c2VydmVy",
	))
	source := must.NotFail(types.NewDocument(
		"_id", "sales.ada", "userId", types.Binary{Subtype: types.BinaryUUID, B: userID[:]},
		"user", "ada", "db", "sales",
		"credentials", must.NotFail(types.NewDocument("SCRAM-SHA-256", credential)),
		"roles", must.NotFail(types.NewArray(must.NotFail(types.NewDocument("role", "readWrite", "db", "sales")))),
		"customData", must.NotFail(types.NewDocument("team", "database")),
		"authenticationRestrictions", must.NotFail(types.NewArray(must.NotFail(types.NewDocument(
			"clientSource", must.NotFail(types.NewArray("10.0.0.0/8")),
		)))),
	))
	translated, err := TranslateUser(source, "rs0")
	if err != nil {
		t.Fatal(err)
	}
	if translated.Owner != "rs0" {
		t.Fatalf("replication owner = %v", translated.Owner)
	}
	if types.Compare(source, translated.Document) != types.Equal {
		t.Fatal("translation changed supported user fields")
	}
	translated.Document.Set("user", "changed")
	if user, _ := source.Get("user"); user != "ada" {
		t.Fatal("translation did not copy source document")
	}
}

func TestTranslateUserRejectsUnsupportedCredentialMechanism(t *testing.T) {
	userID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	source := must.NotFail(types.NewDocument(
		"_id", "$external.client", "userId", types.Binary{Subtype: types.BinaryUUID, B: userID[:]},
		"user", "client", "db", "$external",
		"credentials", must.NotFail(types.NewDocument("external", true)),
		"roles", must.NotFail(types.NewArray()),
	))
	_, err := TranslateUser(source, "rs0")
	var unsupported *UnsupportedAuthSemanticError
	if !errors.As(err, &unsupported) || unsupported.Identity != "$external.client" || unsupported.Field != "credentials.external" {
		t.Fatalf("unsupported credential error = %#v", err)
	}
}

func TestTranslateRoleValidatesResourcesAndActions(t *testing.T) {
	supported := must.NotFail(types.NewDocument(
		"_id", "sales.auditor", "role", "auditor", "db", "sales",
		"privileges", must.NotFail(types.NewArray(must.NotFail(types.NewDocument(
			"resource", must.NotFail(types.NewDocument("db", "sales", "collection", "orders")),
			"actions", must.NotFail(types.NewArray("find", "listIndexes")),
		)))),
		"roles", must.NotFail(types.NewArray(must.NotFail(types.NewDocument("role", "read", "db", "sales")))),
	))
	translated, err := TranslateRole(supported, "rs0")
	if err != nil {
		t.Fatal(err)
	}
	if translated.Owner != "rs0" || types.Compare(supported, translated.Document) != types.Equal {
		t.Fatalf("translated role = %+v", translated)
	}

	unsupported := supported.DeepCopy()
	unsupportedPrivileges := must.NotFail(types.NewArray(must.NotFail(types.NewDocument(
		"resource", must.NotFail(types.NewDocument("db", "sales", "collection", "orders")),
		"actions", must.NotFail(types.NewArray("moveChunk")),
	))))
	unsupported.Set("privileges", unsupportedPrivileges)
	_, err = TranslateRole(unsupported, "rs0")
	var semantic *UnsupportedAuthSemanticError
	if !errors.As(err, &semantic) || semantic.Identity != "sales.auditor" || semantic.Field != "privileges[0].actions[0]" {
		t.Fatalf("unsupported action error = %#v", err)
	}
}

func TestTranslateRoleRejectsReservedConfigResource(t *testing.T) {
	source := must.NotFail(types.NewDocument(
		"_id", "admin.configReader", "role", "configReader", "db", "admin",
		"privileges", must.NotFail(types.NewArray(must.NotFail(types.NewDocument(
			"resource", must.NotFail(types.NewDocument("db", "config", "collection", "transactions")),
			"actions", must.NotFail(types.NewArray("find")),
		)))),
		"roles", must.NotFail(types.NewArray()),
	))
	_, err := TranslateRole(source, "rs0")
	var unsupported *UnsupportedAuthSemanticError
	if !errors.As(err, &unsupported) || unsupported.Field != "privileges[0].resource" {
		t.Fatalf("reserved resource error = %#v", err)
	}
}
