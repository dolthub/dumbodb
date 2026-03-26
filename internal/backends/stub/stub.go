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

// Package stub provides a stub backend implementation that returns errors for all operations.
// It is used to scaffold the Dongo server before the Dolt backend is implemented.
package stub

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dolthub/dongo/internal/backends"
)

// Backend implements backends.Backend interface with stub (no-op) implementations.
type Backend struct {
	l *slog.Logger
}

// NewBackend creates a new stub Backend.
func NewBackend(l *slog.Logger) (backends.Backend, error) {
	b := &Backend{l: l}
	return backends.BackendContract(b), nil
}

// Close implements backends.Backend interface.
func (b *Backend) Close() {}

// Status implements backends.Backend interface.
func (b *Backend) Status(_ context.Context, _ *backends.StatusParams) (*backends.StatusResult, error) {
	return &backends.StatusResult{}, nil
}

// Database implements backends.Backend interface.
func (b *Backend) Database(name string) (backends.Database, error) {
	return backends.DatabaseContract(&database{name: name, l: b.l}), nil
}

// ListDatabases implements backends.Backend interface.
func (b *Backend) ListDatabases(_ context.Context, _ *backends.ListDatabasesParams) (*backends.ListDatabasesResult, error) {
	return &backends.ListDatabasesResult{}, nil
}

// DropDatabase implements backends.Backend interface.
func (b *Backend) DropDatabase(_ context.Context, params *backends.DropDatabaseParams) error {
	return backends.NewError(backends.ErrorCodeDatabaseDoesNotExist, fmt.Errorf("stub: DropDatabase %q not implemented", params.Name))
}

// Describe implements prometheus.Collector interface.
func (b *Backend) Describe(ch chan<- *prometheus.Desc) {}

// Collect implements prometheus.Collector interface.
func (b *Backend) Collect(ch chan<- prometheus.Metric) {}

// database implements backends.Database interface.
type database struct {
	name string
	l    *slog.Logger
}

// Collection implements backends.Database interface.
func (db *database) Collection(name string) (backends.Collection, error) {
	return backends.CollectionContract(&collection{dbName: db.name, name: name, l: db.l}), nil
}

// ListCollections implements backends.Database interface.
func (db *database) ListCollections(_ context.Context, _ *backends.ListCollectionsParams) (*backends.ListCollectionsResult, error) {
	return &backends.ListCollectionsResult{}, nil
}

// CreateCollection implements backends.Database interface.
func (db *database) CreateCollection(_ context.Context, params *backends.CreateCollectionParams) error {
	return backends.NewError(backends.ErrorCodeCollectionAlreadyExists, fmt.Errorf("stub: CreateCollection %q.%q not implemented", db.name, params.Name))
}

// DropCollection implements backends.Database interface.
func (db *database) DropCollection(_ context.Context, params *backends.DropCollectionParams) error {
	return backends.NewError(backends.ErrorCodeCollectionDoesNotExist, fmt.Errorf("stub: DropCollection %q.%q not implemented", db.name, params.Name))
}

// RenameCollection implements backends.Database interface.
func (db *database) RenameCollection(_ context.Context, params *backends.RenameCollectionParams) error {
	return fmt.Errorf("stub: RenameCollection %q not implemented", params.OldName)
}

// Stats implements backends.Database interface.
func (db *database) Stats(_ context.Context, _ *backends.DatabaseStatsParams) (*backends.DatabaseStatsResult, error) {
	return &backends.DatabaseStatsResult{}, nil
}

// DongoCommit implements backends.VersioningBackend interface.
func (b *Backend) DongoCommit(_ context.Context, params *backends.CommitParams) (*backends.CommitResult, error) {
	b.l.Info("stub: DongoCommit", slog.String("db", params.DBName), slog.String("branch", params.Branch))

	return &backends.CommitResult{
		Hash:    "0000000000000000000000000000000000000000",
		Branch:  params.Branch,
		Message: params.Message,
	}, nil
}

// DongoBranch implements backends.VersioningBackend interface.
func (b *Backend) DongoBranch(_ context.Context, params *backends.BranchParams) (*backends.BranchResult, error) {
	b.l.Info("stub: DongoBranch", slog.String("db", params.DBName), slog.String("from", params.From), slog.String("name", params.Name))

	return &backends.BranchResult{
		Branch: params.Name,
	}, nil
}

// DongoMerge implements backends.VersioningBackend interface.
func (b *Backend) DongoMerge(_ context.Context, params *backends.MergeParams) (*backends.MergeResult, error) {
	b.l.Info("stub: DongoMerge", slog.String("db", params.DBName), slog.String("into", params.Into), slog.String("from", params.From))

	return &backends.MergeResult{
		Hash:    "0000000000000000000000000000000000000000",
		Message: fmt.Sprintf("Merged '%s' into '%s'", params.From, params.Into),
	}, nil
}

