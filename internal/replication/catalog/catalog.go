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

// Package catalog applies MongoDB collection catalog changes using source UUID identity.
package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/types"
)

var (
	ErrSourceUUIDNotFound  = errors.New("source collection UUID not found")
	ErrSourceUUIDAmbiguous = errors.New("source collection UUID is not unique")
)

type Location struct {
	Database   string
	Collection string
	LocalUUID  string
	SourceUUID string
}

type Applier struct {
	backend backends.Backend
	store   *control.Store
}

func NewApplier(backend backends.Backend, store *control.Store) (*Applier, error) {
	if backend == nil || store == nil {
		return nil, errors.New("catalog applier requires backend and control store")
	}
	return &Applier{backend: backend, store: store}, nil
}

func (a *Applier) Resolve(ctx context.Context, sourceUUID string) (Location, error) {
	return Resolve(ctx, a.backend, sourceUUID)
}

func Resolve(ctx context.Context, backend backends.Backend, sourceUUID string) (Location, error) {
	if sourceUUID == "" {
		return Location{}, errors.New("source collection UUID is required")
	}
	databases, err := backend.ListDatabases(ctx, nil)
	if err != nil {
		return Location{}, err
	}
	var result Location
	for _, databaseInfo := range databases.Databases {
		database, err := backend.Database(databaseInfo.Name)
		if err != nil {
			return Location{}, err
		}
		collections, err := database.ListCollections(ctx, nil)
		if err != nil {
			return Location{}, err
		}
		for _, collection := range collections.Collections {
			if collection.SourceUUID != sourceUUID {
				continue
			}
			if result.SourceUUID != "" {
				return Location{}, fmt.Errorf("%w: %q is present at both %q.%q and %q.%q", ErrSourceUUIDAmbiguous, sourceUUID, result.Database, result.Collection, databaseInfo.Name, collection.Name)
			}
			result = Location{
				Database: databaseInfo.Name, Collection: collection.Name,
				LocalUUID: collection.UUID, SourceUUID: collection.SourceUUID,
			}
		}
	}
	if result.SourceUUID == "" {
		return Location{}, fmt.Errorf("%w: %q", ErrSourceUUIDNotFound, sourceUUID)
	}
	return result, nil
}

func (a *Applier) Create(ctx context.Context, databaseName string, params backends.CreateCollectionParams, sourceUUID string, opTime control.OpTime) (Location, error) {
	if sourceUUID == "" {
		return Location{}, errors.New("source collection UUID is required")
	}
	if existing, err := a.Resolve(ctx, sourceUUID); err == nil {
		if existing.Database != databaseName || existing.Collection != params.Name {
			return Location{}, fmt.Errorf("source UUID %q already identifies %q.%q", sourceUUID, existing.Database, existing.Collection)
		}
		if err := a.recordCreate(existing, opTime); err != nil {
			return Location{}, err
		}
		return existing, nil
	} else if !errors.Is(err, ErrSourceUUIDNotFound) {
		return Location{}, err
	}
	database, err := a.backend.Database(databaseName)
	if err != nil {
		return Location{}, err
	}
	params.SourceUUID = sourceUUID
	if err := database.CreateCollection(ctx, &params); err != nil {
		return Location{}, err
	}
	location, err := a.Resolve(ctx, sourceUUID)
	if err != nil {
		return Location{}, err
	}
	if err := a.recordCreate(location, opTime); err != nil {
		return Location{}, err
	}
	return location, nil
}

func (a *Applier) CreateView(ctx context.Context, databaseName string, params backends.CreateCollectionParams) error {
	if params.ViewOn == "" {
		return errors.New("replicated view requires viewOn")
	}
	params.SourceUUID = ""
	database, err := a.backend.Database(databaseName)
	if err != nil {
		return err
	}
	return database.CreateCollection(ctx, &params)
}

func (a *Applier) CollModView(ctx context.Context, databaseName string, params backends.CollModParams) error {
	if params.Name == "" || !params.SetView {
		return errors.New("replicated view collMod requires name and view definition")
	}
	database, err := a.backend.Database(databaseName)
	if err != nil {
		return err
	}
	return database.CollMod(ctx, &params)
}

func (a *Applier) DropView(ctx context.Context, databaseName, viewName string) error {
	if viewName == "" {
		return errors.New("replicated view name is required")
	}
	database, err := a.backend.Database(databaseName)
	if err != nil {
		return err
	}
	return database.DropCollection(ctx, &backends.DropCollectionParams{Name: viewName})
}

