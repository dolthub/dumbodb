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
	"fmt"
	"log/slog"

	"github.com/dolthub/dumbodb/internal/backends"
)

// backend implements backends.Backend interface by delegating all methods to the wrapped backend.
type backend struct {
	origB backends.Backend
	l     *slog.Logger
}

// NewBackend creates a new Backend that wraps the given backend.
func NewBackend(origB backends.Backend, l *slog.Logger) backends.Backend {
	return &backend{
		origB: origB,
		l:     l,
	}
}

// Close implements backends.Backend interface.
func (b *backend) Close() {
	b.origB.Close()
}

// Status implements backends.Backend interface.
func (b *backend) Status(ctx context.Context, params *backends.StatusParams) (*backends.StatusResult, error) {
	return b.origB.Status(ctx, params)
}

// Database implements backends.Backend interface.
func (b *backend) Database(name string) (backends.Database, error) {
	origDB, err := b.origB.Database(name)
	if err != nil {
		return nil, err
	}

	return newDatabase(origDB, name, b.origB, b.l), nil
}

// ListDatabases implements backends.Backend interface.
//
//nolint:lll // for readability
func (b *backend) ListDatabases(ctx context.Context, params *backends.ListDatabasesParams) (*backends.ListDatabasesResult, error) {
	return b.origB.ListDatabases(ctx, params)
}

// DropDatabase implements backends.Backend interface.
func (b *backend) DropDatabase(ctx context.Context, params *backends.DropDatabaseParams) error {
	return b.origB.DropDatabase(ctx, params)
}

// DumboDBCommit implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DumboDBCommit(ctx context.Context, params *backends.CommitParams) (*backends.CommitResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DumboDBCommit(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DumboDBCommit: versioning not supported by wrapped backend")
}

// DumboDBBranch implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DumboDBBranch(ctx context.Context, params *backends.BranchParams) (*backends.BranchResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DumboDBBranch(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DumboDBBranch: versioning not supported by wrapped backend")
}

// DumboDBMerge implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DumboDBMerge(ctx context.Context, params *backends.MergeParams) (*backends.MergeResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DumboDBMerge(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DumboDBMerge: versioning not supported by wrapped backend")
}

// DumboDBLog implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DumboDBLog(ctx context.Context, params *backends.LogParams) (*backends.LogResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DumboDBLog(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DumboDBLog: versioning not supported by wrapped backend")
}

// DumboDBStatus implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DumboDBStatus(ctx context.Context, params *backends.VersioningStatusParams) (*backends.VersioningStatusResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DumboDBStatus(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DumboDBStatus: versioning not supported by wrapped backend")
}

// DumboDBDiff implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DumboDBDiff(ctx context.Context, params *backends.DiffParams) (*backends.DiffResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DumboDBDiff(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DumboDBDiff: versioning not supported by wrapped backend")
}

// DumboDBReset implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DumboDBReset(ctx context.Context, params *backends.ResetParams) (*backends.ResetResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DumboDBReset(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DumboDBReset: versioning not supported by wrapped backend")
}

// DumboDBConflicts implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DumboDBConflicts(ctx context.Context, params *backends.ConflictsParams) (*backends.ConflictsResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DumboDBConflicts(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DumboDBConflicts: versioning not supported by wrapped backend")
}

// DumboDBResolveConflict implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DumboDBResolveConflict(ctx context.Context, params *backends.ResolveConflictParams) (*backends.ResolveConflictResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DumboDBResolveConflict(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DumboDBResolveConflict: versioning not supported by wrapped backend")
}

// DumboDBCherryPick implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DumboDBCherryPick(ctx context.Context, params *backends.CherryPickParams) (*backends.CherryPickResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DumboDBCherryPick(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DumboDBCherryPick: versioning not supported by wrapped backend")
}

// DumboDBRebase implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DumboDBRebase(ctx context.Context, params *backends.RebaseParams) (*backends.RebaseResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DumboDBRebase(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DumboDBRebase: versioning not supported by wrapped backend")
}

// DumboDBRevert implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DumboDBRevert(ctx context.Context, params *backends.RevertParams) (*backends.RevertResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DumboDBRevert(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DumboDBRevert: versioning not supported by wrapped backend")
}

// DumboDBTag implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DumboDBTag(ctx context.Context, params *backends.TagParams) (*backends.TagResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DumboDBTag(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DumboDBTag: versioning not supported by wrapped backend")
}

// check interfaces
var (
	_ backends.Backend          = (*backend)(nil)
	_ backends.VersioningBackend = (*backend)(nil)
)