// DongoLog implements backends.VersioningBackend interface.
func (b *Backend) DongoLog(_ context.Context, params *backends.LogParams) (*backends.LogResult, error) {
	b.l.Info("stub: DongoLog", slog.String("db", params.DBName), slog.String("branch", params.Branch))

	return &backends.LogResult{Commits: []backends.CommitInfo{}}, nil
}

// DongoStatus implements backends.VersioningBackend interface.
func (b *Backend) DongoStatus(_ context.Context, params *backends.VersioningStatusParams) (*backends.VersioningStatusResult, error) {
	b.l.Info("stub: DongoStatus", slog.String("db", params.DBName), slog.String("branch", params.Branch))

	return &backends.VersioningStatusResult{
		Branch: params.Branch,
		Tables: []backends.TableStatus{},
	}, nil
}

// DongoDiff implements backends.VersioningBackend interface.
func (b *Backend) DongoDiff(_ context.Context, params *backends.DiffParams) (*backends.DiffResult, error) {
	b.l.Info("stub: DongoDiff", slog.String("db", params.DBName), slog.String("from", params.From), slog.String("to", params.To))

	return &backends.DiffResult{Collections: []backends.CollectionDiff{}}, nil
}

// DongoReset implements backends.VersioningBackend interface.
func (b *Backend) DongoReset(_ context.Context, params *backends.ResetParams) (*backends.ResetResult, error) {
	b.l.Info("stub: DongoReset", slog.String("db", params.DBName), slog.String("hash", params.Hash), slog.Bool("hard", params.Hard))

	return &backends.ResetResult{Hash: params.Hash}, nil
}

// collection implements backends.Collection interface.
type collection struct {
	dbName string
	name   string
	l      *slog.Logger
}

// Query implements backends.Collection interface.
func (c *collection) Query(_ context.Context, _ *backends.QueryParams) (*backends.QueryResult, error) {
	return nil, fmt.Errorf("stub: Query %q.%q not implemented", c.dbName, c.name)
}

// Explain implements backends.Collection interface.
func (c *collection) Explain(_ context.Context, _ *backends.ExplainParams) (*backends.ExplainResult, error) {
	return nil, fmt.Errorf("stub: Explain %q.%q not implemented", c.dbName, c.name)
}

// InsertAll implements backends.Collection interface.
func (c *collection) InsertAll(_ context.Context, _ *backends.InsertAllParams) (*backends.InsertAllResult, error) {
	return nil, fmt.Errorf("stub: InsertAll %q.%q not implemented", c.dbName, c.name)
}

// UpdateAll implements backends.Collection interface.
func (c *collection) UpdateAll(_ context.Context, _ *backends.UpdateAllParams) (*backends.UpdateAllResult, error) {
	return nil, fmt.Errorf("stub: UpdateAll %q.%q not implemented", c.dbName, c.name)
}

// DeleteAll implements backends.Collection interface.
func (c *collection) DeleteAll(_ context.Context, _ *backends.DeleteAllParams) (*backends.DeleteAllResult, error) {
	return nil, fmt.Errorf("stub: DeleteAll %q.%q not implemented", c.dbName, c.name)
}

// Stats implements backends.Collection interface.
func (c *collection) Stats(_ context.Context, _ *backends.CollectionStatsParams) (*backends.CollectionStatsResult, error) {
	return &backends.CollectionStatsResult{}, nil
}

// Compact implements backends.Collection interface.
func (c *collection) Compact(_ context.Context, _ *backends.CompactParams) (*backends.CompactResult, error) {
	return nil, fmt.Errorf("stub: Compact %q.%q not implemented", c.dbName, c.name)
}

// ListIndexes implements backends.Collection interface.
func (c *collection) ListIndexes(_ context.Context, _ *backends.ListIndexesParams) (*backends.ListIndexesResult, error) {
	return &backends.ListIndexesResult{}, nil
}

// CreateIndexes implements backends.Collection interface.
func (c *collection) CreateIndexes(_ context.Context, _ *backends.CreateIndexesParams) (*backends.CreateIndexesResult, error) {
	return nil, fmt.Errorf("stub: CreateIndexes %q.%q not implemented", c.dbName, c.name)
}

// DropIndexes implements backends.Collection interface.
func (c *collection) DropIndexes(_ context.Context, _ *backends.DropIndexesParams) (*backends.DropIndexesResult, error) {
	return nil, fmt.Errorf("stub: DropIndexes %q.%q not implemented", c.dbName, c.name)
}
