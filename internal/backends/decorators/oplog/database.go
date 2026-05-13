// Copyright 2021 FerretDB Inc.
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
	"log/slog"

	"github.com/dolthub/dumbodb/internal/backends"
)

type database struct {
	origDB backends.Database
	name   string
	origB  backends.Backend
	l      *slog.Logger
}

func newDatabase(origDB backends.Database, name string, origB backends.Backend, l *slog.Logger) backends.Database {
	return &database{
		origDB: origDB,
		name:   name,
		origB:  origB,
		l:      l,
	}
}

func (db *database) Collection(name string) (backends.Collection, error) {
	origC, err := db.origDB.Collection(name)
	if err != nil {
		return nil, err
	}

	return newCollection(origC, name, db.name, db.origB, db.l), nil
}

//nolint:lll // for readability
func (db *database) ListCollections(ctx context.Context, params *backends.ListCollectionsParams) (*backends.ListCollectionsResult, error) {
	return db.origDB.ListCollections(ctx, params)
}

func (db *database) CreateCollection(ctx context.Context, params *backends.CreateCollectionParams) error {
	return db.origDB.CreateCollection(ctx, params)
}

func (db *database) DropCollection(ctx context.Context, params *backends.DropCollectionParams) error {
	return db.origDB.DropCollection(ctx, params)
}

func (db *database) RenameCollection(ctx context.Context, params *backends.RenameCollectionParams) error {
	return db.origDB.RenameCollection(ctx, params)
}

func (db *database) CollMod(ctx context.Context, params *backends.CollModParams) error {
	return db.origDB.CollMod(ctx, params)
}

func (db *database) Stats(ctx context.Context, params *backends.DatabaseStatsParams) (*backends.DatabaseStatsResult, error) {
	return db.origDB.Stats(ctx, params)
}

var (
	_ backends.Database = (*database)(nil)
)
