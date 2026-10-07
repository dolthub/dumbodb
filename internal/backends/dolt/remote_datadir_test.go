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
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
)

// Before the fix a file:// remote could point at the server's own data
// directory. Every database there is laid out like a Dolt file remote, so a
// user could clone or fetch any other database (admin included) or push into
// a live store through a second, unsynchronized handle.
func TestRemoteURLsIntoDataDirAreRejected(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "victim", "coll", mustDoc(t, "_id", int64(1), "v", "secret"))
	commitDB(t, b, "victim", "c1")

	outside := t.TempDir()
	link := filepath.Join(outside, "link")
	if err := os.Symlink(b.dataDir, link); err != nil {
		t.Fatal(err)
	}

	for _, u := range []string{
		"file://" + filepath.Join(b.dataDir, "victim"),
		"file://" + b.dataDir,
		"file://" + filepath.Join(outside, "..", filepath.Base(outside), "link", "victim"),
		"file://" + filepath.Join(link, "admin"),
		"git+file://" + filepath.Join(b.dataDir, "repo"),
	} {
		if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "mydb", Action: "add", Name: "r", URL: u}); err == nil {
			t.Errorf("remote add %s: expected an error", u)
		}
		if _, err := b.DumboDBClone(ctx, &backends.CloneParams{From: u, As: "loot"}); err == nil {
			t.Errorf("clone %s: expected an error", u)
		}
	}
}

func TestRemoteURLsOutsideDataDirStillWork(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "mydb", Action: "add", Name: "r", URL: "file://" + t.TempDir()}); err != nil {
		t.Fatalf("remote add outside the data dir: %v", err)
	}
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "mydb", Action: "add", Name: "missing", URL: "file:///nonexistent/dir/repo"}); err != nil {
		t.Fatalf("remote add to a path that does not exist yet: %v", err)
	}
}
