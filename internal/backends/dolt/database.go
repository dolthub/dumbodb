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

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/sqlctx"
)

// database implements backends.Database.
type database struct {
	backend *Backend
	name    string // base database name (actual directory name, without rootish suffix)
	rootish string // rootish from encoded name (branch, commit hash, tag, or ancestor expression)
}

// isReadOnly reports whether the database's rootish resolves to a read-only
// snapshot (commit hash, ancestor expression, caret, or tag).
func (db *database) isReadOnly(ctx context.Context, state *dbState) bool {
	if rootishIsReadOnly(db.rootish) {
		return true
	}
	if db.rootish == defaultBranch {
		return false
	}
	// Tags look like branch names syntactically but are read-only.
	tagDS, err := state.datasDB.GetDataset(ctx, "refs/tags/"+db.rootish)
	return err == nil && tagDS.HasHead()
}

// resolveAM returns the collections AddressMap for the database's rootish.
//
// For "main", the current working-set AM (state.branchAMs[defaultBranch]) is returned. For read-only
// rootishes (commit hashes, ancestor expressions), the AM is loaded from the
// historical RTVL at that revision. For writable branch rootishes, the in-memory
// working-set AM is returned if it exists, otherwise the branch HEAD is loaded.
//
// The caller must hold at least state.mu.RLock().
func (db *database) resolveAM(ctx context.Context, state *dbState) (prolly.AddressMap, error) {
	if db.rootish == defaultBranch {
		if ws, ok := txnVisibleWS(ctx, state, defaultBranch); ok {
			return amFromWorkingRoot(ctx, ws.WorkingRoot(), state.ns)
		}
		ws, err := latestBranchWS(ctx, state, defaultBranch)
		if err != nil {
			return prolly.AddressMap{}, err
		}
		return amFromWorkingRoot(ctx, ws.WorkingRoot(), state.ns)
	}
	if rootishIsReadOnly(db.rootish) {
		return amFromRootish(ctx, state, db.rootish)
	}
	if tagDS, tagErr := state.datasDB.GetDataset(ctx, "refs/tags/"+db.rootish); tagErr == nil && tagDS.HasHead() {
		return amFromRootish(ctx, state, db.rootish)
	}
	if ws, ok := txnVisibleWS(ctx, state, db.rootish); ok {
		return amFromWorkingRoot(ctx, ws.WorkingRoot(), state.ns)
	}
	// Consult the WS via the singleton entry, but use loadBranchWS
	// directly rather than latestBranchWS: the latter falls back to
	// "initialize a WS from HEAD" for branches with no on-disk ref,
	// which is wrong for caret/tilde traversal rootishes ("main^")
	// -- those must fall through to amFromRootish for commit-hash
	// resolution.
	if ws, err := state.loadBranchWS(ctx, db.rootish); err == nil && ws != nil {
		return amFromWorkingRoot(ctx, ws.WorkingRoot(), state.ns)
	}
	return amFromRootish(ctx, state, db.rootish)
}

// latestBranchWS returns the latest committed WS for (state, branch).
// Uncommitted overlays are handled upstream by txnVisibleWS; consulting
// the session's branchState here would hide commits other sessions made
// to disk, because dsess never refreshes branchState after lookup.
func latestBranchWS(ctx context.Context, state *dbState, branch string) (*doltdb.WorkingSet, error) {
	return state.loadCommittedWS(ctx, branch)
}

// txnVisibleWS returns the session's branchState only when DirtyBranch-
// Revisions reports this (db, branch) as dirty; this gives read-your-own-
// writes without pinning to a stale txn-start snapshot for clean reads.
func txnVisibleWS(ctx context.Context, state *dbState, branch string) (*doltdb.WorkingSet, bool) {
	sess := sessionFromContext(ctx)
	if sess == nil {
		return nil, false
	}
	if !dbNameDsessFriendly(state.name) || alwaysAutoCommit(state.name) {
		return nil, false
	}
	qualified := qualifiedDbName(state.name, branch)
	qualifiedLower := strings.ToLower(qualified)
	dirty := false
	for _, d := range sess.DirtyBranchRevisions() {
		if strings.ToLower(d) == qualifiedLower {
			dirty = true
			break
		}
	}
	if !dirty {
		return nil, false
	}
	sqlCtx := sqlctx.Wrap(ctx, sess)
	sessState, ok, err := sess.LookupDbState(sqlCtx, qualified)
	if err != nil || !ok {
		return nil, false
	}
	ws := sessState.WorkingSet()
	if ws == nil {
		return nil, false
	}
	return ws, true
}

