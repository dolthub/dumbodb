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
	"strings"
	"sync"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/writer"
	"github.com/dolthub/dolt/go/libraries/doltcore/table/editor"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/libraries/utils/keymutex"
	"github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/dumbodb/internal/sqlctx"
)

// dumbodbProvider satisfies dsess.DoltDatabaseProvider for DumboDB.
//
// The interesting work happens in SessionDatabase / BaseDatabase: those
// look up an existing DumboDB dbState via the dbLookup callback and wrap
// it in a sqle.Database (the same Database type dolt's SQL engine uses).
// That gives us free integration with dsess's working-set / table-writer
// machinery -- dsess does not care whether the SqlDatabase came from
// dolt's own provider or ours.
//
// The provider is owned by Backend (one per process); the dbLookup
// callback closes over the Backend's dbs map and the Backend's mu.
type dumbodbProvider struct {
	// Embedding the sql.DatabaseProvider interface gives us a satisfying
	// signature; the methods that dsess actually invokes are overridden
	// below. (Database/HasDatabase/AllDatabases would panic against a nil
	// embedded interface, so they are implemented explicitly here.)
	sql.DatabaseProvider

	dataDir string
	fs      filesys.Filesys
	txLocks keymutex.Keymutex

	// dbLookup resolves a DumboDB database name to its dbState. The
	// provider does not own dbStates; Backend.dbs does. The callback is
	// the boundary that keeps the dsess plumbing out of the rest of the
	// backend.
	dbLookup func(name string) (*dbState, bool)

	// dbCache stores constructed sqle.Database instances keyed by lower-
	// case base name. We cache because sqle.NewDatabase has non-trivial
	// setup (NewGlobalStateStoreForDb) that we don't want to pay on every
	// LookupDbState. Cache entries live as long as the Backend.
	dbCacheMu sync.Mutex
	dbCache   map[string]sqle.Database
}

// Compile-time assertion that dumbodbProvider satisfies the dsess interface.
var _ dsess.DoltDatabaseProvider = (*dumbodbProvider)(nil)

// newDumbodbProvider builds a provider rooted at dataDir. dbLookup is the
// callback used by SessionDatabase / BaseDatabase to resolve DumboDB
// database names to dbStates.
func newDumbodbProvider(dataDir string, dbLookup func(name string) (*dbState, bool)) (*dumbodbProvider, error) {
	fs, err := filesys.LocalFilesysWithWorkingDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("constructing local filesystem for dsess provider: %w", err)
	}
	return &dumbodbProvider{
		dataDir:  dataDir,
		fs:       fs,
		txLocks:  keymutex.NewMapped(),
		dbLookup: dbLookup,
		dbCache:  map[string]sqle.Database{},
	}, nil
}

// sql.DatabaseProvider methods.

func (p *dumbodbProvider) Database(ctx *sql.Context, name string) (sql.Database, error) {
	db, ok, err := p.SessionDatabase(ctx, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, sql.ErrDatabaseNotFound.New(name)
	}
	return db, nil
}

func (p *dumbodbProvider) HasDatabase(_ *sql.Context, name string) bool {
	baseName, _ := doltdb.SplitRevisionDbName(name)
	_, ok := p.dbLookup(baseName)
	return ok
}

func (p *dumbodbProvider) AllDatabases(_ *sql.Context) []sql.Database {
	return nil
}

// sql.MutableDatabaseProvider methods. Creation/drop of DumboDB databases
// flows through the Mongo wire protocol (insert auto-creates), not through
// SQL CREATE DATABASE, so these are no-ops.

func (p *dumbodbProvider) CreateDatabase(_ *sql.Context, _ string) error { return nil }

func (p *dumbodbProvider) DropDatabase(_ *sql.Context, _ string) error { return nil }

// dsess.DoltDatabaseProvider methods.

func (p *dumbodbProvider) FileSystem() filesys.Filesys { return p.fs }

func (p *dumbodbProvider) DbFactoryUrl() string { return "" }

func (p *dumbodbProvider) FileSystemForDatabase(_ string) (filesys.Filesys, error) {
	return p.fs, nil
}

