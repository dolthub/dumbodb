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
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedPreservedEntry writes a fake preserved drop for name whose dropId
// encodes droppedAt (mirroring how DropDatabase names the directory). Returns
// the dropId. This is the test seam: age is derived from the dropId, so an old
// drop is simply one with an old timestamp in its directory name.
func seedPreservedEntry(t *testing.T, b *Backend, name string, droppedAt time.Time) string {
	t.Helper()
	dropID := strconv.FormatInt(droppedAt.UnixNano(), 10)
	dir := filepath.Join(b.preservedRoot(), name, dropID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o644))
	return dropID
}

func newTestBackendWithLog(t *testing.T) (*Backend, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	be, err := newBackend(t.TempDir(),
		slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)
	return be, &logBuf
}

func TestPurgeExpiredDroppedDatabases(t *testing.T) {
	be, logBuf := newTestBackendWithLog(t)

	now := time.Now()
	ttl := defaultDroppedDatabaseTTL

	oldID := seedPreservedEntry(t, be, "olddb", now.Add(-40*24*time.Hour))    // expired
	freshID := seedPreservedEntry(t, be, "freshdb", now.Add(-5*24*time.Hour)) // kept
	boundaryID := seedPreservedEntry(t, be, "boundarydb", now.Add(-ttl))      // exactly TTL: kept

	purged, err := be.purgeExpiredDroppedDatabases(now, ttl)
	require.NoError(t, err)

	// Only the expired drop is purged.
	require.Len(t, purged, 1)
	assert.Equal(t, "olddb", purged[0].Name)
	assert.Equal(t, oldID, purged[0].DropID)

	// Expired drop is gone on disk, and its now-empty parent dir is removed.
	_, statErr := os.Stat(filepath.Join(be.preservedRoot(), "olddb"))
	assert.True(t, os.IsNotExist(statErr), "expired drop's parent dir should be removed")

	// Fresh and exactly-TTL drops survive.
	_, statErr = os.Stat(filepath.Join(be.preservedRoot(), "freshdb", freshID))
	assert.NoError(t, statErr, "fresh drop must survive")
	_, statErr = os.Stat(filepath.Join(be.preservedRoot(), "boundarydb", boundaryID))
	assert.NoError(t, statErr, "drop aged exactly TTL must survive (not strictly older)")

	// An INFO line is logged for the purge.
	logs := logBuf.String()
	assert.Contains(t, logs, "level=INFO")
	assert.Contains(t, logs, "purged expired dropped database")
	assert.Contains(t, logs, "olddb")

	// The listing reflects the removal.
	res, err := be.ListDroppedDatabases(context.Background())
	require.NoError(t, err)
	names := map[string]bool{}
	for _, d := range res.Databases {
		names[d.Name] = true
	}
	assert.False(t, names["olddb"], "purged db must not be listed")
	assert.True(t, names["freshdb"])
	assert.True(t, names["boundarydb"])
}

func TestPurgeExpiredDroppedDatabases_KeepsFresherDropOfSameName(t *testing.T) {
	be, _ := newTestBackendWithLog(t)

	now := time.Now()
	oldID := seedPreservedEntry(t, be, "ledger", now.Add(-40*24*time.Hour))
	newID := seedPreservedEntry(t, be, "ledger", now.Add(-1*24*time.Hour))

	purged, err := be.purgeExpiredDroppedDatabases(now, defaultDroppedDatabaseTTL)
	require.NoError(t, err)

	require.Len(t, purged, 1)
	assert.Equal(t, oldID, purged[0].DropID, "only the old drop of the name is purged")

	_, statErr := os.Stat(filepath.Join(be.preservedRoot(), "ledger", oldID))
	assert.True(t, os.IsNotExist(statErr), "old drop removed")
	_, statErr = os.Stat(filepath.Join(be.preservedRoot(), "ledger", newID))
	assert.NoError(t, statErr, "fresh drop of same name kept")
	_, statErr = os.Stat(filepath.Join(be.preservedRoot(), "ledger"))
	assert.NoError(t, statErr, "parent dir kept because a fresh drop remains")
}

func TestPurgeExpiredDroppedDatabases_NothingExpired(t *testing.T) {
	be, logBuf := newTestBackendWithLog(t)

	now := time.Now()
	seedPreservedEntry(t, be, "a", now.Add(-1*time.Hour))
	seedPreservedEntry(t, be, "b", now.Add(-10*24*time.Hour))

	purged, err := be.purgeExpiredDroppedDatabases(now, defaultDroppedDatabaseTTL)
	require.NoError(t, err)
	assert.Empty(t, purged, "no drops old enough to purge")
	assert.NotContains(t, logBuf.String(), "purged expired dropped database")
}

func TestPurgeExpiredDroppedDatabases_EmptyPreservedDir(t *testing.T) {
	be, _ := newTestBackendWithLog(t)
	purged, err := be.purgeExpiredDroppedDatabases(time.Now(), defaultDroppedDatabaseTTL)
	require.NoError(t, err)
	assert.Empty(t, purged)
}

// TestDroppedDatabaseGCLoop_RunsPeriodically exercises the ticker loop itself
// (the part not covered by calling purgeExpiredDroppedDatabases directly) using
// a fast tick, so the timer wiring is not dark code in CI.
func TestDroppedDatabaseGCLoop_RunsPeriodically(t *testing.T) {
	be := &Backend{
		dataDir:         t.TempDir(),
		l:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		dbs:             map[string]*dbState{},
		droppedGCStop:   make(chan struct{}),
		droppedGCDone:   make(chan struct{}),
		droppedGCPeriod: 10 * time.Millisecond,
		droppedGCTTL:    defaultDroppedDatabaseTTL,
	}
	seedPreservedEntry(t, be, "olddb", time.Now().Add(-40*24*time.Hour))

	go be.droppedDatabaseGCLoop()
	t.Cleanup(func() {
		close(be.droppedGCStop)
		<-be.droppedGCDone
	})

	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(be.preservedRoot(), "olddb"))
		return os.IsNotExist(err)
	}, 2*time.Second, 10*time.Millisecond, "the GC loop should purge the expired drop on a tick")
}
