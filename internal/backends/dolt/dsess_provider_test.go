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
	"testing"

	"github.com/dolthub/go-mysql-server/sql"
	_ "github.com/dolthub/go-mysql-server/sql/variables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/sqlctx"
)

// TestBackendNewSessionConstructs verifies that the Backend can produce a
// *dsess.DoltSession via the sqlctx shim. This is the .2.2 acceptance:
// "backend wires DoltSession via the sqlctx shim".
//
// The test bypasses NewBackend (which spins up a flush loop and an admin
// database) because the provider+NewSession path is independent of that
// machinery and we don't want unrelated startup work in a unit test.
func TestBackendNewSessionConstructs(t *testing.T) {
	provider, err := newDumbodbProvider(t.TempDir())
	require.NoError(t, err)
	b := &Backend{provider: provider}

	sess := b.NewSession()
	require.NotNil(t, sess)

	// Sanity: the session is a usable sql.Session.
	var _ sql.Session = sess

	// LookupDbState on an unknown db must propagate ErrDatabaseNotFound,
	// proving the call path threads through the provider correctly. The
	// real provider implementation lands in subsequent beads -- here we
	// only need to confirm the wiring is in place.
	sqlCtx := sqlctx.Wrap(context.Background(), sess)
	state, ok, err := sess.LookupDbState(sqlCtx, "nonexistent")
	require.Error(t, err)
	assert.True(t, sql.ErrDatabaseNotFound.Is(err), "expected ErrDatabaseNotFound, got %v", err)
	assert.False(t, ok)
	assert.Nil(t, state)
}

// TestProviderSurface verifies the parts of the provider that have real
// implementations (rather than not-found stubs) return sensible values.
// FileSystem and TxLocks back the lifecycle pieces that dsess uses
// internally, so they need to be non-nil even before SessionDatabase et al
// are filled in.
func TestProviderSurface(t *testing.T) {
	provider, err := newDumbodbProvider(t.TempDir())
	require.NoError(t, err)

	require.NotNil(t, provider.FileSystem(), "FileSystem must not be nil; dsess accesses it during construction")
	require.NotNil(t, provider.TxLocks(), "TxLocks must not be nil; dsess uses it to serialize transaction commits")
}
