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
	"slices"
	"strings"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
)

// database implements backends.Database.
type database struct {
	backend *Backend
	name    string // base database name (actual directory name, without rootish suffix)
	rootish string // rootish from encoded name (branch, commit hash, tag, or ancestor expression)
}

// resolveAM returns the collections AddressMap for the database's rootish.
//
// For "main", the current working-set AM (state.am) is returned. For read-only
// rootishes (commit hashes, ancestor expressions), the AM is loaded from the
// historical RTVL at that revision. For writable branch rootishes, the in-memory
// working-set AM is returned if it exists, otherwise the branch HEAD is loaded.
//
// The caller must hold at least state.mu.RLock().
func (db *database) resolveAM(ctx context.Context, state *dbState) (prolly.AddressMap, error) {
	if db.rootish == "main" {
		return state.am, nil
	}
	if rootishIsReadOnly(db.rootish) {
		return amFromRootish(ctx, state, db.rootish)
	}
	// Writable branch: prefer working-set AM if already initialized.
	if am, ok := state.branchAMs[db.rootish]; ok {
		return am, nil
	}
	return amFromRootish(ctx, state, db.rootish)
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

	am, err := db.resolveAM(ctx, state)
	if err != nil {
		return nil, err
	}

	var colls []backends.CollectionInfo

	// Include regular collections from the address map.
	if err := am.IterAll(ctx, func(name string, _ hash.Hash) error {
		if params != nil && params.Name != "" && name != params.Name {
			return nil
		}

		collUUID := state.uuids[name]
		ci := backends.CollectionInfo{Name: name, UUID: collUUID}
		if v, ok := state.validators[name]; ok {
			ci.Validator = v.Validator
			ci.ValidationLevel = v.ValidationLevel
			ci.ValidationAction = v.ValidationAction
		}
		if c, ok := state.capped[name]; ok {
			ci.CappedSize = c.CappedSize
			ci.CappedDocuments = c.CappedDocuments
		}
		colls = append(colls, ci)
		return nil
	}); err != nil {
		return nil, err
	}

	// Include views.
	for name, v := range state.views {
		if params != nil && params.Name != "" && name != params.Name {
			continue
		}
		collUUID := state.uuids[name]
		ci := backends.CollectionInfo{
			Name:         name,
			UUID:         collUUID,
			IsView:       true,
			ViewOn:       v.ViewOn,
			ViewPipeline: v.Pipeline,
		}
		colls = append(colls, ci)
	}

	// Include time series collections (already in the address map, but add TS metadata).
	for name, ts := range state.timeSeries {
		// Find and update the existing entry in colls (it's already there from the address map iteration).
		for i, c := range colls {
			if c.Name == name {
				colls[i].IsTimeSeries = true
				colls[i].TimeField = ts.TimeField
				colls[i].MetaField = ts.MetaField
				colls[i].Granularity = ts.Granularity
				break
			}
		}
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

	// Check if already exists as a view.
	if _, isView := state.views[params.Name]; isView {
		return backends.NewError(backends.ErrorCodeCollectionAlreadyExists,
			fmt.Errorf("dolt: collection %q already exists in %q", params.Name, db.name))
	}

	// Check if already exists as a regular collection.
	branchAM, err := state.getOrInitBranchAM(ctx, db.rootish)
	if err != nil {
		return err
	}
	exists, err := branchAM.Has(ctx, params.Name)
	if err != nil {
		return err
	}

	if exists {
		return backends.NewError(backends.ErrorCodeCollectionAlreadyExists,
			fmt.Errorf("dolt: collection %q already exists in %q", params.Name, db.name))
	}

	// If viewOn is set, this is a view  -- store metadata only, no prolly map.
	if params.ViewOn != "" {
		state.views[params.Name] = &viewMeta{
			ViewOn:   params.ViewOn,
			Pipeline: params.ViewPipeline,
		}
		// Generate a UUID for the view.
		collUUID := uuid.New()
		state.uuids[params.Name] = collUUID.String()
		return nil
	}

	// Create an empty prolly map for this collection.
	emptyMap, err := newEmptyMap(ctx, state.ns)
	if err != nil {
		return err
	}

	dtblHash, err := state.dtblHashForCollection(ctx, params.Name, emptyMap, hash.Hash{})
	if err != nil {
		return err
	}
	if err := state.updateAddressMap(ctx, db.rootish, func(ed prolly.AddressMapEditor) error {
		return ed.Add(ctx, params.Name, dtblHash)
	}); err != nil {
		return err
	}

	// Generate and store a UUID for this collection.
	collUUID := uuid.New()
	state.uuids[params.Name] = collUUID.String()

	// Store capped configuration if provided.
	if params.CappedSize > 0 {
		state.capped[params.Name] = &cappedCollectionMeta{
			CappedSize:      params.CappedSize,
			CappedDocuments: params.CappedDocuments,
		}
	}

	// Store validator if provided.
	if params.Validator != nil || params.ValidationLevel != "" || params.ValidationAction != "" {
		state.validators[params.Name] = &collectionValidator{
			Validator:        params.Validator,
			ValidationLevel:  params.ValidationLevel,
			ValidationAction: params.ValidationAction,
		}
	}

	// Store time series metadata if provided.
	if params.IsTimeSeries {
		state.timeSeries[params.Name] = &timeSeriesMeta{
			TimeField:   params.TimeField,
			MetaField:   params.MetaField,
			Granularity: params.Granularity,
		}
	}

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

	// Check if it's a view.
	if _, isView := state.views[params.Name]; isView {
		delete(state.views, params.Name)
		delete(state.uuids, params.Name)
		return nil
	}

	dropBranchAM, err := state.getOrInitBranchAM(ctx, db.rootish)
	if err != nil {
		return err
	}
	exists, err := dropBranchAM.Has(ctx, params.Name)
	if err != nil {
		return err
	}

	if !exists {
		return backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("dolt: collection %q does not exist in %q", params.Name, db.name))
	}

	if err := state.updateAddressMap(ctx, db.rootish, func(ed prolly.AddressMapEditor) error {
		return ed.Delete(ctx, params.Name)
	}); err != nil {
		return err
	}

	delete(state.uuids, params.Name)
	delete(state.validators, params.Name)
	delete(state.capped, params.Name)
	delete(state.insertionOrder, params.Name)
	delete(state.timeSeries, params.Name)

	return nil
}

