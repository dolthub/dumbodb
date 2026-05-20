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
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
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

	dbLookup func(name string) (*dbState, bool)

	dbCacheMu sync.Mutex
	dbCache   map[string]sqle.Database
}

var _ dsess.DoltDatabaseProvider = (*dumbodbProvider)(nil)

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

func (p *dumbodbProvider) SessionDatabase(ctx *sql.Context, name string) (dsess.SqlDatabase, bool, error) {
	baseName, _ := doltdb.SplitRevisionDbName(name)
	db, ok, err := p.getOrBuildSqleDatabase(ctx, baseName)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, sql.ErrDatabaseNotFound.New(name)
	}
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

// SessionRegistry returns the lsid-keyed session registry owned by this
// Backend. Wire-dispatch routes every command through it (Phase B,
// arriving in .6.4.8).
func (b *Backend) SessionRegistry() *sqlctx.SessionRegistry {
	return b.sessions
}

// GCSafepointController returns the controller that the SessionRegistry
// factory wires into every minted DoltSession. Exposed for tests and for
// future GC-driven entry points.
func (b *Backend) GCSafepointController() *gcctx.GCSafepointController {
	return b.gcController
}
