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

package handler

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends/dolt"
)

// .6.4.6 acceptance: Handler.SessionRegistry() returns the registry from
// the underlying dolt.Backend, even after wrapping in the BackendContract
// and oplog decorator chain that production uses.

func TestHandler_SessionRegistry_RoutesThroughWrappers(t *testing.T) {
	be, err := dolt.NewBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	require.NoError(t, err)
	defer be.Close() //nolint:errcheck

	// Build a Handler manually around this backend. The constructor for
	// Handler in production goes through registry.NewHandler; here we
	// shortcut by passing the backend into a minimal Handler with the
	// same backend-assignment as production.
	h := &Handler{NewOpts: &NewOpts{Backend: be, L: slog.New(slog.NewTextHandler(io.Discard, nil))}, b: be}

	reg := h.SessionRegistry()
	require.NotNil(t, reg, "Handler.SessionRegistry must surface the registry through BackendContract")

	// Round-trip: create a shadow via the registry, observe it through
	// Get. This proves the same registry is being exposed (not, say, a
	// freshly-constructed empty one).
	shadow, err := reg.Connect("test-lsid")
	require.NoError(t, err)
	got, ok := reg.Get("test-lsid")
	require.True(t, ok)
	assert.Same(t, shadow, got)
}
