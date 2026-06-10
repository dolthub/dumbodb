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

// Behavior B7 of docs/design/secondary-index-structural-sharing.md:
// cherry-pick, rebase, and revert share the merge machinery and must
// maintain indexes the same way.

import (
	"context"
	"reflect"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
)

func TestCherryPickMaintainsIndexes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "base"))
	createIndex(t, ctx, collAt(t, b, "testdb", "main", "items"), "by_field", "field")
	commitDB(t, b, "testdb", "base")
	branchFrom(t, b, "testdb", "main", "feat")

	// Advance main so the pick is a 3-way apply, not a fast-forward.
	insertOne(t, ctx, collAt(t, b, "testdb", "main", "items"), mustDoc(t, "_id", int32(5), "field", "mainside"))
	commitBranch(t, b, "testdb", "main", "main: doc 5")

	insertOne(t, ctx, collAt(t, b, "testdb", "feat", "items"), mustDoc(t, "_id", int32(20), "field", "november"))
	pickHash := commitBranch(t, b, "testdb", "feat", "feat: november")

	if _, err := b.DumboDBCherryPick(ctx, &backends.CherryPickParams{
		DBName: "testdb", Branch: "main", Commit: pickHash,
	}); err != nil {
		t.Fatalf("DumboDBCherryPick: %v", err)
	}

	coll := collAt(t, b, "testdb", "main", "items")
	if got := equalityLookupIDs(t, ctx, coll, "field", "november"); !reflect.DeepEqual(got, []int32{20}) {
		t.Errorf("index lookup after cherry-pick = %v, want [20]", got)
	}
	if got := indexedCount(t, ctx, coll, "field", "november"); got != 1 {
		t.Errorf("indexed count after cherry-pick = %d, want 1", got)
	}
}

func TestRevertMaintainsIndexes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "base"))
	createIndex(t, ctx, collAt(t, b, "testdb", "main", "items"), "by_field", "field")
	commitDB(t, b, "testdb", "base")

	coll := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(2), "field", "victim"))
	victimHash := commitBranch(t, b, "testdb", "main", "main: victim doc")

	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(3), "field", "keeper"))
	commitBranch(t, b, "testdb", "main", "main: keeper doc")

	if _, err := b.DumboDBRevert(ctx, &backends.RevertParams{
		DBName: "testdb", Branch: "main", Commit: victimHash,
	}); err != nil {
		t.Fatalf("DumboDBRevert: %v", err)
	}

	if got := equalityLookupIDs(t, ctx, coll, "field", "victim"); len(got) != 0 {
		t.Errorf("index lookup of reverted doc = %v, want empty", got)
	}
	if got := indexedCount(t, ctx, coll, "field", "victim"); got != 0 {
		t.Errorf("indexed count of reverted doc = %d, want 0", got)
	}
	if got := equalityLookupIDs(t, ctx, coll, "field", "keeper"); !reflect.DeepEqual(got, []int32{3}) {
		t.Errorf("index lookup of kept doc = %v, want [3]", got)
	}

	// Reverting an index-creating commit: index disappears, docs stay.
	b2 := newTestBackend(t)
	insertDoc(t, b2, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "base"))
	commitDB(t, b2, "testdb", "seed")
	coll2 := collAt(t, b2, "testdb", "main", "items")
	createIndex(t, ctx, coll2, "by_field", "field")
	idxHash := commitBranch(t, b2, "testdb", "main", "main: create index")
	insertOne(t, ctx, coll2, mustDoc(t, "_id", int32(2), "field", "later"))
	commitBranch(t, b2, "testdb", "main", "main: later doc")

	if _, err := b2.DumboDBRevert(ctx, &backends.RevertParams{
		DBName: "testdb", Branch: "main", Commit: idxHash,
	}); err != nil {
		t.Fatalf("DumboDBRevert(index-creating commit): %v", err)
	}
	gotNames := listIndexNames(t, ctx, coll2)
	wantNames := []string{backends.DefaultIndexName}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("indexes after reverting index creation = %v, want %v", gotNames, wantNames)
	}
	docs := drainQuery(t, ctx, coll2, &backends.QueryParams{})
	if len(docs) != 2 {
		t.Errorf("docs after reverting index creation = %d, want 2", len(docs))
	}
}

func TestRebaseMaintainsIndexes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "base"))
	createIndex(t, ctx, collAt(t, b, "testdb", "main", "items"), "by_field", "field")
	commitDB(t, b, "testdb", "base")
	branchFrom(t, b, "testdb", "main", "feat")

	insertOne(t, ctx, collAt(t, b, "testdb", "main", "items"), mustDoc(t, "_id", int32(5), "field", "mainside"))
	commitBranch(t, b, "testdb", "main", "main: doc 5")

	insertOne(t, ctx, collAt(t, b, "testdb", "feat", "items"), mustDoc(t, "_id", int32(20), "field", "november"))
	commitBranch(t, b, "testdb", "feat", "feat: november")

	if _, err := b.DumboDBRebase(ctx, &backends.RebaseParams{
		DBName: "testdb", Branch: "feat", Onto: "main",
	}); err != nil {
		t.Fatalf("DumboDBRebase: %v", err)
	}

	coll := collAt(t, b, "testdb", "feat", "items")
	for _, c := range []struct {
		value string
		want  []int32
	}{
		{"november", []int32{20}},
		{"mainside", []int32{5}},
		{"base", []int32{1}},
	} {
		if got := equalityLookupIDs(t, ctx, coll, "field", c.value); !reflect.DeepEqual(got, c.want) {
			t.Errorf("rebased index lookup %q = %v, want %v", c.value, got, c.want)
		}
	}
}
