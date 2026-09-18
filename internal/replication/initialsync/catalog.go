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
	"fmt"
	"sort"
	"strings"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/types"
)

// Collection describes source catalog state needed to preflight and clone one catalog entry.
type Collection struct {
	Name       string
	Type       string
	SourceUUID string
	UUIDBinary types.Binary
	Options    *types.Document
	Indexes    []*types.Document
}

// Database describes the catalog entries reported for one source database.
type Database struct {
	Name        string
	Collections []Collection
	Special     bool
}

// DiscoverCatalog enumerates every non-local source database and catalog entry.
func DiscoverCatalog(ctx context.Context, client requestClient) ([]Database, error) {
	databaseNames, err := listDatabaseNames(ctx, client)
	if err != nil {
		return nil, err
	}
	databases := make([]Database, 0, len(databaseNames))
	for _, databaseName := range databaseNames {
		if databaseName == "local" {
			continue
		}
		collections, err := listCollections(ctx, client, databaseName)
		if err != nil {
			return nil, fmt.Errorf("listing %s collections: %w", databaseName, err)
		}
		for index := range collections {
			if collections[index].SourceUUID == "" {
				continue
			}
			indexes, err := listIndexes(ctx, client, databaseName, collections[index].UUIDBinary)
			if err != nil {
				return nil, fmt.Errorf("listing %s.%s indexes: %w", databaseName, collections[index].Name, err)
			}
			collections[index].Indexes = indexes
		}
		databases = append(databases, Database{
			Name: databaseName, Collections: collections, Special: databaseName == "admin" || databaseName == "config",
		})
	}
	return databases, nil
}

