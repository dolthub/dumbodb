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

// Package stub provides a stub backend implementation that returns errors for all operations.
// It is used to scaffold the DumboDB server before the Dolt backend is implemented.
package stub

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/dolthub/dumbodb/internal/backends"
)

type Backend struct {
	l *slog.Logger
}

func NewBackend(l *slog.Logger) (backends.Backend, error) {
	b := &Backend{l: l}
	return backends.BackendContract(b), nil
}

func (b *Backend) Close() {}

func (b *Backend) Status(_ context.Context, _ *backends.StatusParams) (*backends.StatusResult, error) {
	return &backends.StatusResult{}, nil
}

func (b *Backend) Database(name string) (backends.Database, error) {
	return backends.DatabaseContract(&database{name: name, l: b.l}), nil
}

func (b *Backend) ListDatabases(_ context.Context, _ *backends.ListDatabasesParams) (*backends.ListDatabasesResult, error) {
	return &backends.ListDatabasesResult{}, nil
}

func (b *Backend) DropDatabase(_ context.Context, params *backends.DropDatabaseParams) error {
	return backends.NewError(backends.ErrorCodeDatabaseDoesNotExist, fmt.Errorf("stub: DropDatabase %q not implemented", params.Name))
}

type database struct {
	name string
	l    *slog.Logger
}

func (db *database) Collection(name string) (backends.Collection, error) {
	return backends.CollectionContract(&collection{dbName: db.name, name: name, l: db.l}), nil
}

func (db *database) ListCollections(_ context.Context, _ *backends.ListCollectionsParams) (*backends.ListCollectionsResult, error) {
	return &backends.ListCollectionsResult{}, nil
}

func (db *database) CreateCollection(_ context.Context, params *backends.CreateCollectionParams) error {
	return backends.NewError(backends.ErrorCodeCollectionAlreadyExists, fmt.Errorf("stub: CreateCollection %q.%q not implemented", db.name, params.Name))
}

func (db *database) DropCollection(_ context.Context, params *backends.DropCollectionParams) error {
	return backends.NewError(backends.ErrorCodeCollectionDoesNotExist, fmt.Errorf("stub: DropCollection %q.%q not implemented", db.name, params.Name))
}

func (db *database) RenameCollection(_ context.Context, params *backends.RenameCollectionParams) error {
	return fmt.Errorf("stub: RenameCollection %q not implemented", params.OldName)
}

func (db *database) CollMod(_ context.Context, params *backends.CollModParams) error {
	return backends.NewError(backends.ErrorCodeCollectionDoesNotExist, fmt.Errorf("stub: CollMod %q.%q not implemented", db.name, params.Name))
}

func (db *database) Stats(_ context.Context, _ *backends.DatabaseStatsParams) (*backends.DatabaseStatsResult, error) {
	return &backends.DatabaseStatsResult{}, nil
}

func (b *Backend) DumboDBCommit(_ context.Context, params *backends.CommitParams) (*backends.CommitResult, error) {
	b.l.Info("stub: DumboDBCommit", slog.String("db", params.DBName), slog.String("branch", params.Branch))

	ts := params.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	return &backends.CommitResult{
		CommitID:  "0000000000000000000000000000000000000000",
		Branch:    params.Branch,
		Message:   params.Message,
		Author:    params.Author,
		Timestamp: ts.UnixMilli(),
	}, nil
}

func (b *Backend) DumboDBBranch(_ context.Context, params *backends.BranchParams) (*backends.BranchResult, error) {
	b.l.Info("stub: DumboDBBranch", slog.String("db", params.DBName), slog.String("from", params.From), slog.String("name", params.Name))

	if params.List {
		return &backends.BranchResult{Branches: []backends.BranchInfo{}}, nil
	}

	return &backends.BranchResult{
		Branch: params.Name,
	}, nil
}

func (b *Backend) DumboDBMerge(_ context.Context, params *backends.MergeParams) (*backends.MergeResult, error) {
	b.l.Info("stub: DumboDBMerge", slog.String("db", params.DBName), slog.String("into", params.Into), slog.String("from", params.From))

	return &backends.MergeResult{
		CommitID: "0000000000000000000000000000000000000000",
		Message:  fmt.Sprintf("Merged '%s' into '%s'", params.From, params.Into),
	}, nil
}

func (b *Backend) DumboDBLog(_ context.Context, params *backends.LogParams) (*backends.LogResult, error) {
	b.l.Info("stub: DumboDBLog", slog.String("db", params.DBName), slog.String("branch", params.Branch))

	return &backends.LogResult{Commits: []backends.CommitInfo{}}, nil
}

func (b *Backend) DumboDBStatus(_ context.Context, params *backends.VersioningStatusParams) (*backends.VersioningStatusResult, error) {
	b.l.Info("stub: DumboDBStatus", slog.String("db", params.DBName), slog.String("branch", params.Branch))

	return &backends.VersioningStatusResult{
		Branch: params.Branch,
		Tables: []backends.TableStatus{},
	}, nil
}

func (b *Backend) DumboDBDiff(_ context.Context, params *backends.DiffParams) (*backends.DiffResult, error) {
	b.l.Info("stub: DumboDBDiff", slog.String("db", params.DBName), slog.String("from", params.From), slog.String("to", params.To))

	return &backends.DiffResult{Collections: []backends.CollectionDiff{}}, nil
}

func (b *Backend) DumboDBReset(_ context.Context, params *backends.ResetParams) (*backends.ResetResult, error) {
	b.l.Info("stub: DumboDBReset", slog.String("db", params.DBName), slog.String("commitId", params.CommitID), slog.Bool("hard", params.Hard))

	return &backends.ResetResult{CommitID: params.CommitID}, nil
}

