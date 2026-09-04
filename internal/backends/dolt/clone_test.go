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
	"errors"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/ref"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

func TestDumboDBClone_FromFileRemote(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)
	remoteURL := "file://" + t.TempDir()

	// Source db: insert, commit, push to the file:// remote.
	insertDoc(t, b, "src", "coll", mustDoc(t, "_id", int64(1), "v", "hello"))
	c1 := commitDB(t, b, "src", "c1")
	if _, err := b.DumboDBRemote(ctx, &backends.RemoteParams{DBName: "src", Action: "add", Name: "origin", URL: remoteURL}); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	if _, err := b.DumboDBPush(ctx, &backends.PushParams{DBName: "src", Remote: "origin", RefSpec: "main"}); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Clone the remote into a brand-new database.
	res, err := b.DumboDBClone(ctx, &backends.CloneParams{From: remoteURL, As: "clonedb"})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if res.DB != "clonedb" {
		t.Errorf("clone db = %s, want clonedb", res.DB)
	}

	// Cloned main head resolves to c1.
	st := mustDB(t, b, "clonedb")
	cm, err := st.doltDB.ResolveCommitRef(ctx, ref.NewBranchRef("main"))
	if err != nil {
		t.Fatalf("resolve clonedb main: %v", err)
	}
	h, _ := cm.HashOf()
	if h.String() != c1 {
		t.Errorf("clonedb refs/heads/main = %s, want c1 %s", h.String(), c1)
	}

	// The cloned database is readable: the document is present with its value.
	adb, err := b.Database("clonedb")
	if err != nil {
		t.Fatalf("open clonedb: %v", err)
	}
	coll, err := adb.Collection("coll")
	if err != nil {
		t.Fatalf("open collection: %v", err)
	}
	qr, err := coll.Query(ctx, nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer qr.Iter.Close()

	found := false
	for {
		_, doc, err := qr.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			t.Fatalf("iter: %v", err)
		}
		if id, _ := doc.Get("_id"); id == int64(1) {
			found = true
			if v, _ := doc.Get("v"); v != "hello" {
				t.Errorf("cloned doc v = %v, want \"hello\"", v)
			}
		}
	}
	if !found {
		t.Error("cloned document _id:1 not found in clonedb (clone did not materialize data)")
	}

	// Clone into an existing db is refused.
	if _, err := b.DumboDBClone(ctx, &backends.CloneParams{From: remoteURL, As: "clonedb"}); err == nil {
		t.Error("clone into existing db: want error, got nil")
	}

	// Unsupported scheme is refused (ssh is known but not implemented).
	if _, err := b.DumboDBClone(ctx, &backends.CloneParams{From: "ssh://host/o/r", As: "c2"}); err == nil {
		t.Error("clone ssh: want error, got nil")
	}

	// Reserved db name is refused.
	if _, err := b.DumboDBClone(ctx, &backends.CloneParams{From: remoteURL, As: "admin"}); err == nil {
		t.Error("clone into admin: want error, got nil")
	}
}
