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
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
)

func backupParams(db, action, name, url string) *backends.BackupParams {
	return &backends.BackupParams{DBName: db, Action: action, Name: name, URL: url}
}

func TestDumboDBBackupRegistry(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	insertDoc(t, b, "mydb", "coll", mustDoc(t, "_id", int64(1)))
	insertDoc(t, b, "otherdb", "coll", mustDoc(t, "_id", int64(1)))
	url := "file://" + filepath.Join(t.TempDir(), "mydb")

	res, err := b.DumboDBBackup(ctx, backupParams("mydb", "add", "nightly", url))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(res.Backups) != 1 || res.Backups[0].Name != "nightly" || res.Backups[0].URL != url {
		t.Fatalf("add result = %+v", res)
	}

	res, err = b.DumboDBBackup(ctx, backupParams("mydb", "list", "", ""))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Backups) != 1 || res.Backups[0].Name != "nightly" || res.Backups[0].URL != url {
		t.Fatalf("list = %+v", res)
	}

	res, err = b.DumboDBBackup(ctx, backupParams("otherdb", "list", "", ""))
	if err != nil {
		t.Fatalf("otherdb list: %v", err)
	}
	if len(res.Backups) != 0 {
		t.Errorf("otherdb list = %+v, want empty", res.Backups)
	}

	remotes, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "mydb", Action: "list"})
	if err != nil {
		t.Fatalf("remote list: %v", err)
	}
	if len(remotes.Remotes) != 0 {
		t.Errorf("backup leaked into remotes: %+v", remotes.Remotes)
	}

	if _, err = b.DumboDBBackup(ctx, backupParams("mydb", "remove", "nightly", "")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	res, err = b.DumboDBBackup(ctx, backupParams("mydb", "list", "", ""))
	if err != nil {
		t.Fatalf("list after remove: %v", err)
	}
	if len(res.Backups) != 0 {
		t.Errorf("list after remove = %+v, want empty", res.Backups)
	}

	if _, err = b.DumboDBBackup(ctx, backupParams("mydb", "remove", "nightly", "")); err == nil {
		t.Error("remove missing backup: want error, got nil")
	}
}

func TestDumboDBBackupAddRejects(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	insertDoc(t, b, "mydb", "coll", mustDoc(t, "_id", int64(1)))
	insertDoc(t, b, "otherdb", "coll", mustDoc(t, "_id", int64(1)))
	dir := t.TempDir()
	used := "file://" + filepath.Join(dir, "used")
	remoteURL := "file://" + filepath.Join(dir, "remote")
	fresh := "file://" + filepath.Join(dir, "fresh")

	if _, err := b.DumboDBBackup(ctx, backupParams("mydb", "add", "nightly", used)); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "otherdb", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("remote add: %v", err)
	}

	for _, tc := range []struct {
		desc   string
		params *backends.BackupParams
	}{
		{"duplicate name", backupParams("mydb", "add", "nightly", fresh)},
		{"url used by a backup of the same db", backupParams("mydb", "add", "other", used)},
		{"url used by a backup of another db", backupParams("otherdb", "add", "nightly", used)},
		{"url used by a remote", backupParams("mydb", "add", "other", remoteURL)},
		{"unparseable url", backupParams("mydb", "add", "bad", "://x")},
		{"mem url", backupParams("mydb", "add", "mem", "mem://x")},
		{"url inside the data dir", backupParams("mydb", "add", "inside", "file://"+filepath.Join(b.dataDir, "loot"))},
		{"name with @", backupParams("mydb", "add", "a@b", fresh)},
		{"name with whitespace", backupParams("mydb", "add", "a b", fresh)},
		{"empty name", backupParams("mydb", "add", "", fresh)},
		{"missing database", backupParams("nosuchdb", "add", "nightly", fresh)},
		{"admin database", backupParams("admin", "add", "nightly", fresh)},
		{"local database", backupParams("local", "add", "nightly", fresh)},
		{"empty database", backupParams("", "add", "nightly", fresh)},
		{"traversal database", backupParams("../mydb", "add", "nightly", fresh)},
	} {
		if _, err := b.DumboDBBackup(ctx, tc.params); err == nil {
			t.Errorf("%s: want error, got nil", tc.desc)
		}
	}

	if _, err := b.DumboDBBackup(ctx, backupParams("admin", "list", "", "")); err == nil {
		t.Error("admin list: want error, got nil")
	}

	res, err := b.DumboDBBackup(ctx, backupParams("mydb", "list", "", ""))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Backups) != 1 {
		t.Errorf("rejected adds were stored: %+v", res.Backups)
	}
}

