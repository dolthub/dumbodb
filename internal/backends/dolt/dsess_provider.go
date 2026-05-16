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

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/libraries/utils/keymutex"
	"github.com/dolthub/dolt/go/store/types"

	"github.com/dolthub/dumbodb/internal/sqlctx"
)

// dumbodbProvider is the minimal dsess.DoltDatabaseProvider needed to
// construct a *dsess.DoltSession backed by DumboDB's storage.
//
// Phase 1 scope: most methods return ErrDatabaseNotFound / no-ops. They
// exist to satisfy the interface so DefaultSession can be constructed and
// LookupDbState can be called with the expected not-found semantics. Real
// implementations of SessionDatabase / BaseDatabase / DbState arrive in
// later beads (.2.4 and onward) when the Mongo write paths actually need
// dsess to resolve a SqlDatabase.
//
// The provider is owned by Backend (one per process). Per-session state
// lives in dsess.DoltSession, not here.
type dumbodbProvider struct {
	// Embedding the sql.DatabaseProvider interface gives us nil-method-set
	// behavior for sql.DatabaseProvider methods that aren't overridden below.
	// Calls to Database/HasDatabase/AllDatabases would panic against a nil
	// interface, so they are implemented explicitly here.
	sql.DatabaseProvider

	dataDir string
	fs      filesys.Filesys
	txLocks keymutex.Keymutex
}

// Compile-time assertion that dumbodbProvider satisfies the dsess interface.
var _ dsess.DoltDatabaseProvider = (*dumbodbProvider)(nil)

// newDumbodbProvider builds a provider rooted at the given data directory.
// The directory is expected to exist (Backend.NewBackend already ensures it).
func newDumbodbProvider(dataDir string) (*dumbodbProvider, error) {
	fs, err := filesys.LocalFilesysWithWorkingDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("constructing local filesystem for dsess provider: %w", err)
	}
	return &dumbodbProvider{
		dataDir: dataDir,
		fs:      fs,
		txLocks: keymutex.NewMapped(),
	}, nil
}

// sql.DatabaseProvider methods -- Phase 1 returns not-found everywhere.

func (p *dumbodbProvider) Database(_ *sql.Context, name string) (sql.Database, error) {
	return nil, sql.ErrDatabaseNotFound.New(name)
}

func (p *dumbodbProvider) HasDatabase(_ *sql.Context, _ string) bool { return false }

func (p *dumbodbProvider) AllDatabases(_ *sql.Context) []sql.Database { return nil }

// sql.MutableDatabaseProvider methods.

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

func (p *dumbodbProvider) SessionDatabase(_ *sql.Context, name string) (dsess.SqlDatabase, bool, error) {
	return nil, false, sql.ErrDatabaseNotFound.New(name)
}

func (p *dumbodbProvider) BaseDatabase(_ *sql.Context, _ string) (dsess.SqlDatabase, bool) {
	return nil, false
}

func (p *dumbodbProvider) DoltDatabases() []dsess.SqlDatabase { return nil }

func (p *dumbodbProvider) UndropDatabase(_ *sql.Context, _ string) error { return nil }

func (p *dumbodbProvider) ListDroppedDatabases(_ *sql.Context) ([]string, error) {
	return nil, nil
}

func (p *dumbodbProvider) PurgeDroppedDatabases(_ *sql.Context) error { return nil }

func (p *dumbodbProvider) EngineOverrides() sql.EngineOverrides { return sql.EngineOverrides{} }

func (p *dumbodbProvider) TxLocks() keymutex.Keymutex { return p.txLocks }

// NewSession builds a fresh *dsess.DoltSession against this backend's
// provider via the sqlctx shim. The session's lifecycle is the caller's
// responsibility; this method does not cache.
//
// Phase 1 stops here: the session is constructable, but the Mongo write
// paths still go through dbState.workingSets directly. Phase 1 sub-bead
// .2.4 routes Insert/Update/DeleteAll through dsess.WriteSession, at which
// point per-connection caching of this session becomes load-bearing and
// gets added.
func (b *Backend) NewSession() *dsess.DoltSession {
	return sqlctx.NewSession(b.provider, nil)
}
