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

	"github.com/dolthub/dolt/go/libraries/doltcore/dsess"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackend_HasSessionRegistry(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	defer be.Close() //nolint:errcheck

	require.NotNil(t, be.SessionRegistry())
	require.NotNil(t, be.GCSafepointController())
}

func TestBackend_RegistryFactoryWiresGCController(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	defer be.Close() //nolint:errcheck

	shadow, err := be.SessionRegistry().Connect("test-lsid-A")
	require.NoError(t, err)
	require.True(t, shadow.Active())

	sess := shadow.Session()
	require.NotNil(t, sess)
	require.Same(t, be.GCSafepointController(), sess.GCSafepointController())
}

func TestBackend_NewSessionCanVisitGCRoots(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	defer be.Close() //nolint:errcheck

	shadow, err := be.SessionRegistry().Connect("test-lsid-B")
	require.NoError(t, err)

	sess := shadow.Session()
	err = sess.VisitGCRoots(context.Background(), "admin", func(hash.Hash) bool {
		t.Fatalf("VisitGCRoots called keep() on a fresh session with no dbStates")
		return false
	})
	require.NoError(t, err)
}

func TestBackend_RegistryYieldsDistinctSessionsPerLsid(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	defer be.Close() //nolint:errcheck

	sA, err := be.SessionRegistry().Connect("test-lsid-A")
	require.NoError(t, err)

	sB, err := be.SessionRegistry().Connect("test-lsid-B")
	require.NoError(t, err)

	assert.NotSame(t, sA.Session(), sB.Session())
}

func TestBackend_RegistryReusesSessionOnReconnect(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	defer be.Close() //nolint:errcheck

	first, err := be.SessionRegistry().Connect("test-lsid-A")
	require.NoError(t, err)

	second, err := be.SessionRegistry().Connect("test-lsid-A")
	require.NoError(t, err)

	assert.Same(t, first.Session(), second.Session())
	assert.False(t, first.Active())
	assert.True(t, second.Active())
}

func TestBackend_RegistrySessionConstructsSqlContext(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
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
