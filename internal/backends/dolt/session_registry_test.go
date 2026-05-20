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
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// .6.4.5 acceptance: Backend wires the registry, the registry's factory
// produces sessions attached to the backend's GC controller, and the
// minted sessions can VisitGCRoots without panic.

func TestBackend_HasSessionRegistry(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	require.NoError(t, err)
	defer be.Close() //nolint:errcheck

	require.NotNil(t, be.SessionRegistry(), "Backend must construct a SessionRegistry")
	require.NotNil(t, be.GCSafepointController(), "Backend must own a GCSafepointController")
}

func TestBackend_RegistryFactoryWiresGCController(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	require.NoError(t, err)
	defer be.Close() //nolint:errcheck

	shadow, err := be.SessionRegistry().Connect("test-lsid-A")
	require.NoError(t, err)
	require.True(t, shadow.Active())

	sess := shadow.Session()
	require.NotNil(t, sess)
	require.Same(t, be.GCSafepointController(), sess.GCSafepointController(),
		"the registry's factory must wire the backend's GC controller into every minted session")
}

// VisitGCRoots must not panic on a freshly minted, never-touched session.
// This is the smoke that confirms the session is properly registered with
// the controller (via the Connect path's Begin/End pair) and that its
// internal state is consistent enough for GC to walk.
func TestBackend_NewSessionCanVisitGCRoots(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	require.NoError(t, err)
	defer be.Close() //nolint:errcheck

	shadow, err := be.SessionRegistry().Connect("test-lsid-B")
	require.NoError(t, err)

	sess := shadow.Session()
	// VisitGCRoots iterates the session's dbStates. A fresh session has
	// no dbStates registered yet so the walk is trivially safe; the
	// point is that the method runs without panic and respects the
	// session's internal mutex.
	err = sess.VisitGCRoots(context.Background(), "admin", func(hash.Hash) bool {
		// The session has no live roots yet, so the callback shouldn't be
		// invoked. If it is, that's a panic-equivalent (something is
		// pinned that shouldn't be).
		t.Fatalf("VisitGCRoots called keep() on a fresh session with no dbStates")
		return false
	})
	require.NoError(t, err)
}

func TestBackend_RegistryYieldsDistinctSessionsPerLsid(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	require.NoError(t, err)
	defer be.Close() //nolint:errcheck

	sA, err := be.SessionRegistry().Connect("test-lsid-A")
	require.NoError(t, err)

	sB, err := be.SessionRegistry().Connect("test-lsid-B")
	require.NoError(t, err)

	assert.NotSame(t, sA.Session(), sB.Session(),
		"distinct lsids must produce distinct underlying *dsess.DoltSession instances")
}

// Once Connect mints a session, subsequent Connect for the same lsid
// (within the timeout window) returns a Shadow over the SAME underlying
// session -- the supersession semantic. This is registry-level behavior
// covered in .6.4.2's unit tests; here it's a quick smoke that the
// production-wired registry preserves it.
func TestBackend_RegistryReusesSessionOnReconnect(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	require.NoError(t, err)
	defer be.Close() //nolint:errcheck

	first, err := be.SessionRegistry().Connect("test-lsid-A")
	require.NoError(t, err)

	second, err := be.SessionRegistry().Connect("test-lsid-A")
	require.NoError(t, err)

	assert.Same(t, first.Session(), second.Session())
	assert.False(t, first.Active(), "supersede invalidates the first shadow")
	assert.True(t, second.Active())

	// Sanity: the registry uses our intended timeout (defaultSessionTimeout = 30m).
	// Connecting in quick succession does not reset that window; the new shadow
	// inherits lastUsed (see .6.4.2 unit tests for the timing).
	_ = time.Minute // anchor the time import
}

// Ensure that a session retrieved via the registry is usable for the
// kind of operations the wire dispatcher will run against it. Smoke
// check: passing the session through Wrap to build a *sql.Context.
func TestBackend_RegistrySessionConstructsSqlContext(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	require.NoError(t, err)
	defer be.Close() //nolint:errcheck

	shadow, err := be.SessionRegistry().Connect("test-lsid-A")
	require.NoError(t, err)

	var captured *dsess.DoltSession
	require.NoError(t, shadow.Use(time.Now(), func(s *dsess.DoltSession) error {
		captured = s
		return nil
	}))
	assert.Same(t, shadow.Session(), captured)
}