func (p *dumbodbProvider) GetRemoteDB(_ context.Context, _ *types.NomsBinFormat, _ env.Remote, _ bool) (*doltdb.DoltDB, error) {
	return nil, fmt.Errorf("dumbodb provider: GetRemoteDB not supported")
}

func (p *dumbodbProvider) CloneDatabaseFromRemote(_ *sql.Context, _, _, _, _ string, _ int, _ map[string]string) error {
	return fmt.Errorf("dumbodb provider: CloneDatabaseFromRemote not supported")
}

// SessionDatabase resolves a DumboDB database name to a working
// dsess.SqlDatabase. The name may carry an "@branch" revision suffix
// (e.g. "mydb@feat"); the base part identifies the DumboDB dbState and
// the suffix identifies the branch the returned database operates against.
func (p *dumbodbProvider) SessionDatabase(ctx *sql.Context, name string) (dsess.SqlDatabase, bool, error) {
	baseName, _ := doltdb.SplitRevisionDbName(name)
	db, ok, err := p.getOrBuildSqleDatabase(ctx, baseName)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, sql.ErrDatabaseNotFound.New(name)
	}
	// A revision suffix produces a revision database via WithBranchRevision;
	// callers that need that should request it explicitly.
	return db, true, nil
}

func (p *dumbodbProvider) BaseDatabase(ctx *sql.Context, name string) (dsess.SqlDatabase, bool) {
	baseName, _ := doltdb.SplitRevisionDbName(name)
	db, ok, err := p.getOrBuildSqleDatabase(ctx, baseName)
	if err != nil || !ok {
		return nil, false
	}
	return db, true
}

func (p *dumbodbProvider) DoltDatabases() []dsess.SqlDatabase {
	// Snapshot the cache so callers iterating the slice don't race with
	// concurrent inserts.
	p.dbCacheMu.Lock()
	defer p.dbCacheMu.Unlock()
	out := make([]dsess.SqlDatabase, 0, len(p.dbCache))
	for _, db := range p.dbCache {
		out = append(out, db)
	}
	return out
}

func (p *dumbodbProvider) UndropDatabase(_ *sql.Context, _ string) error { return nil }

func (p *dumbodbProvider) ListDroppedDatabases(_ *sql.Context) ([]string, error) {
	return nil, nil
}

func (p *dumbodbProvider) PurgeDroppedDatabases(_ *sql.Context) error { return nil }

func (p *dumbodbProvider) EngineOverrides() sql.EngineOverrides { return sql.EngineOverrides{} }

func (p *dumbodbProvider) TxLocks() keymutex.Keymutex { return p.txLocks }

// getOrBuildSqleDatabase returns the cached sqle.Database for baseName,
// constructing it on first lookup. Returns ok=false if the name is not a
// known DumboDB database -- callers turn that into ErrDatabaseNotFound.
func (p *dumbodbProvider) getOrBuildSqleDatabase(ctx *sql.Context, baseName string) (sqle.Database, bool, error) {
	key := strings.ToLower(baseName)

	p.dbCacheMu.Lock()
	defer p.dbCacheMu.Unlock()

	if db, ok := p.dbCache[key]; ok {
		return db, true, nil
	}

	state, ok := p.dbLookup(baseName)
	if !ok {
		return sqle.Database{}, false, nil
	}

	rsrw := newRepoStateAdapter(defaultBranch)
	dbData := env.DbData[context.Context]{
		Ddb: state.doltDB,
		Rsr: rsrw,
		Rsw: rsrw,
	}
	db, err := sqle.NewDatabase(ctx, baseName, dbData, editor.Options{})
	if err != nil {
		return sqle.Database{}, false, fmt.Errorf("constructing sqle.Database for %q: %w", baseName, err)
	}
	p.dbCache[key] = db
	return db, true, nil
}

// NewSession builds a fresh *dsess.DoltSession against this backend's
// provider via the sqlctx shim. The session's lifecycle is the caller's
// responsibility; this method does not cache.
//
// writer.NewWriteSession is the dolt-canonical WriteSessFunc; it constructs
// a prollyWriteSession that backs dsess.TableWriter for Insert/Update/
// Delete operations.
func (b *Backend) NewSession() *dsess.DoltSession {
	return sqlctx.NewSession(b.provider, writer.NewWriteSession)
}