func (db *database) Collection(name string) (backends.Collection, error) {
	return backends.CollectionContract(&collection{
		db:   db,
		name: name,
	}), nil
}

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

	// Per-collection metadata (UUID, validator, capped, timeseries) is read from
	// the durable, branch-scoped catalog for this AM.
	catalog, err := listCatalog(ctx, state, am)
	if err != nil {
		return nil, err
	}

	// Each collections-AddressMap entry is either a collection (DTBL chunk) or
	// a view (BlobFileID metadata chunk); classify by chunk type and surface
	// views with their definition. The internal catalog collection is hidden.
	if err := am.IterAll(ctx, func(name string, h hash.Hash) error {
		if name == reservedCatalogName {
			return nil
		}
		if params != nil && params.Name != "" && name != params.Name {
			return nil
		}

		isView, err := isViewEntry(ctx, state.cs, h)
		if err != nil {
			return err
		}
		if isView {
			vm, err := readViewChunk(ctx, state.ns, h)
			if err != nil {
				return err
			}
			colls = append(colls, backends.CollectionInfo{
				Name:         name,
				IsView:       true,
				ViewOn:       vm.ViewOn,
				ViewPipeline: vm.Pipeline,
			})
			return nil
		}

		ci := backends.CollectionInfo{Name: name}
		if m := catalog[name]; m != nil {
			ci.UUID = m.UUID
			ci.Validator = m.Validator
			ci.ValidationLevel = m.ValidationLevel
			ci.ValidationAction = m.ValidationAction
			ci.IsTimeSeries = m.IsTimeSeries
			ci.TimeField = m.TimeField
			ci.MetaField = m.MetaField
			ci.Granularity = m.Granularity
		}
		colls = append(colls, ci)
		return nil
	}); err != nil {
		return nil, err
	}

	slices.SortFunc(colls, func(a, b backends.CollectionInfo) int {
		return strings.Compare(a.Name, b.Name)
	})

	return &backends.ListCollectionsResult{Collections: colls}, nil
}

func (db *database) CreateCollection(ctx context.Context, params *backends.CreateCollectionParams) error {
	state, err := db.backend.getOrOpenDB(ctx, db.name, true)
	if err != nil {
		return err
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	// A view and a collection share the collections AddressMap namespace, so a
	// single existence check below covers a name already taken by either.
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
			fmt.Errorf("collection %q already exists in %q", params.Name, db.name))
	}

	// If viewOn is set, this is a view: persist a self-describing metadata blob
	// under the view name in the collections AddressMap (no prolly map). This is
	// what makes views durable, branch-scoped, and versioned like collections.
	if params.ViewOn != "" {
		viewHash, err := writeViewChunk(ctx, state.ns, &viewMeta{
			ViewOn:   params.ViewOn,
			Pipeline: params.ViewPipeline,
		})
		if err != nil {
			return err
		}
		return state.updateAddressMap(ctx, db.rootish, fmt.Sprintf("auto: create view %s", params.Name), func(ed prolly.AddressMapEditor) error {
			return ed.Add(ctx, params.Name, viewHash)
		})
	}

	// Create an empty prolly map for this collection.
	emptyMap, err := newEmptyMap(ctx, state.ns)
	if err != nil {
		return err
	}

	emptyAM, err := emptyIndexAM(state.ns)
	if err != nil {
		return err
	}
	dtblHash, err := state.dtblHashForCollection(ctx, params.Name, emptyMap, emptyAM, hash.Hash{})
	if err != nil {
		return err
	}
	if err := state.updateAddressMap(ctx, db.rootish, fmt.Sprintf("auto: create collection %s", params.Name), func(ed prolly.AddressMapEditor) error {
		return ed.Add(ctx, params.Name, dtblHash)
	}); err != nil {
		return err
	}

	// Persist the collection's metadata (UUID + validator + capped + timeseries)
	// durably in the branch-scoped catalog. The in-memory maps are kept as a
	// write-through cache for in-session reads.
	collUUID := uuid.New()
	meta := &collMeta{
		UUID:             collUUID.String(),
		Validator:        params.Validator,
		ValidationLevel:  params.ValidationLevel,
		ValidationAction: params.ValidationAction,
		IsTimeSeries:     params.IsTimeSeries,
		TimeField:        params.TimeField,
		MetaField:        params.MetaField,
		Granularity:      params.Granularity,
	}
	if err := state.upsertCatalogDoc(ctx, db.rootish, params.Name, meta); err != nil {
		return fmt.Errorf("persisting collection metadata for %q: %w", params.Name, err)
	}

	state.uuids[params.Name] = collUUID.String()
	if params.CappedSize > 0 {
		state.capped[params.Name] = &cappedCollectionMeta{
			CappedSize:      params.CappedSize,
			CappedDocuments: params.CappedDocuments,
		}
	}
	if params.Validator != nil || params.ValidationLevel != "" || params.ValidationAction != "" {
		state.validators[params.Name] = &collectionValidator{
			Validator:        params.Validator,
			ValidationLevel:  params.ValidationLevel,
			ValidationAction: params.ValidationAction,
		}
	}
	if params.IsTimeSeries {
		state.timeSeries[params.Name] = &timeSeriesMeta{
			TimeField:   params.TimeField,
			MetaField:   params.MetaField,
			Granularity: params.Granularity,
		}
	}

	return nil
}

