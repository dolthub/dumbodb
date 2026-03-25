// Copyright 2024 Dolt Inc.
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
	"slices"
	"strings"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/google/uuid"

	"github.com/dolthub/dongo/internal/backends"
)

// database implements backends.Database.
type database struct {
	backend *Backend
	name    string
}

// Collection implements backends.Database.
func (db *database) Collection(name string) (backends.Collection, error) {
	return backends.CollectionContract(&collection{
		db:   db,
		name: name,
	}), nil
}

// ListCollections implements backends.Database.
func (db *database) ListCollections(ctx context.Context, params *backends.ListCollectionsParams) (*backends.ListCollectionsResult, error) {
	state, err := db.backend.getOrOpenDB(ctx, db.name, false)
	if err != nil {
		return nil, err
	}

	if state == nil {
		// Database doesn't exist yet; return empty list.
		return &backends.ListCollectionsResult{}, nil
	}

	state.mu.RLock()
	defer state.mu.RUnlock()

	var colls []backends.CollectionInfo

	if err := state.am.IterAll(ctx, func(name string, _ hash.Hash) error {
		if params != nil && params.Name != "" && name != params.Name {
			return nil
		}

		collUUID := state.uuids[name]
		colls = append(colls, backends.CollectionInfo{Name: name, UUID: collUUID})
		return nil
	}); err != nil {
		return nil, err
	}

	slices.SortFunc(colls, func(a, b backends.CollectionInfo) int {
		return strings.Compare(a.Name, b.Name)
	})

	return &backends.ListCollectionsResult{Collections: colls}, nil
}

// CreateCollection implements backends.Database.
func (db *database) CreateCollection(ctx context.Context, params *backends.CreateCollectionParams) error {
	state, err := db.backend.getOrOpenDB(ctx, db.name, true)
	if err != nil {
		return err
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	// Check if already exists.
	exists, err := state.am.Has(ctx, params.Name)
	if err != nil {
		return err
	}

	if exists {
		return backends.NewError(backends.ErrorCodeCollectionAlreadyExists,
			fmt.Errorf("dolt: collection %q already exists in %q", params.Name, db.name))
	}

	// Create an empty prolly map for this collection.
	emptyMap, err := newEmptyMap(ctx, state.ns)
	if err != nil {
		return err
	}

	dtblHash, err := state.dtblHashForMap(ctx, emptyMap)
	if err != nil {
		return err
	}
	if err := state.updateAddressMap(ctx, func(ed prolly.AddressMapEditor) error {
		return ed.Add(ctx, params.Name, dtblHash)
	}); err != nil {
		return err
	}

	// Generate and store a UUID for this collection.
	collUUID := uuid.New()
	state.uuids[params.Name] = collUUID.String()

	return nil
}

// DropCollection implements backends.Database.
func (db *database) DropCollection(ctx context.Context, params *backends.DropCollectionParams) error {
	state, err := db.backend.getOrOpenDB(ctx, db.name, false)
	if err != nil {
		return err
	}

	if state == nil {
		return backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("dolt: database %q does not exist", db.name))
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	exists, err := state.am.Has(ctx, params.Name)
	if err != nil {
		return err
	}

	if !exists {
		return backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("dolt: collection %q does not exist in %q", params.Name, db.name))
	}

	if err := state.updateAddressMap(ctx, func(ed prolly.AddressMapEditor) error {
		return ed.Delete(ctx, params.Name)
	}); err != nil {
		return err
	}

	delete(state.uuids, params.Name)

	return nil
}

// RenameCollection implements backends.Database.
func (db *database) RenameCollection(ctx context.Context, params *backends.RenameCollectionParams) error {
	state, err := db.backend.getOrOpenDB(ctx, db.name, false)
	if err != nil {
		return err
	}

	if state == nil {
		return backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("dolt: database %q does not exist", db.name))
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	oldAddr, err := state.am.Get(ctx, params.OldName)
	if err != nil {
		return err
	}

	if oldAddr.IsEmpty() {
		return backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("dolt: collection %q does not exist in %q", params.OldName, db.name))
	}

	newExists, err := state.am.Has(ctx, params.NewName)
	if err != nil {
		return err
	}

	if newExists {
		return backends.NewError(backends.ErrorCodeCollectionAlreadyExists,
			fmt.Errorf("dolt: collection %q already exists in %q", params.NewName, db.name))
	}

	if err := state.updateAddressMap(ctx, func(ed prolly.AddressMapEditor) error {
		if err := ed.Delete(ctx, params.OldName); err != nil {
			return err
		}

		return ed.Add(ctx, params.NewName, oldAddr)
	}); err != nil {
		return err
	}

	// Transfer UUID from old name to new name.
	if collUUID, ok := state.uuids[params.OldName]; ok {
		state.uuids[params.NewName] = collUUID
		delete(state.uuids, params.OldName)
	}

	return nil
}

// Stats implements backends.Database.
func (db *database) Stats(ctx context.Context, params *backends.DatabaseStatsParams) (*backends.DatabaseStatsResult, error) {
	state, err := db.backend.getOrOpenDB(ctx, db.name, false)
	if err != nil {
		return nil, err
	}

	if state == nil {
		return &backends.DatabaseStatsResult{}, nil
	}

	state.mu.RLock()
	defer state.mu.RUnlock()

	const (
		avgDocSize      = 512 // rough bytes per document estimate
		avgIndexEntSize = 32  // rough bytes per index entry estimate
	)

	var totalDocs, totalSizeCollection, totalSizeIndexes int64

	if err := state.am.IterAll(ctx, func(_ string, collHash hash.Hash) error {
		if collHash.IsEmpty() {
			return nil
		}

		m, err := openCollection(ctx, state.cs, state.ns, collHash)
		if err != nil {
			return err
		}

		count, err := m.Count()
		if err != nil {
			return err
		}

		totalDocs += int64(count)
		totalSizeCollection += int64(count) * avgDocSize
		totalSizeIndexes += int64(count) * avgIndexEntSize
		return nil
	}); err != nil {
		return nil, err
	}

	// Preserve a non-zero SizeTotal when the database has collections but no documents,
	// so that listDatabases reports empty=false.
	if totalSizeCollection == 0 {
		collCount, _ := state.am.Count()
		if collCount > 0 {
			totalSizeCollection = int64(collCount) * 4096
		}
	}

	return &backends.DatabaseStatsResult{
		CountDocuments:  totalDocs,
		SizeCollections: totalSizeCollection,
		SizeIndexes:     totalSizeIndexes,
		SizeTotal:       totalSizeCollection + totalSizeIndexes,
	}, nil
}
