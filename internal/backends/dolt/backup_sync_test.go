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
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
)

func newSyncTestBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(b.Close)
	return b
}

func openBackupDB(t *testing.T, url string) *doltdb.DoltDB {
	t.Helper()
	ddb, err := doltdb.LoadDoltDBWithParams(context.Background(), dolttypes.Format_DOLT, url, filesys.LocalFS, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ddb.Close() })
	return ddb
}

func sourceRoot(t *testing.T, b *Backend, dbName string) string {
	t.Helper()
	state, err := b.getOrOpenDB(context.Background(), dbName, false)
	require.NoError(t, err)
	require.NotNil(t, state)
	root, err := state.doltDB.NomsRoot(context.Background())
	require.NoError(t, err)
	return root.String()
}

// workingAndHeadRoots returns the hashes of a branch's working root and of its
// HEAD commit's root; they differ while the branch has uncommitted changes.
func workingAndHeadRoots(t *testing.T, ddb *doltdb.DoltDB, branch string) (working, head string) {
	t.Helper()
	ctx := context.Background()
	wsRef, err := ref.WorkingSetRefForHead(ref.NewBranchRef(branch))
	require.NoError(t, err)
	ws, err := ddb.ResolveWorkingSet(ctx, wsRef)
	require.NoError(t, err)
	wh, err := ws.WorkingRoot().HashOf()
	require.NoError(t, err)

	cm, err := ddb.ResolveCommitRef(ctx, ref.NewBranchRef(branch))
	require.NoError(t, err)
	hr, err := cm.GetRootValue(ctx)
	require.NoError(t, err)
	hh, err := hr.HashOf()
	require.NoError(t, err)
	return wh.String(), hh.String()
}

func syncBackup(t *testing.T, b *Backend, dbName, name string) *backends.BackupResult {
	t.Helper()
	res, err := b.DumboDBBackup(context.Background(), &backends.BackupParams{DBName: dbName, Action: "sync", Name: name})
	require.NoError(t, err)
	return res
}

func TestDumboDBBackupSyncCarriesWholeRoot(t *testing.T) {
	ctx := context.Background()
	b := newSyncTestBackend(t)
	url := "file://" + filepath.Join(t.TempDir(), "bk")

	insertDoc(t, b, "mydb", "coll", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, "mydb", "c1")
	bsBranch(t, b, "mydb", "main", "feature")
	insertDoc(t, b, "mydb", "coll", mustDoc(t, "_id", int64(2)))

	_, err := b.DumboDBBackup(ctx, backupParams("mydb", "add", "nightly", url))
	require.NoError(t, err)

	res := syncBackup(t, b, "mydb", "nightly")
	require.False(t, res.UpToDate)
	require.Equal(t, url, res.URL)
	require.Equal(t, sourceRoot(t, b, "mydb"), res.Root)

	backup := openBackupDB(t, url)
	working, head := workingAndHeadRoots(t, backup, "main")
	require.NotEqual(t, head, working, "uncommitted insert must be in the backup's working set")
	_, err = backup.ResolveCommitRef(ctx, ref.NewBranchRef("feature"))
	require.NoError(t, err, "branch must be in the backup")

	res = syncBackup(t, b, "mydb", "nightly")
	require.True(t, res.UpToDate)
	require.Equal(t, sourceRoot(t, b, "mydb"), res.Root)

	insertDoc(t, b, "mydb", "coll", mustDoc(t, "_id", int64(3)))
	commitDB(t, b, "mydb", "c2")
	res = syncBackup(t, b, "mydb", "nightly")
	require.False(t, res.UpToDate)
	require.Equal(t, sourceRoot(t, b, "mydb"), res.Root)
}

