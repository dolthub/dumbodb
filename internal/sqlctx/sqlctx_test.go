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

package sqlctx

import (
	"context"
	"testing"

	"github.com/dolthub/go-mysql-server/sql"
	_ "github.com/dolthub/go-mysql-server/sql/variables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/libraries/utils/keymutex"
	"github.com/dolthub/dolt/go/store/types"
)

// stubProvider satisfies dsess.DoltDatabaseProvider with no databases. Models
// dolt's own emptyRevisionDatabaseProvider; only enough is implemented to let
// DefaultSession construct and LookupDbState return a clean not-found.
type stubProvider struct {
	sql.DatabaseProvider
}

func (stubProvider) DbFactoryUrl() string                                { return "" }
func (stubProvider) UndropDatabase(*sql.Context, string) error           { return nil }
func (stubProvider) ListDroppedDatabases(*sql.Context) ([]string, error) { return nil, nil }
func (stubProvider) PurgeDroppedDatabases(*sql.Context) error            { return nil }
func (stubProvider) BaseDatabase(*sql.Context, string) (dsess.SqlDatabase, bool) {
	return nil, false
}
func (stubProvider) SessionDatabase(_ *sql.Context, dbName string) (dsess.SqlDatabase, bool, error) {
	return nil, false, sql.ErrDatabaseNotFound.New(dbName)
}
func (stubProvider) DoltDatabases() []dsess.SqlDatabase { return nil }
func (stubProvider) DbState(_ *sql.Context, dbName string, _ string) (dsess.InitialDbState, error) {
	return dsess.InitialDbState{}, sql.ErrDatabaseNotFound.New(dbName)
}
func (stubProvider) DropDatabase(*sql.Context, string) error { return nil }
func (stubProvider) GetRevisionForRevisionDatabase(*sql.Context, string) (string, string, error) {
	return "", "", nil
}
func (stubProvider) IsRevisionDatabase(*sql.Context, string) (bool, error) { return false, nil }
func (stubProvider) GetRemoteDB(context.Context, *types.NomsBinFormat, env.Remote, bool) (*doltdb.DoltDB, error) {
	return nil, nil
}
func (stubProvider) FileSystem() filesys.Filesys { return nil }
func (stubProvider) FileSystemForDatabase(string) (filesys.Filesys, error) {
	return nil, nil
}
func (stubProvider) CloneDatabaseFromRemote(*sql.Context, string, string, string, string, int, map[string]string) error {
	return nil
}
func (stubProvider) CreateDatabase(*sql.Context, string) error { return nil }
func (stubProvider) RevisionDbState(_ *sql.Context, revDB string) (dsess.InitialDbState, error) {
	return dsess.InitialDbState{}, sql.ErrDatabaseNotFound.New(revDB)
}
func (stubProvider) EngineOverrides() sql.EngineOverrides { return sql.EngineOverrides{} }
func (stubProvider) TxLocks() keymutex.Keymutex           { return keymutex.NewMapped() }

// TestNewSessionConstructs verifies that NewSession returns a valid DoltSession
// from a stub provider. This is the minimum integration boundary: dsess is
// importable, can be constructed, and produces a sql.Session-compatible value.
func TestNewSessionConstructs(t *testing.T) {
	sess := NewSession(stubProvider{}, nil)
	require.NotNil(t, sess)

	var _ sql.Session = sess
}

// TestWrapProducesContext verifies that Wrap produces a *sql.Context whose
// Session is the supplied DoltSession.
func TestWrapProducesContext(t *testing.T) {
	sess := NewSession(stubProvider{}, nil)
	sqlCtx := Wrap(context.Background(), sess)
	require.NotNil(t, sqlCtx)
	require.NotNil(t, sqlCtx.Session)
	require.Same(t, sess, dsess.DSessFromSess(sqlCtx.Session))
}

// TestNewOneShotSmokeLookupDbState exercises a smoke call into
// dsess.LookupDbState through the shim. The stub provider has no databases,
// so the call should propagate sql.ErrDatabaseNotFound -- proving the call
// path threads correctly into dsess. Once a real backend adapter lands
// (.2.2), an integration test in that package will exercise the same path
// against a populated provider and read an actual working set.
func TestNewOneShotSmokeLookupDbState(t *testing.T) {
	sqlCtx, sess := New(context.Background(), stubProvider{}, nil)
	require.NotNil(t, sqlCtx)
	require.NotNil(t, sess)

	state, ok, err := sess.LookupDbState(sqlCtx, "nonexistent")
	require.Error(t, err)
	assert.True(t, sql.ErrDatabaseNotFound.Is(err), "expected ErrDatabaseNotFound, got %v", err)
	assert.False(t, ok)
	assert.Nil(t, state)
}
