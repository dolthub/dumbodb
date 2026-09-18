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

	"github.com/FerretDB/wire"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/replication/catalog"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestMaterializeCollectionLoadsAndIndexesMain(t *testing.T) {
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
	catalogApplier, err := catalog.NewApplier(backend, store)
	if err != nil {
		t.Fatal(err)
	}
	sourceUUID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	firstToken := must.NotFail(types.NewDocument("$recordId", int64(2)))
	finalToken := must.NotFail(types.NewDocument("$recordId", int64(3)))
	client := &boundaryClient{responses: []*wire.OpMsg{
		cloneCursorResponse(t, "firstBatch", 77, "orders.items", firstToken,
			must.NotFail(types.NewDocument("_id", int32(1), "account", "a")),
			must.NotFail(types.NewDocument("_id", int32(2), "account", "b")),
		),
		cloneCursorResponse(t, "nextBatch", 0, "orders.items", finalToken,
			must.NotFail(types.NewDocument("_id", int32(3), "account", "c")),
		),
	}}
	indexes := []*types.Document{
		must.NotFail(types.NewDocument("v", int32(2), "key", must.NotFail(types.NewDocument("_id", int32(1))), "name", "_id_")),
		must.NotFail(types.NewDocument("v", int32(2), "key", must.NotFail(types.NewDocument("account", int32(1))), "name", "account_1", "unique", true)),
	}
	result, err := MaterializeCollection(ctx, client, catalogApplier, "orders", Collection{
		Name: "items", SourceUUID: sourceUUID.String(), UUIDBinary: types.Binary{Subtype: types.BinaryUUID, B: sourceUUID[:]},
		Options: must.NotFail(types.NewDocument()), Indexes: indexes,
	}, control.OpTime{Seconds: 100, Increment: 1, Term: 8}, LoaderLimits{Documents: 2, Bytes: 1024}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Location.Database != "orders" || result.Location.Collection != "items" || result.Loader.Documents != 3 || result.Loader.Batches != 2 {
		t.Fatalf("materialize result = %+v", result)
	}
	if types.Compare(result.ResumeToken, finalToken) != types.Equal {
		t.Fatalf("resume token = %v", result.ResumeToken)
	}
	mainDatabase, err := backend.Database("orders")
	if err != nil {
		t.Fatal(err)
	}
	mainCollection, err := mainDatabase.Collection("items")
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentCount(t, ctx, mainCollection, 3)
	listed, err := mainCollection.ListIndexes(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Indexes) != 2 || listed.Indexes[1].Name != "account_1" || !listed.Indexes[1].Unique {
		t.Fatalf("materialized indexes = %+v", listed.Indexes)
	}
}

func assertDocumentCount(t *testing.T, ctx context.Context, collection backends.Collection, want int) {
	t.Helper()
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
		t.Fatalf("document count = %d, want %d", count, want)
	}
}
