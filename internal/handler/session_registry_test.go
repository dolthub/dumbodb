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

func TestHandler_SessionRegistry_RoutesThroughWrappers(t *testing.T) {
	be, err := dolt.NewBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	require.NoError(t, err)
	defer be.Close() //nolint:errcheck

	h := &Handler{NewOpts: &NewOpts{Backend: be, L: slog.New(slog.NewTextHandler(io.Discard, nil))}, b: be}

	reg := h.SessionRegistry()
	require.NotNil(t, reg)

	shadow, err := reg.Connect("test-lsid")
	require.NoError(t, err)
	got, ok := reg.Get("test-lsid")
	require.True(t, ok)
	assert.Same(t, shadow, got)
}