func (a *Applier) Rename(ctx context.Context, sourceUUID, newDatabase, newCollection string, opTime control.OpTime) error {
	location, err := a.Resolve(ctx, sourceUUID)
	if err != nil {
		return err
	}
	mapping, ok := a.store.ActiveCollectionMapping(sourceUUID)
	if !ok {
		return fmt.Errorf("source UUID %q has no active control mapping", sourceUUID)
	}
	if location.Database == newDatabase && location.Collection == newCollection {
		return a.store.RenameCollectionMapping(sourceUUID, mapping.Database, mapping.Collection, newDatabase, newCollection, opTime)
	}
	if location.Database != mapping.Database || location.Collection != mapping.Collection {
		return fmt.Errorf("catalog location %q.%q disagrees with control mapping %q.%q", location.Database, location.Collection, mapping.Database, mapping.Collection)
	}
	if location.Database != newDatabase {
		return errors.New("cross-database replicated rename is not supported")
	}
	database, err := a.backend.Database(location.Database)
	if err != nil {
		return err
	}
	if err := database.RenameCollection(ctx, &backends.RenameCollectionParams{OldName: location.Collection, NewName: newCollection}); err != nil {
		return err
	}
	return a.store.RenameCollectionMapping(sourceUUID, location.Database, location.Collection, newDatabase, newCollection, opTime)
}

func (a *Applier) Drop(ctx context.Context, sourceUUID string, opTime control.OpTime) error {
	location, err := a.Resolve(ctx, sourceUUID)
	if errors.Is(err, ErrSourceUUIDNotFound) {
		return a.store.DropCollectionMapping(sourceUUID, opTime)
	}
	if err != nil {
		return err
	}
	database, err := a.backend.Database(location.Database)
	if err != nil {
		return err
	}
	if err := database.DropCollection(ctx, &backends.DropCollectionParams{Name: location.Collection}); err != nil {
		return err
	}
	return a.store.DropCollectionMapping(sourceUUID, opTime)
}

func (a *Applier) CollMod(ctx context.Context, sourceUUID string, params backends.CollModParams) error {
	location, err := a.Resolve(ctx, sourceUUID)
	if err != nil {
		return err
	}
	if params.Name != "" && params.Name != location.Collection {
		return fmt.Errorf("collMod name %q does not match source UUID location %q", params.Name, location.Collection)
	}
	params.Name = location.Collection
	database, err := a.backend.Database(location.Database)
	if err != nil {
		return err
	}
	return database.CollMod(ctx, &params)
}

func (a *Applier) CreateIndexes(ctx context.Context, sourceUUID string, indexes []backends.IndexInfo) error {
	location, err := a.Resolve(ctx, sourceUUID)
	if err != nil {
		return err
	}
	database, err := a.backend.Database(location.Database)
	if err != nil {
		return err
	}
	collection, err := database.Collection(location.Collection)
	if err != nil {
		return err
	}
	existing, err := collection.ListIndexes(ctx, nil)
	if err != nil {
		return err
	}
	existingByName := make(map[string]backends.IndexInfo, len(existing.Indexes))
	for _, index := range existing.Indexes {
		existingByName[index.Name] = index
	}
	toCreate := make([]backends.IndexInfo, 0, len(indexes))
	seen := make(map[string]struct{}, len(indexes))
	for _, index := range indexes {
		if _, ok := seen[index.Name]; ok {
			return fmt.Errorf("replicated index list repeats name %q", index.Name)
		}
		seen[index.Name] = struct{}{}
		if current, ok := existingByName[index.Name]; ok {
			if !sameIndex(current, index) {
				return fmt.Errorf("replicated index %q conflicts with existing definition", index.Name)
			}
			continue
		}
		toCreate = append(toCreate, index)
	}
	if len(toCreate) == 0 {
		return nil
	}
	_, err = collection.CreateIndexes(ctx, &backends.CreateIndexesParams{Indexes: toCreate})
	return err
}

func (a *Applier) DropIndexes(ctx context.Context, sourceUUID string, indexNames []string) error {
	location, err := a.Resolve(ctx, sourceUUID)
	if err != nil {
		return err
	}
	database, err := a.backend.Database(location.Database)
	if err != nil {
		return err
	}
	collection, err := database.Collection(location.Collection)
	if err != nil {
		return err
	}
	_, err = collection.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: indexNames})
	return err
}

func (a *Applier) recordCreate(location Location, opTime control.OpTime) error {
	return a.store.PutCollectionMapping(control.CollectionMapping{
		SourceUUID: location.SourceUUID, Database: location.Database,
		Collection: location.Collection, LocalUUID: location.LocalUUID,
		CreateOpTime: opTime, LastUpdateOpTime: opTime,
	})
}

func sameIndex(left, right backends.IndexInfo) bool {
	if left.Name != right.Name || left.Unique != right.Unique || left.Sparse != right.Sparse || left.Hidden != right.Hidden || len(left.Key) != len(right.Key) {
		return false
	}
	for index := range left.Key {
		if left.Key[index] != right.Key[index] {
			return false
		}
	}
	if !sameDocument(left.PartialFilterExpression, right.PartialFilterExpression) {
		return false
	}
	return sameDocument(left.Collation, right.Collation)
}

func sameDocument(left, right *types.Document) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return types.Compare(left, right) == types.Equal
}
