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
	"testing"

	"github.com/FerretDB/wire"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestDiscoverCatalogExcludesLocalAndUsesCollectionUUID(t *testing.T) {
	sourceUUID := uuid.MustParse("12345678-1234-4234-9234-123456789abc")
	databases := must.NotFail(types.NewArray(
		must.NotFail(types.NewDocument("name", "local")),
		must.NotFail(types.NewDocument("name", "orders")),
	))
	options := must.NotFail(types.NewDocument(
		"validator", must.NotFail(types.NewDocument("active", must.NotFail(types.NewDocument("$type", "bool")))),
	))
	collectionInfo := must.NotFail(types.NewDocument(
		"name", "items", "type", "collection", "options", options,
		"info", must.NotFail(types.NewDocument("readOnly", false, "uuid", types.Binary{Subtype: types.BinaryUUID, B: sourceUUID[:]})),
	))
	index := must.NotFail(types.NewDocument("v", int32(2), "key", must.NotFail(types.NewDocument("_id", int32(1))), "name", "_id_"))
	client := &boundaryClient{responses: []*wire.OpMsg{
		responseMessage(t, must.NotFail(types.NewDocument("databases", databases, "ok", float64(1)))),
		cursorResponse(t, collectionInfo),
		cursorResponse(t, index),
	}}
	catalog, err := DiscoverCatalog(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog) != 1 || catalog[0].Name != "orders" || catalog[0].Special || len(catalog[0].Collections) != 1 {
		t.Fatalf("catalog = %+v", catalog)
	}
	collection := catalog[0].Collections[0]
	if collection.Name != "items" || collection.SourceUUID != sourceUUID.String() || types.Compare(collection.Options, options) != types.Equal {
		t.Fatalf("collection = %+v", collection)
	}
	if len(collection.Indexes) != 1 || types.Compare(collection.Indexes[0], index) != types.Equal {
		t.Fatalf("indexes = %v", collection.Indexes)
	}
	if len(client.requests) != 3 {
		t.Fatalf("request count = %d", len(client.requests))
	}
	assertCommand(t, client.requests[0], "listDatabases", "admin")
	assertCommand(t, client.requests[1], "listCollections", "orders")
	indexRequest := decodeRequest(t, client.requests[2])
	listIndexes, _ := indexRequest.Get("listIndexes")
	indexUUID, ok := listIndexes.(types.Binary)
	if !ok || indexUUID.Subtype != types.BinaryUUID || string(indexUUID.B) != string(sourceUUID[:]) {
		t.Fatalf("listIndexes UUID = %v", listIndexes)
	}
	if includeBuildUUIDs, _ := indexRequest.Get("includeBuildUUIDs"); includeBuildUUIDs != true {
		t.Fatalf("includeBuildUUIDs = %v", includeBuildUUIDs)
	}
}

func TestDiscoverCatalogMarksAdminAndConfigSpecial(t *testing.T) {
	databases := must.NotFail(types.NewArray(
		must.NotFail(types.NewDocument("name", "config")),
		must.NotFail(types.NewDocument("name", "admin")),
	))
	client := &boundaryClient{responses: []*wire.OpMsg{
		responseMessage(t, must.NotFail(types.NewDocument("databases", databases, "ok", float64(1)))),
		cursorResponse(t),
		cursorResponse(t),
	}}
	catalog, err := DiscoverCatalog(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog) != 2 || catalog[0].Name != "admin" || !catalog[0].Special || catalog[1].Name != "config" || !catalog[1].Special {
		t.Fatalf("special catalog = %+v", catalog)
	}
}
