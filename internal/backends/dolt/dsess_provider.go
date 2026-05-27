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
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/gcctx"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle"
	"github.com/dolthub/dolt/go/libraries/doltcore/dsess"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/writer"
	"github.com/dolthub/dolt/go/libraries/doltcore/table/editor"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/libraries/utils/keymutex"
	"github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/dumbodb/internal/sqlctx"
)

type dumbodbProvider struct {
	// Embedded for the interface check; dsess only calls the methods we
	// explicitly define below, so the nil interface value is never invoked.
	sql.DatabaseProvider

	dataDir string
	fs      filesys.Filesys
	txLocks keymutex.Keymutex

	dbLookup     func(name string) (*dbState, bool)
	dbNames      func() []string
	gcController *gcctx.GCSafepointController

	dbCacheMu sync.Mutex
	dbCache   map[string]sqle.Database
}

var _ dsess.DoltDatabaseProvider = (*dumbodbProvider)(nil)

func newDumbodbProvider(dataDir string, dbLookup func(name string) (*dbState, bool), dbNames func() []string, gc *gcctx.GCSafepointController) (*dumbodbProvider, error) {
	fs, err := filesys.LocalFilesysWithWorkingDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("constructing local filesystem for dsess provider: %w", err)
	}
	return &dumbodbProvider{
		dataDir:      dataDir,
		gcController: gc,
		fs:       fs,
		txLocks:  keymutex.NewMapped(),
		dbLookup: dbLookup,
		dbNames:  dbNames,
		dbCache:  map[string]sqle.Database{},
	}, nil
}

func (p *dumbodbProvider) Database(ctx *sql.Context, name string) (sql.Database, error) {
	baseName, _ := doltdb.SplitRevisionDbName(name)
	db, ok, err := p.getOrBuildSqleDatabase(ctx, baseName)
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

func (p *dumbodbProvider) CreateDatabase(_ *sql.Context, _ string) error { return nil }

func (p *dumbodbProvider) DropDatabase(_ *sql.Context, _ string) error { return nil }

func (p *dumbodbProvider) FileSystem() filesys.Filesys { return p.fs }

func (p *dumbodbProvider) DbFactoryUrl() string { return "" }

func (p *dumbodbProvider) FileSystemForDatabase(_ string) (filesys.Filesys, error) {
	return p.fs, nil
}

func (p *dumbodbProvider) GetRemoteDB(_ context.Context, _ *types.NomsBinFormat, _ env.Remote) (*doltdb.DoltDB, error) {
	return nil, fmt.Errorf("dumbodb provider: GetRemoteDB not supported")
}

func (p *dumbodbProvider) CloneDatabaseFromRemote(_ *sql.Context, _, _, _, _ string, _ int, _ map[string]string) error {
	return fmt.Errorf("dumbodb provider: CloneDatabaseFromRemote not supported")
}

func (p *dumbodbProvider) SessionDatabase(ctx *sql.Context, name string) (dsess.VersionedDatabase, bool, error) {
	baseName, rev := doltdb.SplitRevisionDbName(name)
	if rev == "" {
		rev = defaultBranch
	}
	db, ok, err := p.getOrBuildSqleDatabase(ctx, baseName)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, sql.ErrDatabaseNotFound.New(name)
	}
	// dsess keys branchStates by db.Revision(); the base db has Revision == ""
	// which collapses every branch under one empty-key bucket. Set it via
	// WithBranchRevision so each (db, branch) maps to a distinct branchState.
	revDb, err := db.WithBranchRevision(doltdb.RevisionDbName(baseName, rev), dsess.SessionDatabaseBranchSpec{
		RepoState: newSqlCtxRepoStateAdapter(rev),
		Branch:    rev,
	})
	if err != nil {
		return nil, false, fmt.Errorf("dumbodb provider: WithBranchRevision for %q: %w", name, err)
	}
	return revDb, true, nil
}

func (p *dumbodbProvider) BaseDatabase(ctx *sql.Context, name string) (dsess.VersionedDatabase, bool) {
	// BaseDatabase returns the unqualified base, NOT a WithBranchRevision'd
	// copy. dsess.AddDb (transactions.go) keys dbStartPoints by db.Name()
	// without stripping revision; using the base name keeps the key
	// symmetrical with NewDoltTransaction's baseName-keyed startPoints.
	baseName, _ := doltdb.SplitRevisionDbName(name)
	db, ok, err := p.getOrBuildSqleDatabase(ctx, baseName)
	if err != nil || !ok {
		return nil, false
	}
	return db, true
}

func (p *dumbodbProvider) DoltDatabases() []dsess.VersionedDatabase {
	// dsess uses this at StartTransaction time to snapshot dbStartPoints
	// for every db under management. The dbCache only has entries that have
	// already been requested via SessionDatabase, so we walk the backend's
	// open db list to ensure dsess sees every database. The interface gives
	// us no sql.Context; sqle.NewDatabase reaches into the GC safepoint
	// controller via the context, so we embed it directly via gcctx rather
	// than synthesise a *sql.Context with no session.
	names := p.dbNames()
	out := make([]dsess.VersionedDatabase, 0, len(names))
	for _, name := range names {
		db, ok, err := p.buildSqleDatabaseNoSession(name)
		if err != nil || !ok {
			continue
		}
		out = append(out, db)
	}
	return out
}

// buildSqleDatabaseNoSession is the session-less variant of
// getOrBuildSqleDatabase used by DoltDatabases. It threads the backend's
// GCSafepointController into a context.Background so sqle.NewDatabase ->
// dsess.NewGlobalStateStoreForDb -> dsess.NewAutoIncrementTracker can
// resolve the GC controller via gcctx.GetGCSafepointController instead of
// dereferencing a *DoltSession that doesn't exist yet.
func (p *dumbodbProvider) buildSqleDatabaseNoSession(baseName string) (sqle.Database, bool, error) {
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
	ctx := gcctx.WithGCSafepointController(context.Background(), p.gcController)
	db, err := sqle.NewDatabase(ctx, baseName, dbData, editor.Options{})
	if err != nil {
		return sqle.Database{}, false, fmt.Errorf("constructing sqle.Database for %q (session-less): %w", baseName, err)
	}
	p.dbCache[key] = db
	return db, true, nil
}

func (p *dumbodbProvider) UndropDatabase(_ *sql.Context, _ string) error { return nil }

func (p *dumbodbProvider) ListDroppedDatabases(_ *sql.Context) ([]string, error) {
	return nil, nil
}

func (p *dumbodbProvider) PurgeDroppedDatabases(_ *sql.Context) error { return nil }

func (p *dumbodbProvider) EngineOverrides() sql.EngineOverrides { return sql.EngineOverrides{} }

func (p *dumbodbProvider) TxLocks() keymutex.Keymutex { return p.txLocks }

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

// writer.NewWriteSession is required, not optional: dsess.addDB invokes the
// WriteSessFunc when first materializing a branchState and nil-derefs without
// it.
func (b *Backend) NewSession() *dsess.DoltSession {
	return sqlctx.NewSession(b.provider, writer.NewWriteSession)
}

func (b *Backend) SessionRegistry() *sqlctx.SessionRegistry {
	return b.sessions
}

func (b *Backend) GCSafepointController() *gcctx.GCSafepointController {
	return b.gcController
}
