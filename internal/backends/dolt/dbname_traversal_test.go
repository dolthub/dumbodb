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
	"os"
	"path/filepath"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
)

var traversalNames = []string{"../escape", "../../escape", "x/../config", "a/b", `a\b`, "..", ".", "with\x00nul"}

// Before the fix version-control commands, clone's "as" and undrop's
// "toDatabase" reached filepath.Join(dataDir, name) without the database
// name validation normal commands get, so "../" names opened, created or
// renamed stores outside the data directory.
func TestDatabaseNamesCannotLeaveDataDir(t *testing.T) {
	ctx := context.Background()
	outside := t.TempDir()
	dataDir := filepath.Join(outside, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := newBackend(dataDir, slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	before := listDir(t, outside)

	insertDoc(t, b, "victim", "coll", mustDoc(t, "_id", int64(1)))
	commitDB(t, b, "victim", "c1")
	remote := "file://" + t.TempDir()
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "victim", Action: "add", Name: "origin", URL: remote}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: "victim", Remote: "origin", RefSpec: "main"}); err != nil {
		t.Fatal(err)
	}

	for _, name := range traversalNames {
		if db, err := b.getOrOpenDB(ctx, name, true); err == nil && db != nil {
			t.Errorf("getOrOpenDB(%q, create) opened a store", name)
		}
		if _, err := b.DumboDBLog(ctx, &backends.LogParams{DBName: name}); err == nil {
			t.Errorf("DumboDBLog(%q) succeeded", name)
		}
		if _, err := b.DumboDBClone(ctx, &backends.CloneParams{From: remote, As: name}); err == nil {
			t.Errorf("DumboDBClone(as %q) succeeded", name)
		}
	}

	if err := b.DropDatabase(ctx, &backends.DropDatabaseParams{Name: "victim"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range traversalNames {
		if _, err := b.UndropDatabase(ctx, &backends.UndropParams{Name: "victim", ToDatabase: name}); err == nil {
			t.Errorf("UndropDatabase(toDatabase %q) succeeded", name)
		}
	}

	after := listDir(t, outside)
	for entry := range after {
		if !before[entry] && entry != filepath.Base(b.dataDir) {
			t.Errorf("created %q outside the data directory", filepath.Join(outside, entry))
		}
	}
}

func listDir(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}