func listDatabaseNames(ctx context.Context, client requestClient) ([]string, error) {
	filter := wirebson.MakeDocument(0)
	readPreference := secondaryPreferred()
	document, err := requestDocument(ctx, client, wire.MustOpMsg(
		"listDatabases", int32(1), "filter", filter, "nameOnly", int32(1),
		"$readPreference", readPreference, "$db", "admin",
	))
	if err != nil {
		return nil, err
	}
	value, _ := document.Get("databases")
	array, ok := value.(*types.Array)
	if !ok {
		return nil, fmt.Errorf("listDatabases databases has type %T, want array", value)
	}
	names := make([]string, 0, array.Len())
	for index := 0; index < array.Len(); index++ {
		value, _ := array.Get(index)
		database, ok := value.(*types.Document)
		if !ok {
			return nil, fmt.Errorf("listDatabases entry has type %T, want document", value)
		}
		nameValue, _ := database.Get("name")
		name, ok := nameValue.(string)
		if !ok || name == "" {
			return nil, fmt.Errorf("listDatabases name has type %T, want non-empty string", nameValue)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func listCollections(ctx context.Context, client requestClient, database string) ([]Collection, error) {
	cursor := wirebson.MakeDocument(0)
	documents, err := readCommandCursor(ctx, client, wire.MustOpMsg(
		"listCollections", int32(1), "cursor", cursor,
		"$readPreference", secondaryPreferred(), "$db", database,
	), database)
	if err != nil {
		return nil, err
	}
	collections := make([]Collection, 0, len(documents))
	for _, document := range documents {
		nameValue, _ := document.Get("name")
		name, ok := nameValue.(string)
		if !ok || name == "" {
			return nil, fmt.Errorf("listCollections name has type %T, want non-empty string", nameValue)
		}
		typeValue, typeErr := document.Get("type")
		collectionType, ok := typeValue.(string)
		if typeErr != nil {
			collectionType = "collection"
		} else if !ok || collectionType == "" {
			return nil, fmt.Errorf("listCollections type has type %T, want non-empty string", typeValue)
		}
		switch collectionType {
		case "collection", "view", "timeseries":
		default:
			return nil, fmt.Errorf("listCollections %s.%s has unsupported type %q", database, name, collectionType)
		}
		optionsValue, _ := document.Get("options")
		options, ok := optionsValue.(*types.Document)
		if !ok {
			return nil, fmt.Errorf("listCollections options has type %T, want document", optionsValue)
		}
		infoValue, _ := document.Get("info")
		info, ok := infoValue.(*types.Document)
		if !ok {
			return nil, fmt.Errorf("listCollections info has type %T, want document", infoValue)
		}
		collection := Collection{Name: name, Type: collectionType, Options: options}
		if collectionType == "collection" {
			uuidValue, _ := info.Get("uuid")
			uuidBinary, ok := uuidValue.(types.Binary)
			if !ok || uuidBinary.Subtype != types.BinaryUUID || len(uuidBinary.B) != 16 {
				return nil, fmt.Errorf("listCollections UUID has type %T, want UUID binary", uuidValue)
			}
			parsed, err := uuid.FromBytes(uuidBinary.B)
			if err != nil {
				return nil, err
			}
			collection.SourceUUID = parsed.String()
			collection.UUIDBinary = uuidBinary
		}
		collections = append(collections, collection)
	}
	sort.Slice(collections, func(i, j int) bool { return collections[i].Name < collections[j].Name })
	return collections, nil
}

func listIndexes(ctx context.Context, client requestClient, database string, sourceUUID types.Binary) ([]*types.Document, error) {
	uuidBinary := wirebson.Binary{Subtype: wirebson.BinarySubtype(sourceUUID.Subtype), B: sourceUUID.B}
	cursor := wirebson.MakeDocument(0)
	return readCommandCursor(ctx, client, wire.MustOpMsg(
		"listIndexes", uuidBinary, "cursor", cursor, "includeBuildUUIDs", true,
		"$readPreference", secondaryPreferred(), "$db", database,
	), database)
}

func readCommandCursor(ctx context.Context, client requestClient, request *wire.OpMsg, database string) ([]*types.Document, error) {
	response, err := client.Request(ctx, request)
	if err != nil {
		return nil, err
	}
	var documents []*types.Document
	for {
		document, err := responseDocument(response)
		if err != nil {
			return nil, err
		}
		cursorValue, _ := document.Get("cursor")
		cursor, ok := cursorValue.(*types.Document)
		if !ok {
			return nil, fmt.Errorf("command response cursor has type %T, want document", cursorValue)
		}
		batchValue, batchErr := cursor.Get("firstBatch")
		if batchErr != nil {
			batchValue, _ = cursor.Get("nextBatch")
		}
		batch, ok := batchValue.(*types.Array)
		if !ok {
			return nil, fmt.Errorf("command cursor batch has type %T, want array", batchValue)
		}
		for index := 0; index < batch.Len(); index++ {
			value, _ := batch.Get(index)
			entry, ok := value.(*types.Document)
			if !ok {
				return nil, fmt.Errorf("command cursor entry has type %T, want document", value)
			}
			documents = append(documents, entry)
		}
		idValue, _ := cursor.Get("id")
		cursorID, err := integer(idValue)
		if err != nil {
			return nil, err
		}
		if cursorID == 0 {
			return documents, nil
		}
		namespaceValue, _ := cursor.Get("ns")
		namespace, ok := namespaceValue.(string)
		if !ok {
			return nil, fmt.Errorf("command cursor namespace has type %T, want string", namespaceValue)
		}
		dot := strings.IndexByte(namespace, '.')
		if dot <= 0 || dot == len(namespace)-1 {
			return nil, fmt.Errorf("command cursor has invalid namespace %q", namespace)
		}
		response, err = client.Request(ctx, cloneGetMoreRequest(cursorID, namespace[dot+1:], database))
		if err != nil {
			return nil, err
		}
	}
}

func secondaryPreferred() *wirebson.Document {
	preference := wirebson.MakeDocument(1)
	_ = preference.Add("mode", "secondaryPreferred")
	return preference
}