func (db *database) DropCollection(ctx context.Context, params *backends.DropCollectionParams) error {
	state, err := db.backend.getOrOpenDB(ctx, db.name, false)
	if err != nil {
		return err
	}

	if state == nil {
		return backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("database %q does not exist", db.name))
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	// A view lives in the collections AddressMap like a collection, so the
	// generic existence check and AddressMap delete below drop it uniformly.
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
			fmt.Errorf("collection %q does not exist in %q", params.Name, db.name))
	}

	if err := state.updateAddressMap(ctx, db.rootish, fmt.Sprintf("auto: drop collection %s", params.Name), func(ed prolly.AddressMapEditor) error {
		return ed.Delete(ctx, params.Name)
	}); err != nil {
		return err
	}

	if err := state.deleteCatalogDoc(ctx, db.rootish, params.Name); err != nil {
		return fmt.Errorf("removing collection metadata for %q: %w", params.Name, err)
	}

	delete(state.uuids, params.Name)
	delete(state.validators, params.Name)
	delete(state.capped, params.Name)
	delete(state.insertionOrder, params.Name)
	delete(state.timeSeries, params.Name)

	return nil
}

func (db *database) RenameCollection(ctx context.Context, params *backends.RenameCollectionParams) error {
	state, err := db.backend.getOrOpenDB(ctx, db.name, false)
	if err != nil {
		return err
	}

	if state == nil {
		return backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("database %q does not exist", db.name))
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
			fmt.Errorf("collection %q does not exist in %q", params.OldName, db.name))
	}

	newExists, err := renameBranchAM.Has(ctx, params.NewName)
	if err != nil {
		return err
	}

	if newExists {
		return backends.NewError(backends.ErrorCodeCollectionAlreadyExists,
			fmt.Errorf("collection %q already exists in %q", params.NewName, db.name))
	}

	if err := state.updateAddressMap(ctx, db.rootish, fmt.Sprintf("auto: rename collection %s to %s", params.OldName, params.NewName), func(ed prolly.AddressMapEditor) error {
		if err := ed.Delete(ctx, params.OldName); err != nil {
			return err
		}

		return ed.Add(ctx, params.NewName, oldAddr)
	}); err != nil {
		return err
	}

	// Move the metadata document with the collection, preserving its UUID.
	renameAM, err := state.getOrInitBranchAM(ctx, db.rootish)
	if err != nil {
		return err
	}
	if meta, mErr := readCatalogDoc(ctx, state, renameAM, params.OldName); mErr != nil {
		return fmt.Errorf("reading collection metadata for %q: %w", params.OldName, mErr)
	} else if meta != nil {
		if err := state.upsertCatalogDoc(ctx, db.rootish, params.NewName, meta); err != nil {
			return fmt.Errorf("moving collection metadata to %q: %w", params.NewName, err)
		}
		if err := state.deleteCatalogDoc(ctx, db.rootish, params.OldName); err != nil {
			return fmt.Errorf("removing old collection metadata for %q: %w", params.OldName, err)
		}
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

func (db *database) CollMod(ctx context.Context, params *backends.CollModParams) error {
	state, err := db.backend.getOrOpenDB(ctx, db.name, false)
	if err != nil {
		return err
	}

	if state == nil {
		return backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("database %q does not exist", db.name))
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
			fmt.Errorf("collection %q does not exist in %q", params.Name, db.name))
	}

	// Redefining a view: rewrite its metadata blob in place. Only applies when
	// the target is actually a view; view fields on a real collection are
	// ignored (as they were before views became first-class).
	if params.SetView {
		entryHash, err := collModBranchAM.Get(ctx, params.Name)
		if err != nil {
			return err
		}
		isView, err := isViewEntry(ctx, state.cs, entryHash)
		if err != nil {
			return err
		}
		if isView {
			viewHash, err := writeViewChunk(ctx, state.ns, &viewMeta{
				ViewOn:   params.ViewOn,
				Pipeline: params.ViewPipeline,
			})
			if err != nil {
				return err
			}
			return state.updateAddressMap(ctx, db.rootish, fmt.Sprintf("auto: collMod view %s", params.Name), func(ed prolly.AddressMapEditor) error {
				return ed.Update(ctx, params.Name, viewHash)
			})
		}
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

	// Persist the changed metadata to the durable catalog (read-modify-write so
	// unrelated fields such as the UUID are preserved).
	meta, err := readCatalogDoc(ctx, state, collModBranchAM, params.Name)
	if err != nil {
		return fmt.Errorf("reading collection metadata for %q: %w", params.Name, err)
	}
	if meta == nil {
		meta = &collMeta{}
	}
	if params.SetValidator {
		meta.Validator = params.Validator
	}
	if params.ValidationLevel != "" {
		meta.ValidationLevel = params.ValidationLevel
	}
	if params.ValidationAction != "" {
		meta.ValidationAction = params.ValidationAction
	}
	if err := state.upsertCatalogDoc(ctx, db.rootish, params.Name, meta); err != nil {
		return fmt.Errorf("persisting collection metadata for %q: %w", params.Name, err)
	}

	return nil
}

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

	if err := am.IterAll(ctx, func(name string, collHash hash.Hash) error {
		if collHash.IsEmpty() || name == reservedCatalogName {
			return nil
		}

		isView, err := isViewEntry(ctx, state.cs, collHash)
		if err != nil {
			return err
		}
		if isView {
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
