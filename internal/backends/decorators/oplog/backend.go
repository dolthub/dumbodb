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

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dolthub/docudolt/internal/backends"
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

// Describe implements prometheus.Collector.
func (b *backend) Describe(ch chan<- *prometheus.Desc) {
	b.origB.Describe(ch)
}

// Collect implements prometheus.Collector.
func (b *backend) Collect(ch chan<- prometheus.Metric) {
	b.origB.Collect(ch)
}

// DocudoltCommit implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocudoltCommit(ctx context.Context, params *backends.CommitParams) (*backends.CommitResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocudoltCommit(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocudoltCommit: versioning not supported by wrapped backend")
}

// DocudoltBranch implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocudoltBranch(ctx context.Context, params *backends.BranchParams) (*backends.BranchResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocudoltBranch(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocudoltBranch: versioning not supported by wrapped backend")
}

// DocudoltMerge implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocudoltMerge(ctx context.Context, params *backends.MergeParams) (*backends.MergeResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocudoltMerge(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocudoltMerge: versioning not supported by wrapped backend")
}

// DocudoltLog implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocudoltLog(ctx context.Context, params *backends.LogParams) (*backends.LogResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocudoltLog(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocudoltLog: versioning not supported by wrapped backend")
}

// DocudoltStatus implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocudoltStatus(ctx context.Context, params *backends.VersioningStatusParams) (*backends.VersioningStatusResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocudoltStatus(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocudoltStatus: versioning not supported by wrapped backend")
}

// DocudoltDiff implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocudoltDiff(ctx context.Context, params *backends.DiffParams) (*backends.DiffResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocudoltDiff(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocudoltDiff: versioning not supported by wrapped backend")
}

// DocudoltCurrentBranch implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocudoltCurrentBranch(ctx context.Context, params *backends.CurrentBranchParams) (*backends.CurrentBranchResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocudoltCurrentBranch(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocudoltCurrentBranch: versioning not supported by wrapped backend")
}

// DocudoltReset implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocudoltReset(ctx context.Context, params *backends.ResetParams) (*backends.ResetResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocudoltReset(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocudoltReset: versioning not supported by wrapped backend")
}

// DocudoltConflicts implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocudoltConflicts(ctx context.Context, params *backends.ConflictsParams) (*backends.ConflictsResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocudoltConflicts(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocudoltConflicts: versioning not supported by wrapped backend")
}

// DocudoltResolveConflict implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocudoltResolveConflict(ctx context.Context, params *backends.ResolveConflictParams) (*backends.ResolveConflictResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocudoltResolveConflict(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocudoltResolveConflict: versioning not supported by wrapped backend")
}

// check interfaces
var (
	_ backends.Backend          = (*backend)(nil)
	_ backends.VersioningBackend = (*backend)(nil)
)
