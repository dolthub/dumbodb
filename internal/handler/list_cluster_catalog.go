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

package handler

import (
	"context"

	"github.com/FerretDB/wire"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// aggregateListClusterCatalog implements the $listClusterCatalog source stage
// as a standalone mongod does: one entry per collection and view of dbName, or
// of every database on admin, none of them sharded.
func (h *Handler) aggregateListClusterCatalog(connCtx context.Context, document, firstStage *types.Document, pipeline *types.Array, dbName string) (*wire.OpMsg, error) {
	spec, err := stages.ParseListClusterCatalogSpec(firstStage)
	if err != nil {
		return nil, err
	}
	entries, err := h.listClusterCatalogEntries(connCtx, spec, dbName)
	if err != nil {
		return nil, err
	}
	return h.aggregateSourceDocuments(connCtx, document, entries, pipeline, dbName)
}

// listClusterCatalogEntries returns the documents $listClusterCatalog with
// spec lists on dbName.
func (h *Handler) listClusterCatalogEntries(connCtx context.Context, spec *stages.ListClusterCatalogSpec, dbName string) ([]*types.Document, error) {
	dbNames := []string{dbName}
	if dbName == "admin" {
		res, err := h.b.ListDatabases(connCtx, nil)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
		dbNames = dbNames[:0]
		for _, info := range res.Databases {
			dbNames = append(dbNames, info.Name)
		}
	}

	var entries []*types.Document
	for _, name := range dbNames {
		db, err := h.b.Database(name)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
		res, err := db.ListCollections(connCtx, nil)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
		// mongod stores the definitions of views and time-series collections in
		// system.views.
		var hasViews bool
		for _, collection := range res.Collections {
			hasViews = hasViews || collection.IsView || collection.IsTimeSeries
			listed, err := collectionCatalogEntry(&collection)
			if err != nil {
				return nil, err
			}
			entry := types.MakeDocument(8)
			entry.Set("ns", name+"."+collection.Name)
			entry.Set("db", name)
			for _, key := range listed.Keys() {
				if key != "name" {
					entry.Set(key, listed.Remove(key))
				}
			}
			entry.Set("sharded", false)
			if spec.Shards {
				entry.Set("shards", types.MakeArray(0))
			}
			entries = append(entries, entry)
		}
		if hasViews {
			entries = append(entries, systemViewsCatalogEntry(name, spec.Shards))
		}
	}
	return entries, nil
}

// systemViewsCatalogEntry is the entry mongod lists for the system.views
// collection that holds a database's view definitions. DumboDB keeps views in
// its catalog instead; its uuid is derived from the namespace.
func systemViewsCatalogEntry(dbName string, shards bool) *types.Document {
	ns := dbName + ".system.views"
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(ns))
	entry := must.NotFail(types.NewDocument(
		"ns", ns,
		"db", dbName,
		"type", "collection",
		"options", must.NotFail(types.NewDocument()),
		"info", must.NotFail(types.NewDocument(
			"readOnly", false,
			"uuid", types.Binary{Subtype: types.BinaryUUID, B: id[:]},
		)),
		"idIndex", must.NotFail(types.NewDocument(
			"v", int32(2),
			"key", must.NotFail(types.NewDocument("_id", int32(1))),
			"name", "_id_",
		)),
		"sharded", false,
	))
	if shards {
		entry.Set("shards", types.MakeArray(0))
	}
	return entry
}
