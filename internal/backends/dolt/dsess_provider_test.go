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
	"io"
	"log/slog"
	"testing"

	"github.com/dolthub/go-mysql-server/sql"
	_ "github.com/dolthub/go-mysql-server/sql/variables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/sqlctx"
)

func TestBackendNewSessionConstructs(t *testing.T) {
	provider, err := newDumbodbProvider(t.TempDir(), func(string) (*dbState, bool) { return nil, false })
	require.NoError(t, err)
	b := &Backend{provider: provider}

	sess := b.NewSession()
	require.NotNil(t, sess)

	var _ sql.Session = sess

	sqlCtx := sqlctx.Wrap(context.Background(), sess)
	state, ok, err := sess.LookupDbState(sqlCtx, "nonexistent")
	require.Error(t, err)
	assert.True(t, sql.ErrDatabaseNotFound.Is(err), "expected ErrDatabaseNotFound, got %v", err)
	assert.False(t, ok)
	assert.Nil(t, state)
}

func TestSessionLookupDbStateResolvesRealDb(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	sess := be.NewSession()
	sqlCtx := sqlctx.Wrap(context.Background(), sess)

	state, found, err := sess.LookupDbState(sqlCtx, "admin")
	require.NoError(t, err)
	require.True(t, found, "admin database should be resolvable via LookupDbState")
	require.NotNil(t, state)
	require.NotNil(t, state.WorkingRoot(), "branchState must have a working root")
}

func TestProviderSurface(t *testing.T) {
	provider, err := newDumbodbProvider(t.TempDir(), func(string) (*dbState, bool) { return nil, false })
	require.NoError(t, err)

	require.NotNil(t, provider.FileSystem(), "FileSystem must not be nil; dsess accesses it during construction")
	require.NotNil(t, provider.TxLocks(), "TxLocks must not be nil; dsess uses it to serialize transaction commits")
}
