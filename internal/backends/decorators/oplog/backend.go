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

// DocuDoltCommit implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocuDoltCommit(ctx context.Context, params *backends.CommitParams) (*backends.CommitResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocuDoltCommit(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocuDoltCommit: versioning not supported by wrapped backend")
}

// DocuDoltBranch implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocuDoltBranch(ctx context.Context, params *backends.BranchParams) (*backends.BranchResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocuDoltBranch(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocuDoltBranch: versioning not supported by wrapped backend")
}

// DocuDoltMerge implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocuDoltMerge(ctx context.Context, params *backends.MergeParams) (*backends.MergeResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocuDoltMerge(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocuDoltMerge: versioning not supported by wrapped backend")
}

// DocuDoltLog implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocuDoltLog(ctx context.Context, params *backends.LogParams) (*backends.LogResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocuDoltLog(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocuDoltLog: versioning not supported by wrapped backend")
}

// DocuDoltStatus implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocuDoltStatus(ctx context.Context, params *backends.VersioningStatusParams) (*backends.VersioningStatusResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocuDoltStatus(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocuDoltStatus: versioning not supported by wrapped backend")
}

// DocuDoltDiff implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocuDoltDiff(ctx context.Context, params *backends.DiffParams) (*backends.DiffResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocuDoltDiff(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocuDoltDiff: versioning not supported by wrapped backend")
}

// DocuDoltCurrentBranch implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocuDoltCurrentBranch(ctx context.Context, params *backends.CurrentBranchParams) (*backends.CurrentBranchResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocuDoltCurrentBranch(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocuDoltCurrentBranch: versioning not supported by wrapped backend")
}

// DocuDoltReset implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocuDoltReset(ctx context.Context, params *backends.ResetParams) (*backends.ResetResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocuDoltReset(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocuDoltReset: versioning not supported by wrapped backend")
}

// DocuDoltConflicts implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocuDoltConflicts(ctx context.Context, params *backends.ConflictsParams) (*backends.ConflictsResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocuDoltConflicts(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocuDoltConflicts: versioning not supported by wrapped backend")
}

// DocuDoltResolveConflict implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocuDoltResolveConflict(ctx context.Context, params *backends.ResolveConflictParams) (*backends.ResolveConflictResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocuDoltResolveConflict(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocuDoltResolveConflict: versioning not supported by wrapped backend")
}

// DocuDoltCherryPick implements backends.VersioningBackend if the wrapped backend supports it.
func (b *backend) DocuDoltCherryPick(ctx context.Context, params *backends.CherryPickParams) (*backends.CherryPickResult, error) {
	if vb, ok := b.origB.(backends.VersioningBackend); ok {
		return vb.DocuDoltCherryPick(ctx, params)
	}

	return nil, fmt.Errorf("oplog: DocuDoltCherryPick: versioning not supported by wrapped backend")
}

// check interfaces
var (
	_ backends.Backend          = (*backend)(nil)
	_ backends.VersioningBackend = (*backend)(nil)
)
