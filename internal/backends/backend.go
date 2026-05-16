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

	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/util/must"
	"github.com/dolthub/dumbodb/internal/util/resource"
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

func (bc *backendContract) Close() {
	bc.b.Close()

	resource.Untrack(bc, bc.token)
}

// OnSessionEnd / OnTransactionCommit / OnTransactionAbort delegate to the
// wrapped backend when (and only when) it implements
// SessionAwareBackend. The contract wrapper itself implements
// SessionAwareBackend unconditionally so handlers can type-assert
// against the wrapper -- backends that hold no per-session state get
// no-op delegation.

func (bc *backendContract) OnSessionEnd(owner string) {
	if sab, ok := bc.b.(SessionAwareBackend); ok {
		sab.OnSessionEnd(owner)
	}
}

func (bc *backendContract) OnTransactionCommit(ctx context.Context, owner string) error {
	if sab, ok := bc.b.(SessionAwareBackend); ok {
		return sab.OnTransactionCommit(ctx, owner)
	}
	return nil
}

func (bc *backendContract) OnTransactionAbort(owner string) {
	if sab, ok := bc.b.(SessionAwareBackend); ok {
		sab.OnTransactionAbort(owner)
	}
}

type StatusParams struct{}

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

type ListDatabasesParams struct {
	Name string
}

type ListDatabasesResult struct {
	Databases []DatabaseInfo
}

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

func (bc *backendContract) DumboDBCommit(ctx context.Context, params *CommitParams) (*CommitResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DumboDBCommit(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DumboDBCommit")
}

func (bc *backendContract) DumboDBBranch(ctx context.Context, params *BranchParams) (*BranchResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DumboDBBranch(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DumboDBBranch")
}

func (bc *backendContract) DumboDBMerge(ctx context.Context, params *MergeParams) (*MergeResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DumboDBMerge(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DumboDBMerge")
}

func (bc *backendContract) DumboDBLog(ctx context.Context, params *LogParams) (*LogResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DumboDBLog(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DumboDBLog")
}

func (bc *backendContract) DumboDBStatus(ctx context.Context, params *VersioningStatusParams) (*VersioningStatusResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DumboDBStatus(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DumboDBStatus")
}

func (bc *backendContract) DumboDBDiff(ctx context.Context, params *DiffParams) (*DiffResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DumboDBDiff(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DumboDBDiff")
}

func (bc *backendContract) DumboDBReset(ctx context.Context, params *ResetParams) (*ResetResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DumboDBReset(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DumboDBReset")
}

func (bc *backendContract) DumboDBConflicts(ctx context.Context, params *ConflictsParams) (*ConflictsResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DumboDBConflicts(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DumboDBConflicts")
}

func (bc *backendContract) DumboDBResolveConflict(ctx context.Context, params *ResolveConflictParams) (*ResolveConflictResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DumboDBResolveConflict(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DumboDBResolveConflict")
}

func (bc *backendContract) DumboDBCherryPick(ctx context.Context, params *CherryPickParams) (*CherryPickResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DumboDBCherryPick(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DumboDBCherryPick")
}

func (bc *backendContract) DumboDBRebase(ctx context.Context, params *RebaseParams) (*RebaseResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DumboDBRebase(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DumboDBRebase")
}

func (bc *backendContract) DumboDBRevert(ctx context.Context, params *RevertParams) (*RevertResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DumboDBRevert(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DumboDBRevert")
}

func (bc *backendContract) DumboDBTag(ctx context.Context, params *TagParams) (*TagResult, error) {
	if vb, ok := bc.b.(VersioningBackend); ok {
		return vb.DumboDBTag(ctx, params)
	}

	return nil, newVersioningUnsupportedError("DumboDBTag")
}

func newVersioningUnsupportedError(op string) error {
	return fmt.Errorf("dolt versioning not supported by this backend: %s", op)
}

var (
	_ Backend           = (*backendContract)(nil)
	_ VersioningBackend = (*backendContract)(nil)
)
