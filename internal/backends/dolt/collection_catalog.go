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

package dolt

import (
	"context"
	"fmt"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// reservedCatalogName is the internal collection that durably stores
// per-collection metadata (one document per user collection, keyed by the
// collection name). Because it is an ordinary per-branch collection, metadata is
// durable, branch-scoped, and participates in commit/branch/merge for free. It
// is hidden from listCollections and the version-control diff/status walks, and
// rejected as a user collection name (see backends.validateCollectionName).
const reservedCatalogName = backends.ReservedCatalogName

// collMeta is the persisted per-collection metadata document. Capped
// configuration is deliberately absent: capped collections are rejected at the
// API, so no capped collection can exist.
type collMeta struct {
	UUID             string
	Validator        *types.Document
	ValidationLevel  string
	ValidationAction string
	IsTimeSeries     bool
	TimeField        string
	MetaField        string
	Granularity      string
}

func collMetaToDoc(collName string, m *collMeta) (*types.Document, error) {
	var validator any = types.Null
	if m.Validator != nil {
		validator = m.Validator
	}
	return types.NewDocument(
		"_id", collName,
		"uuid", m.UUID,
		"validator", validator,
		"validationLevel", m.ValidationLevel,
		"validationAction", m.ValidationAction,
		"isTimeSeries", m.IsTimeSeries,
		"timeField", m.TimeField,
		"metaField", m.MetaField,
		"granularity", m.Granularity,
	)
}

func docToCollMeta(doc *types.Document) *collMeta {
	m := &collMeta{}
	getStr := func(k string) string {
		v, err := doc.Get(k)
		if err != nil {
			return ""
		}
		s, _ := v.(string)
		return s
	}
	m.UUID = getStr("uuid")
	if v, err := doc.Get("validator"); err == nil {
		if vd, ok := v.(*types.Document); ok {
			m.Validator = vd
		}
	}
	m.ValidationLevel = getStr("validationLevel")
	m.ValidationAction = getStr("validationAction")
	if v, err := doc.Get("isTimeSeries"); err == nil {
		m.IsTimeSeries, _ = v.(bool)
	}
	m.TimeField = getStr("timeField")
	m.MetaField = getStr("metaField")
	m.Granularity = getStr("granularity")
	return m
}

// catalogKey is the document-map key for a collection's metadata entry.
func catalogKey(collName string) (val.Tuple, error) {
	h, err := hashID(collName)
	if err != nil {
		return nil, fmt.Errorf("hashing catalog key %q: %w", collName, err)
	}
	return buildKey(h[:])
}

// catalogMapFromAM opens the catalog collection's document map from a
// collections AddressMap, or an empty map if the catalog does not exist yet.
func catalogMapFromAM(ctx context.Context, state *dbState, am prolly.AddressMap) (prolly.Map, error) {
	h, err := am.Get(ctx, reservedCatalogName)
	if err != nil {
		return prolly.Map{}, err
	}
	if h.IsEmpty() {
		return newEmptyMap(ctx, state.ns)
	}
	return openCollection(ctx, state.cs, state.ns, h)
}

// writeCatalogMap rebuilds the catalog DTBL from m and stores it in the branch's
// collections AddressMap. The caller holds state.mu (write lock).
func (state *dbState) writeCatalogMap(ctx context.Context, rootish string, m prolly.Map) error {
	emptyIdx, err := emptyIndexAM(state.ns)
	if err != nil {
		return err
	}
	dtblHash, err := state.dtblHashForCollection(ctx, reservedCatalogName, m, emptyIdx, hash.Hash{})
	if err != nil {
		return err
	}
	am, err := state.getOrInitBranchAM(ctx, rootish)
	if err != nil {
		return err
	}
	exists, err := am.Has(ctx, reservedCatalogName)
	if err != nil {
		return err
	}
	return state.updateAddressMap(ctx, rootish, "auto: update collection catalog", func(ed prolly.AddressMapEditor) error {
		if exists {
			return ed.Update(ctx, reservedCatalogName, dtblHash)
		}
		return ed.Add(ctx, reservedCatalogName, dtblHash)
	})
}

// upsertCatalogDoc writes (or replaces) the metadata document for collName. The
// caller holds state.mu (write lock).
func (state *dbState) upsertCatalogDoc(ctx context.Context, rootish, collName string, meta *collMeta) error {
	am, err := state.getOrInitBranchAM(ctx, rootish)
	if err != nil {
		return err
	}
	catMap, err := catalogMapFromAM(ctx, state, am)
	if err != nil {
		return err
	}
	key, err := catalogKey(collName)
	if err != nil {
		return err
	}
	doc, err := collMetaToDoc(collName, meta)
	if err != nil {
		return err
	}
	value, err := writeBSONDocToValue(ctx, state.ns, doc)
	if err != nil {
		return err
	}
	mut := catMap.Mutate()
	if err := mut.Put(ctx, key, value); err != nil {
		return err
	}
	newMap, err := mut.Map(ctx)
	if err != nil {
		return err
	}
	return state.writeCatalogMap(ctx, rootish, newMap)
}

// deleteCatalogDoc removes the metadata document for collName (a no-op if the
// catalog or entry is absent). The caller holds state.mu (write lock).
func (state *dbState) deleteCatalogDoc(ctx context.Context, rootish, collName string) error {
	am, err := state.getOrInitBranchAM(ctx, rootish)
	if err != nil {
		return err
	}
	if h, err := am.Get(ctx, reservedCatalogName); err != nil || h.IsEmpty() {
		return err
	}
	catMap, err := catalogMapFromAM(ctx, state, am)
	if err != nil {
		return err
	}
	key, err := catalogKey(collName)
	if err != nil {
		return err
	}
	mut := catMap.Mutate()
	if err := mut.Delete(ctx, key); err != nil {
		return err
	}
	newMap, err := mut.Map(ctx)
	if err != nil {
		return err
	}
	return state.writeCatalogMap(ctx, rootish, newMap)
}

// readCatalogDoc returns the metadata for collName from the catalog in am, or nil
// if there is none.
func readCatalogDoc(ctx context.Context, state *dbState, am prolly.AddressMap, collName string) (*collMeta, error) {
	catMap, err := catalogMapFromAM(ctx, state, am)
	if err != nil {
		return nil, err
	}
	key, err := catalogKey(collName)
	if err != nil {
		return nil, err
	}
	var meta *collMeta
	if err := catMap.Get(ctx, key, func(_, v val.Tuple) error {
		if v == nil {
			return nil
		}
		doc, derr := readBSONDocFromValue(ctx, state.ns, v)
		if derr != nil {
			return derr
		}
		meta = docToCollMeta(doc)
		return nil
	}); err != nil {
		return nil, err
	}
	return meta, nil
}

// listCatalog returns all collection metadata in am, keyed by collection name.
func listCatalog(ctx context.Context, state *dbState, am prolly.AddressMap) (map[string]*collMeta, error) {
	out := map[string]*collMeta{}
	catMap, err := catalogMapFromAM(ctx, state, am)
	if err != nil {
		return nil, err
	}
	iter, err := catMap.IterAll(ctx)
	if err != nil {
		return nil, err
	}
	for {
		_, v, err := iter.Next(ctx)
		if err != nil {
			break
		}
		doc, derr := readBSONDocFromValue(ctx, state.ns, v)
		if derr != nil {
			return nil, derr
		}
		name, _ := doc.Get("_id")
		if s, ok := name.(string); ok {
			out[s] = docToCollMeta(doc)
		}
	}
	return out, nil
}
