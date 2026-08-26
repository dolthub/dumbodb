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
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/ref"

	"github.com/dolthub/dumbodb/internal/backends"
)

func TestReadOnlyRootishStatusAndDiff(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int64(1)))
	snapshotHash := commitDB(t, b, "testdb", "snapshot")
	if _, err := b.DumboDBTag(ctx, &backends.TagParams{
		DBName: "testdb",
		Branch: "main",
		Name:   "v1",
		Hash:   snapshotHash,
	}); err != nil {
		t.Fatalf("DumboDBTag: %v", err)
	}

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int64(2)))
	commitDB(t, b, "testdb", "advance main")

	for _, rootish := range []string{"v1", snapshotHash} {
		t.Run(rootish, func(t *testing.T) {
			assertWorkingSetDoesNotExist(t, b, "testdb", rootish)

			status, err := b.DumboDBStatus(ctx, &backends.VersioningStatusParams{
				DBName: "testdb",
				Branch: rootish,
			})
			if err != nil {
				t.Fatalf("DumboDBStatus(%q): %v", rootish, err)
			}
			if status.CommitID != snapshotHash {
				t.Errorf("DumboDBStatus(%q) commit ID = %q, want %q", rootish, status.CommitID, snapshotHash)
			}
			if len(status.Tables) != 0 || len(status.Views) != 0 {
				t.Errorf("DumboDBStatus(%q) reported snapshot changes: tables=%v views=%v", rootish, status.Tables, status.Views)
			}

			diff, err := b.DumboDBDiff(ctx, &backends.DiffParams{
				DBName:      "testdb",
				ConnRootish: rootish,
			})
			if err != nil {
				t.Fatalf("DumboDBDiff(%q): %v", rootish, err)
			}
			if len(diff.Collections) != 0 || len(diff.Views) != 0 {
				t.Errorf("DumboDBDiff(%q) reported snapshot changes: collections=%v views=%v", rootish, diff.Collections, diff.Views)
			}

			fromSnapshot, err := b.DumboDBDiff(ctx, &backends.DiffParams{
				DBName:      "testdb",
				ConnRootish: rootish,
				From:        rootish,
				To:          "main",
			})
			if err != nil {
				t.Fatalf("DumboDBDiff(%q, main): %v", rootish, err)
			}
			if len(fromSnapshot.Collections) != 1 || len(fromSnapshot.Collections[0].Added) != 1 {
				t.Errorf("DumboDBDiff(%q, main) did not resolve the snapshot: %+v", rootish, fromSnapshot.Collections)
			}

			assertWorkingSetDoesNotExist(t, b, "testdb", rootish)
		})
	}
}

func assertWorkingSetDoesNotExist(t *testing.T, b *Backend, dbName, rootish string) {
	t.Helper()

	state, err := b.getOrOpenDB(context.Background(), dbName, false)
	if err != nil {
		t.Fatalf("getOrOpenDB(%q): %v", dbName, err)
	}
	if _, err = state.doltDB.ResolveWorkingSet(context.Background(), ref.NewWorkingSetRef("heads/"+rootish)); err == nil {
		t.Errorf("working set unexpectedly exists for read-only rootish %q", rootish)
	}
}