func (b *Backend) DumboDBConflicts(_ context.Context, params *backends.ConflictsParams) (*backends.ConflictsResult, error) {
	b.l.Info("stub: DumboDBConflicts", slog.String("db", params.DBName), slog.String("branch", params.Branch))

	return &backends.ConflictsResult{}, nil
}

func (b *Backend) DumboDBResolveConflict(_ context.Context, params *backends.ResolveConflictParams) (*backends.ResolveConflictResult, error) {
	b.l.Info("stub: DumboDBResolveConflict", slog.String("db", params.DBName), slog.String("branch", params.Branch), slog.String("conflictId", params.ConflictID))

	return &backends.ResolveConflictResult{}, nil
}

func (b *Backend) DumboDBCherryPick(_ context.Context, params *backends.CherryPickParams) (*backends.CherryPickResult, error) {
	b.l.Info("stub: DumboDBCherryPick", slog.String("db", params.DBName), slog.String("branch", params.Branch), slog.String("commit", params.Commit))

	return &backends.CherryPickResult{CommitID: "stub", Message: "stub cherry-pick"}, nil
}

func (b *Backend) DumboDBRebase(_ context.Context, params *backends.RebaseParams) (*backends.RebaseResult, error) {
	b.l.Info("stub: DumboDBRebase", slog.String("db", params.DBName), slog.String("branch", params.Branch), slog.String("onto", params.Onto))

	return &backends.RebaseResult{CommitsReplayed: 0, NewTip: "stub"}, nil
}

func (b *Backend) DumboDBTag(_ context.Context, params *backends.TagParams) (*backends.TagResult, error) {
	b.l.Info("stub: DumboDBTag",
		slog.String("db", params.DBName),
		slog.String("name", params.Name),
		slog.String("hash", params.Hash),
		slog.Bool("delete", params.Delete),
	)

	return &backends.TagResult{Tags: []backends.TagInfo{}}, nil
}

type collection struct {
	dbName string
	name   string
	l      *slog.Logger
}

func (c *collection) Query(_ context.Context, _ *backends.QueryParams) (*backends.QueryResult, error) {
	return nil, fmt.Errorf("stub: Query %q.%q not implemented", c.dbName, c.name)
}

func (c *collection) Count(_ context.Context, _ *backends.CountParams) (*backends.CountResult, error) {
	return &backends.CountResult{}, nil
}

func (c *collection) Explain(_ context.Context, _ *backends.ExplainParams) (*backends.ExplainResult, error) {
	return nil, fmt.Errorf("stub: Explain %q.%q not implemented", c.dbName, c.name)
}

func (c *collection) InsertAll(_ context.Context, _ *backends.InsertAllParams) (*backends.InsertAllResult, error) {
	return nil, fmt.Errorf("stub: InsertAll %q.%q not implemented", c.dbName, c.name)
}

func (c *collection) UpdateAll(_ context.Context, _ *backends.UpdateAllParams) (*backends.UpdateAllResult, error) {
	return nil, fmt.Errorf("stub: UpdateAll %q.%q not implemented", c.dbName, c.name)
}

func (c *collection) DeleteAll(_ context.Context, _ *backends.DeleteAllParams) (*backends.DeleteAllResult, error) {
	return nil, fmt.Errorf("stub: DeleteAll %q.%q not implemented", c.dbName, c.name)
}

func (c *collection) Stats(_ context.Context, _ *backends.CollectionStatsParams) (*backends.CollectionStatsResult, error) {
	return &backends.CollectionStatsResult{}, nil
}

func (c *collection) Compact(_ context.Context, _ *backends.CompactParams) (*backends.CompactResult, error) {
	return nil, fmt.Errorf("stub: Compact %q.%q not implemented", c.dbName, c.name)
}

func (c *collection) ListIndexes(_ context.Context, _ *backends.ListIndexesParams) (*backends.ListIndexesResult, error) {
	return &backends.ListIndexesResult{}, nil
}

func (c *collection) CreateIndexes(_ context.Context, _ *backends.CreateIndexesParams) (*backends.CreateIndexesResult, error) {
	return nil, fmt.Errorf("stub: CreateIndexes %q.%q not implemented", c.dbName, c.name)
}

func (c *collection) DropIndexes(_ context.Context, _ *backends.DropIndexesParams) (*backends.DropIndexesResult, error) {
	return nil, fmt.Errorf("stub: DropIndexes %q.%q not implemented", c.dbName, c.name)
}

func (b *Backend) DumboDBRemote(_ context.Context, params *backends.RemoteParams) (*backends.RemoteResult, error) {
	b.l.Info("stub: DumboDBRemote",
		slog.String("db", params.DBName),
		slog.String("action", params.Action),
		slog.String("name", params.Name),
	)

	return &backends.RemoteResult{Remotes: []backends.RemoteInfo{}}, nil
}

func (b *Backend) DumboDBPush(_ context.Context, params *backends.PushParams) (*backends.PushResult, error) {
	b.l.Info("stub: DumboDBPush",
		slog.String("db", params.DBName),
		slog.String("remote", params.Remote),
		slog.String("branch", params.Branch),
	)

	return &backends.PushResult{Remote: params.Remote, Branch: params.Branch}, nil
}

func (b *Backend) DumboDBFetch(_ context.Context, params *backends.FetchParams) (*backends.FetchResult, error) {
	b.l.Info("stub: DumboDBFetch",
		slog.String("db", params.DBName),
		slog.String("remote", params.Remote),
		slog.String("branch", params.Branch),
	)

	return &backends.FetchResult{Remote: params.Remote, Branch: params.Branch}, nil
}
