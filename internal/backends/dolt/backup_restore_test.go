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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
)

func restoreParams(url, as string, force bool) *backends.BackupParams {
	return &backends.BackupParams{Action: "restore", URL: url, As: as, Force: force}
}

// syncedBackup builds mydb with a committed doc, a feature branch with its own
// commit, a tag and an uncommitted doc on main, syncs it to a new file backup
// and returns the backup URL.
func syncedBackup(t *testing.T, b *Backend) string {
	t.Helper()
	insertDoc(t, b, "mydb", "coll", mustDoc(t, "_id", int64(1)))
	c1 := commitDB(t, b, "mydb", "c1")
	makeTag(t, b, "mydb", "v1", c1)
	bsBranch(t, b, "mydb", "main", "feature")
	insertDoc(t, b, "mydb@feature", "coll", mustDoc(t, "_id", int64(10)))
	commitBranch(t, b, "mydb", "feature", "feature c1")
	insertDoc(t, b, "mydb", "coll", mustDoc(t, "_id", int64(2)))

	url := "file://" + filepath.Join(t.TempDir(), "bk")
	_, err := b.DumboDBBackup(context.Background(), &backends.BackupParams{DBName: "mydb", Action: "sync-url", URL: url})
	require.NoError(t, err)
	return url
}

func requireNoStagingDirs(t *testing.T, b *Backend) {
	t.Helper()
	entries, err := os.ReadDir(b.dataDir)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasPrefix(e.Name(), ".restore"), "leftover staging dir %s", e.Name())
	}
}

func TestDumboDBBackupRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	b := newSyncTestBackend(t)
	url := syncedBackup(t, b)
	srcRoot := sourceRoot(t, b, "mydb")

	res, err := b.DumboDBBackup(ctx, restoreParams(url, "copy", false))
	require.NoError(t, err)
	require.Equal(t, "copy", res.DB)
	require.Equal(t, srcRoot, res.Root)
	require.Equal(t, srcRoot, sourceRoot(t, b, "copy"))

	require.Equal(t, 2, countDocs(t, b, "copy", "coll"), "main must include the uncommitted doc")
	require.Equal(t, 2, countDocs(t, b, "copy@feature", "coll"))
	tags, err := b.DumboDBTag(ctx, &backends.TagParams{DBName: "copy"})
	require.NoError(t, err)
	require.Len(t, tags.Tags, 1)

	// The restored database is fully writable and committable.
	insertDoc(t, b, "copy", "coll", mustDoc(t, "_id", int64(3)))
	commitDB(t, b, "copy", "after restore")
	require.Equal(t, 3, countDocs(t, b, "copy", "coll"))
	require.Equal(t, 2, countDocs(t, b, "mydb", "coll"))
	requireNoStagingDirs(t, b)

	// It survives a backend restart.
	b.Close()
	b2, err := newBackend(b.dataDir, b.l, false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(b2.Close)
	require.Equal(t, 3, countDocs(t, b2, "copy", "coll"))
}

func TestDumboDBBackupRestoreExisting(t *testing.T) {
	ctx := context.Background()
	b := newSyncTestBackend(t)
	url := syncedBackup(t, b)
	insertDoc(t, b, "target", "coll", mustDoc(t, "_id", int64(99)))
	commitDB(t, b, "target", "t1")

	_, err := b.DumboDBBackup(ctx, restoreParams(url, "target", false))
	require.ErrorContains(t, err, "already exists")
	require.Equal(t, 1, countDocs(t, b, "target", "coll"), "a refused restore must not touch the database")
	requireNoStagingDirs(t, b)

	_, err = b.DumboDBBackup(ctx, restoreParams(url, "mydb", true))
	require.NoError(t, err, "force may replace the source database itself")

	res, err := b.DumboDBBackup(ctx, restoreParams(url, "target", true))
	require.NoError(t, err)
	require.Equal(t, res.Root, sourceRoot(t, b, "target"))
	require.Equal(t, 2, countDocs(t, b, "target", "coll"))

	dropped, err := b.ListDroppedDatabases(ctx)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, d := range dropped.Databases {
		names[d.Name] = true
	}
	require.True(t, names["target"], "force must soft-drop the replaced database: %+v", dropped.Databases)
	requireNoStagingDirs(t, b)
}

func TestDumboDBBackupRestoreRejects(t *testing.T) {
	ctx := context.Background()
	b := newSyncTestBackend(t)
	url := syncedBackup(t, b)
	dir := t.TempDir()

	emptyURL := "file://" + filepath.Join(dir, "empty")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "empty"), 0o755))

	for _, tc := range []struct {
		desc   string
		params *backends.BackupParams
	}{
		{"admin target", restoreParams(url, "admin", true)},
		{"local target", restoreParams(url, "local", false)},
		{"traversal target", restoreParams(url, "../escape", false)},
		{"missing target", restoreParams(url, "", false)},
		{"missing url", restoreParams("", "x", false)},
		{"url inside the data dir", restoreParams("file://"+filepath.Join(b.dataDir, "mydb"), "x", false)},
		{"mem url", restoreParams("mem://x", "x", false)},
		{"empty backup", restoreParams(emptyURL, "x", false)},
		{"nonexistent backup", restoreParams("file://"+filepath.Join(dir, "nope"), "x", false)},
	} {
		if _, err := b.DumboDBBackup(ctx, tc.params); err == nil {
			t.Errorf("%s: want error, got nil", tc.desc)
		}
	}

	_, err := os.Stat(filepath.Join(b.dataDir, "x"))
	require.True(t, os.IsNotExist(err), "a failed restore must not create the database")
	_, err = os.Stat(filepath.Join(filepath.Dir(b.dataDir), "escape"))
	require.True(t, os.IsNotExist(err))
	requireNoStagingDirs(t, b)
}