func TestDumboDBBackupSyncURL(t *testing.T) {
	ctx := context.Background()
	b := newSyncTestBackend(t)
	dir := t.TempDir()
	insertDoc(t, b, "mydb", "coll", mustDoc(t, "_id", int64(1)))
	insertDoc(t, b, "otherdb", "coll", mustDoc(t, "_id", int64(1)))

	url := "file://" + filepath.Join(dir, "adhoc")
	res, err := b.DumboDBBackup(ctx, &backends.BackupParams{DBName: "mydb", Action: "sync-url", URL: url})
	require.NoError(t, err)
	require.Equal(t, sourceRoot(t, b, "mydb"), res.Root)

	named := "file://" + filepath.Join(dir, "named")
	_, err = b.DumboDBBackup(ctx, backupParams("mydb", "add", "nightly", named))
	require.NoError(t, err)
	_, err = b.DumboDBBackup(ctx, &backends.BackupParams{DBName: "mydb", Action: "sync-url", URL: named})
	require.NoError(t, err, "sync-url to the database's own backup url is allowed")

	remoteURL := "file://" + filepath.Join(dir, "remote")
	_, err = b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "mydb", Action: "add", Name: "origin", URL: remoteURL})
	require.NoError(t, err)

	for _, tc := range []struct {
		desc   string
		params *backends.BackupParams
	}{
		{"url of a remote", &backends.BackupParams{DBName: "mydb", Action: "sync-url", URL: remoteURL}},
		{"url of another database's backup", &backends.BackupParams{DBName: "otherdb", Action: "sync-url", URL: named}},
		{"missing url", &backends.BackupParams{DBName: "mydb", Action: "sync-url"}},
		{"url inside the data dir", &backends.BackupParams{DBName: "mydb", Action: "sync-url", URL: "file://" + filepath.Join(b.dataDir, "x")}},
		{"unknown backup name", &backends.BackupParams{DBName: "mydb", Action: "sync", Name: "nope"}},
		{"missing backup name", &backends.BackupParams{DBName: "mydb", Action: "sync"}},
		{"admin database", &backends.BackupParams{DBName: "admin", Action: "sync-url", URL: "file://" + filepath.Join(dir, "admin")}},
		{"missing database", &backends.BackupParams{DBName: "nosuchdb", Action: "sync-url", URL: "file://" + filepath.Join(dir, "none")}},
	} {
		if _, err := b.DumboDBBackup(ctx, tc.params); err == nil {
			t.Errorf("%s: want error, got nil", tc.desc)
		}
	}
}

// A backup sync and a GC of the same database run concurrently; the sync must
// still produce a backup matching some source root and readable end to end.
func TestDumboDBBackupSyncConcurrentGC(t *testing.T) {
	b := newSyncTestBackend(t)
	ctx := ctxWithSession(t, b, "lsid-backup-gc")
	dir := t.TempDir()

	for i := 0; i < 200; i++ {
		insertDoc(t, b, "mydb", "coll", mustDoc(t, "_id", int64(i), "pad", fmt.Sprintf("padding-%d-xxxxxxxxxxxxxxxxxxxxxxxx", i)))
	}
	commitDB(t, b, "mydb", "c1")

	var wg sync.WaitGroup
	gcErrs := make(chan error, 5)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			_, err := b.DumboDBGC(ctx, &backends.GCParams{DBName: "mydb", Mode: "full"})
			gcErrs <- err
		}
	}()

	for i := 0; i < 5; i++ {
		url := "file://" + filepath.Join(dir, fmt.Sprintf("bk%d", i))
		res, err := b.DumboDBBackup(context.Background(), &backends.BackupParams{DBName: "mydb", Action: "sync-url", URL: url})
		require.NoError(t, err)
		backup := openBackupDB(t, url)
		root, err := backup.NomsRoot(context.Background())
		require.NoError(t, err)
		require.Equal(t, res.Root, root.String())
		cm, err := backup.ResolveCommitRef(context.Background(), ref.NewBranchRef("main"))
		require.NoError(t, err)
		_, err = cm.GetRootValue(context.Background())
		require.NoError(t, err)
	}
	wg.Wait()
	close(gcErrs)
	for err := range gcErrs {
		require.NoError(t, err)
	}
}
