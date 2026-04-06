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

package backends

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"

	"github.com/dolthub/docudolt/internal/clientconn/conninfo"
	"github.com/dolthub/docudolt/internal/util/must"
	"github.com/dolthub/docudolt/internal/util/resource"
)

// Backend is a generic interface for all backends for accessing them.
//
// Backend object should be stateful and wrap database connection(s).
// Handler uses only one long-lived Backend object.
//
// Backend(s) methods can be called by multiple client connections / command handlers concurrently.
// They should be thread-safe.
//
// See backendContract and its methods for additional details.
type Backend interface {
	Close()

	Status(context.Context, *StatusParams) (*StatusResult, error)

	Database(string) (Database, error)
	ListDatabases(context.Context, *ListDatabasesParams) (*ListDatabasesResult, error)
	DropDatabase(context.Context, *DropDatabaseParams) error

	prometheus.Collector

	// There is no interface method to create a database; see package documentation.
}

// backendContract implements Backend interface.
type backendContract struct {
	b     Backend
	token *resource.Token
}

// BackendContract wraps Backend and enforces its contract.
//
// All backend implementations should use that function when they create new Backend instances.
// The handler should not use that function.
//
// See backendContract and its methods for additional details.
func BackendContract(b Backend) Backend {
	bc := &backendContract{
		b:     b,
		token: resource.NewToken(),
	}
	resource.Track(bc, bc.token)

	return bc
}

// Close closes all database connections and frees all resources associated with the backend.
func (bc *backendContract) Close() {
	bc.b.Close()

	resource.Untrack(bc, bc.token)
}

// StatusParams represents the parameters of Backend.Status method.
type StatusParams struct{}

// StatusResult represents the results of Backend.Status method.
type StatusResult struct {
	CountCollections       int64
	CountCappedCollections int32
}

// Status returns backend's status.
//
// This method should also be used to check that the backend is alive,
// connection can be established and authenticated.
// For that reason, the implementation should not return only cached results.
func (bc *backendContract) Status(ctx context.Context, params *StatusParams) (*StatusResult, error) {
	// to both check that conninfo is present (which is important for that method),
	// and to render doc.go correctly
	must.NotBeZero(conninfo.Get(ctx))

	ctx, span := otel.Tracer("").Start(ctx, "Status")
	defer span.End()

	res, err := bc.b.Status(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err)

	return res, err
}

// Database returns a Database instance for the given valid name.
//
// The database does not need to exist.
func (bc *backendContract) Database(name string) (Database, error) {
	var res Database

	err := validateDatabaseName(name)
	if err == nil {
		res, err = bc.b.Database(name)
	}

	checkError(err, ErrorCodeDatabaseNameIsInvalid)

	return res, err
}

// ListDatabasesParams represents the parameters of Backend.ListDatabases method.
type ListDatabasesParams struct {
	Name string
}

// ListDatabasesResult represents the results of Backend.ListDatabases method.
type ListDatabasesResult struct {
	Databases []DatabaseInfo
}

// DatabaseInfo represents information about a single database.
type DatabaseInfo struct {
	Name string
}

// ListDatabases returns a list of databases sorted by name.
//
// If ListDatabasesParams' Name is not empty, then only the database with that name should be returned (or an empty list).
//
// Database may not exist; that's not an error.
func (bc *backendContract) ListDatabases(ctx context.Context, params *ListDatabasesParams) (*ListDatabasesResult, error) {
	ctx, span := otel.Tracer("").Start(ctx, "ListDatabases")
	defer span.End()

	res, err := bc.b.ListDatabases(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err)

	if res != nil && len(res.Databases) > 0 {
		must.BeTrue(slices.IsSortedFunc(res.Databases, func(a, b DatabaseInfo) int {
			return cmp.Compare(a.Name, b.Name)
		}))

		if params != nil && params.Name != "" {
			must.BeTrue(len(res.Databases) == 1)
			must.BeTrue(res.Databases[0].Name == params.Name)
		}
	}

	return res, err
}

// DropDatabaseParams represents the parameters of Backend.DropDatabase method.
type DropDatabaseParams struct {
	Name string
}

// DropDatabase drops existing database for given parameters (including valid name).
func (bc *backendContract) DropDatabase(ctx context.Context, params *DropDatabaseParams) error {
	ctx, span := otel.Tracer("").Start(ctx, "DropDatabase")
	defer span.End()

	err := validateDatabaseName(params.Name)
	if err == nil {
		err = bc.b.DropDatabase(ctx, params)
	}

	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err, ErrorCodeDatabaseNameIsInvalid, ErrorCodeDatabaseDoesNotExist)

	return err
}

// Describe implements prometheus.Collector.
func (bc *backendContract) Describe(ch chan<- *prometheus.Desc) {
	bc.b.Describe(ch)
}

// Collect implements prometheus.Collector.
func (bc *backendContract) Collect(ch chan<- prometheus.Metric) {
	bc.b.Collect(ch)
}

// DocudoltCommit implements VersioningBackend if the wrapped backend supports it.
func (bc *backendContract) DocudoltCommit(ctx context.Context, params *CommitParams) (*CommitResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DocudoltCommit(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DocudoltCommit")
}

// DocudoltBranch implements VersioningBackend if the wrapped backend supports it.
func (bc *backendContract) DocudoltBranch(ctx context.Context, params *BranchParams) (*BranchResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DocudoltBranch(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DocudoltBranch")
}

// DocudoltMerge implements VersioningBackend if the wrapped backend supports it.
func (bc *backendContract) DocudoltMerge(ctx context.Context, params *MergeParams) (*MergeResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DocudoltMerge(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DocudoltMerge")
}

// DocudoltLog implements VersioningBackend if the wrapped backend supports it.
func (bc *backendContract) DocudoltLog(ctx context.Context, params *LogParams) (*LogResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DocudoltLog(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DocudoltLog")
}

// DocudoltStatus implements VersioningBackend if the wrapped backend supports it.
func (bc *backendContract) DocudoltStatus(ctx context.Context, params *VersioningStatusParams) (*VersioningStatusResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DocudoltStatus(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DocudoltStatus")
}

// DocudoltDiff implements VersioningBackend if the wrapped backend supports it.
func (bc *backendContract) DocudoltDiff(ctx context.Context, params *DiffParams) (*DiffResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DocudoltDiff(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DocudoltDiff")
}

func (bc *backendContract) DocudoltCurrentBranch(ctx context.Context, params *CurrentBranchParams) (*CurrentBranchResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DocudoltCurrentBranch(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DocudoltCurrentBranch")
}

func (bc *backendContract) DocudoltReset(ctx context.Context, params *ResetParams) (*ResetResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DocudoltReset(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DocudoltReset")
}

// DocudoltConflicts implements VersioningBackend if the wrapped backend supports it.
func (bc *backendContract) DocudoltConflicts(ctx context.Context, params *ConflictsParams) (*ConflictsResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DocudoltConflicts(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DocudoltConflicts")
}

// DocudoltResolveConflict implements VersioningBackend if the wrapped backend supports it.
func (bc *backendContract) DocudoltResolveConflict(ctx context.Context, params *ResolveConflictParams) (*ResolveConflictResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DocudoltResolveConflict(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DocudoltResolveConflict")
}

// newVersioningUnsupportedError returns a standard error for when a versioning operation
// is not supported by the current backend.
func newVersioningUnsupportedError(op string) error {
	return fmt.Errorf("docudolt versioning not supported by this backend: %s", op)
}

// check interfaces
var (
	_ Backend          = (*backendContract)(nil)
	_ VersioningBackend = (*backendContract)(nil)
)