// A remote, including the origin a clone registers, may not take a URL that
// is already a backup: pushes would write into it and the next backup sync
// would replace their refs.
func TestRemotesCannotShareABackupURL(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	insertDoc(t, b, "orders", "coll", mustDoc(t, "_id", int64(1)))
	insertDoc(t, b, "mydb", "coll", mustDoc(t, "_id", int64(1)))
	url := "file://" + filepath.Join(t.TempDir(), "orders-backup")

	if _, err := b.DumboDBBackup(ctx, backupParams("orders", "add", "nightly", url)); err != nil {
		t.Fatalf("backup add: %v", err)
	}
	// Sync so the URL holds a real store, as clone reads it.
	commitDB(t, b, "orders", "c1")
	if _, err := b.DumboDBBackup(ctx, backupParams("orders", "sync", "nightly", "")); err != nil {
		t.Fatalf("backup sync: %v", err)
	}

	for _, db := range []string{"mydb", "orders"} {
		if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: db, Action: "add", Name: "origin", URL: url}); err == nil {
			t.Errorf("remote add on %s with a backup url: want error, got nil", db)
		}
		res, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: db, Action: "list"})
		if err != nil {
			t.Fatalf("remote list: %v", err)
		}
		if len(res.Remotes) != 0 {
			t.Errorf("rejected remote was stored on %s: %+v", db, res.Remotes)
		}
	}

	// A clone from a backup URL is allowed but registers nothing: the URL
	// stays owned by the backup.
	if _, err := b.DumboDBClone(ctx, &backends.CloneParams{From: url, As: "loot"}); err != nil {
		t.Fatalf("clone from a backup url: %v", err)
	}
	res, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "loot", Action: "list"})
	if err != nil {
		t.Fatalf("remote list on backup clone: %v", err)
	}
	if len(res.Remotes) != 0 {
		t.Errorf("backup clone registered remotes: %+v", res.Remotes)
	}
	pull, err := b.getBranchPull(ctx, "loot", defaultBranch)
	if err != nil {
		t.Fatalf("branch pull config: %v", err)
	}
	if !pull.empty() {
		t.Errorf("backup clone configured upstream: %+v", pull)
	}

	other := "file://" + filepath.Join(t.TempDir(), "shared-remote")
	for _, db := range []string{"mydb", "orders"} {
		if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: db, Action: "add", Name: "origin", URL: other}); err != nil {
			t.Errorf("remotes may still share a non-backup url (%s): %v", db, err)
		}
	}
}

// Claiming a URL is check-then-insert; concurrent claims on one URL, whether
// by remotes or backups of any database, must leave exactly one owner.
func TestConcurrentClaimsOnOneURLLeaveOneOwner(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	dbs := []string{"orders", "inventory", "billing"}
	for _, db := range dbs {
		insertDoc(t, b, db, "coll", mustDoc(t, "_id", int64(1)))
	}

	for round := 0; round < 20; round++ {
		url := "file://" + filepath.Join(t.TempDir(), fmt.Sprintf("contested-%d", round))
		name := fmt.Sprintf("r%d", round)

		var wg sync.WaitGroup
		var succeeded atomic.Int32
		claim := func(fn func() error) {
			defer wg.Done()
			if fn() == nil {
				succeeded.Add(1)
			}
		}
		for _, db := range dbs {
			wg.Add(2)
			go claim(func() error {
				_, err := b.DumboDBBackup(ctx, backupParams(db, "add", name, url))
				return err
			})
			go claim(func() error {
				_, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: db, Action: "add", Name: name, URL: url})
				return err
			})
		}
		wg.Wait()

		backupOwners := 0
		for _, db := range dbs {
			res, err := b.DumboDBBackup(ctx, backupParams(db, "list", "", ""))
			if err != nil {
				t.Fatalf("backup list: %v", err)
			}
			for _, bk := range res.Backups {
				if bk.URL == url {
					backupOwners++
				}
			}
		}
		remoteOwners := 0
		for _, db := range dbs {
			res, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: db, Action: "list"})
			if err != nil {
				t.Fatalf("remote list: %v", err)
			}
			for _, r := range res.Remotes {
				if r.URL == url {
					remoteOwners++
				}
			}
		}

		// Remotes may share a URL with each other; a backup shares it with nothing.
		if backupOwners > 1 || (backupOwners == 1 && remoteOwners > 0) {
			t.Fatalf("round %d: %d backups and %d remotes claim %s (%d claims succeeded)",
				round, backupOwners, remoteOwners, url, succeeded.Load())
		}
	}
}