// RenameCollection implements backends.Database.
func (db *database) RenameCollection(ctx context.Context, params *backends.RenameCollectionParams) error {
	state, err := db.backend.getOrOpenDB(ctx, db.name, false)
	if err != nil {
		return err
	}

	if state == nil {
		return backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: database %q does not exist", db.name))
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	renameBranchAM, err := state.getOrInitBranchAM(ctx, db.rootish)
	if err != nil {
		return err
	}

	oldAddr, err := renameBranchAM.Get(ctx, params.OldName)
	if err != nil {
		return err
	}

	if oldAddr.IsEmpty() {
		return backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("dolt: collection %q does not exist in %q", params.OldName, db.name))
	}

	newExists, err := renameBranchAM.Has(ctx, params.NewName)
	if err != nil {
		return err
	}

	if newExists {
		return backends.NewError(backends.ErrorCodeCollectionAlreadyExists,
			fmt.Errorf("dolt: collection %q already exists in %q", params.NewName, db.name))
	}

	if err := state.updateAddressMap(ctx, db.rootish, func(ed prolly.AddressMapEditor) error {
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

	// Transfer validator from old name to new name.
	if v, ok := state.validators[params.OldName]; ok {
		state.validators[params.NewName] = v
		delete(state.validators, params.OldName)
	}

	// Transfer capped metadata from old name to new name.
	if c, ok := state.capped[params.OldName]; ok {
		state.capped[params.NewName] = c
		delete(state.capped, params.OldName)
	}

	// Transfer insertion order from old name to new name.
	if ord, ok := state.insertionOrder[params.OldName]; ok {
		state.insertionOrder[params.NewName] = ord
		delete(state.insertionOrder, params.OldName)
	}

	return nil
}

// CollMod implements backends.Database.
func (db *database) CollMod(ctx context.Context, params *backends.CollModParams) error {
	state, err := db.backend.getOrOpenDB(ctx, db.name, false)
	if err != nil {
		return err
	}

	if state == nil {
		return backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: database %q does not exist", db.name))
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	collModBranchAM, err := state.getOrInitBranchAM(ctx, db.rootish)
	if err != nil {
		return err
	}
	exists, err := collModBranchAM.Has(ctx, params.Name)
	if err != nil {
		return err
	}

	if !exists {
		return backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("dolt: collection %q does not exist in %q", params.Name, db.name))
	}

	// Get or create validator entry.
	v, ok := state.validators[params.Name]
	if !ok {
		v = &collectionValidator{}
	}

	if params.SetValidator {
		v.Validator = params.Validator
	}
	if params.ValidationLevel != "" {
		v.ValidationLevel = params.ValidationLevel
	}
	if params.ValidationAction != "" {
		v.ValidationAction = params.ValidationAction
	}

	// Store (or clear if all fields are empty).
	if v.Validator == nil && v.ValidationLevel == "" && v.ValidationAction == "" {
		delete(state.validators, params.Name)
	} else {
		state.validators[params.Name] = v
	}

	// Update capped metadata if CappedSize is specified.
	if params.CappedSize > 0 {
		state.capped[params.Name] = &cappedCollectionMeta{
			CappedSize: params.CappedSize,
		}
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

	am, err := db.resolveAM(ctx, state)
	if err != nil {
		return nil, err
	}

	const (
		avgDocSize      = 512 // rough bytes per document estimate
		avgIndexEntSize = 32  // rough bytes per index entry estimate
	)

	var totalDocs, totalSizeCollection, totalSizeIndexes int64

	if err := am.IterAll(ctx, func(_ string, collHash hash.Hash) error {
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
		collCount, _ := am.Count()
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
