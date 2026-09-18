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
	"slices"
	"testing"

	"github.com/dolthub/dumbodb/internal/replication/special"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestAffectedDatabasesReturnsOnlyMutatedRoots(t *testing.T) {
	ctx := context.Background()
	backend, applier, ordersUUID := newTestApplier(t)
	accountsUUID := "87654321-4321-4321-8321-cba987654321"
	createTestCollection(t, ctx, backend, applier, ordersUUID, "orders", "items")
	createTestCollection(t, ctx, backend, applier, accountsUUID, "accounts", "customers")

	tests := []struct {
		name  string
		entry Entry
		want  []string
	}{
		{
			name: "UUID location overrides stale namespace",
			entry: makeOplogEntry(t, 2, "i", "stale.items", ordersUUID,
				must.NotFail(types.NewDocument("_id", int32(1))), nil),
			want: []string{"orders"},
		},
		{
			name: "source-local metadata is ignored",
			entry: makeOplogEntry(t, 3, "i", "admin.system.keys", "",
				must.NotFail(types.NewDocument("_id", int32(1))), nil),
		},
		{
			name: "translated special state lives in admin",
			entry: makeOplogEntry(t, 4, "i", special.TransactionsNamespace, "",
				replicatedTransactionRecord(), nil),
			want: []string{"admin"},
		},
		{
			name: "applyOps spans databases",
			entry: makeTransactionEntry(t, 5, 40, nullOpTimeDocument(), false, false,
				embeddedOperation("i", "orders.items", ordersUUID, must.NotFail(types.NewDocument("_id", int32(2))), nil),
				embeddedOperation("i", "accounts.customers", accountsUUID, must.NotFail(types.NewDocument("_id", int32(3))), nil),
			),
			want: []string{"accounts", "orders"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := applier.AffectedDatabases(ctx, test.entry)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("affected databases = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAffectedDatabasesDefersTransactionFragmentsUntilCommit(t *testing.T) {
	ctx := context.Background()
	backend, applier, ordersUUID := newTestApplier(t)
	accountsUUID := "87654321-4321-4321-8321-cba987654321"
	createTestCollection(t, ctx, backend, applier, ordersUUID, "orders", "items")
	createTestCollection(t, ctx, backend, applier, accountsUUID, "accounts", "customers")

	partial := makeTransactionEntry(t, 10, 41, nullOpTimeDocument(), true, false,
		embeddedOperation("i", "orders.items", ordersUUID, must.NotFail(types.NewDocument("_id", int32(1))), nil),
	)
	affected, err := applier.AffectedDatabases(ctx, partial)
	if err != nil {
		t.Fatal(err)
	}
	if len(affected) != 0 {
		t.Fatalf("staged fragment affected databases = %v, want none", affected)
	}
	if err := applier.Apply(ctx, partial); err != nil {
		t.Fatal(err)
	}

	terminal := makeTransactionEntry(t, 11, 41, opTimeDocument(partial.OpTime), false, false,
		embeddedOperation("i", "accounts.customers", accountsUUID, must.NotFail(types.NewDocument("_id", int32(2))), nil),
	)
	affected, err = applier.AffectedDatabases(ctx, terminal)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"accounts", "orders"}; !slices.Equal(affected, want) {
		t.Fatalf("terminal transaction affected databases = %v, want %v", affected, want)
	}
}

func TestAffectedDatabasesPlansCreateAndInsertTogether(t *testing.T) {
	ctx := context.Background()
	_, applier, _ := newTestApplier(t)
	sourceUUID := "87654321-4321-4321-8321-cba987654321"
	create := embeddedOperation("c", "events.$cmd", sourceUUID,
		must.NotFail(types.NewDocument("create", "records")), nil)
	insert := embeddedOperation("i", "events.records", sourceUUID,
		must.NotFail(types.NewDocument("_id", int32(1))), nil)
	entry := makeTransactionEntry(t, 12, 42, nullOpTimeDocument(), false, false, create, insert)
	affected, err := applier.AffectedDatabases(ctx, entry)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(affected, []string{"events"}) {
		t.Fatalf("create-and-insert affected databases = %v, want [events]", affected)
	}
}
