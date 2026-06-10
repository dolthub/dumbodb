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

// Red-bar tests for behavior B2 (merged indexes are correct) from
// docs/design/secondary-index-structural-sharing.md.
//
// These fail today: mergeAddressMapsWithConflicts merges only the
// primary maps and re-attaches the into-branch's index AM, so documents
// written on the from branch are invisible to index lookups after the
// merge. They turn green with Phase 3 (workspace-nth).

import (
	"context"
	"reflect"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
)

// mergeBranches merges from into into and fails the test on error or
// unresolved conflicts.
func mergeBranches(t *testing.T, b *Backend, dbName, into, from string) {
	t.Helper()
	res, err := b.DumboDBMerge(context.Background(), &backends.MergeParams{
		DBName:  dbName,
		Into:    into,
		From:    from,
		Message: "test merge",
		Author:  "testuser <test@example.com>",
	})
	if err != nil {
		t.Fatalf("DumboDBMerge(%s <- %s): %v", into, from, err)
	}
	if res.CommitID == "" {
		t.Fatalf("DumboDBMerge(%s <- %s): no merge commit created (conflicts pending?)", into, from)
	}
}

// TestMergedIndexReflectsBothBranches is the disjoint-write scenario:
// each branch inserts its own documents under a shared index; after the
// merge, index lookups on the merged branch must find both sides' docs.
func TestMergedIndexReflectsBothBranches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	// Base: collection with the index and one seed doc, committed on main.
	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "base"))
	createIndex(t, ctx, collAt(t, b, "testdb", "main", "items"), "by_field", "field")
	commitDB(t, b, "testdb", "base: seed + index")

	branchFrom(t, b, "testdb", "main", "feat")

	// Main writes alpha..charlie.
	mainColl := collAt(t, b, "testdb", "main", "items")
	for i, v := range []string{"alpha", "bravo", "charlie"} {
		insertOne(t, ctx, mainColl, mustDoc(t, "_id", int32(10+i), "field", v))
	}
	commitBranch(t, b, "testdb", "main", "main: alpha..charlie")

	// Feat writes november..papa.
	featColl := collAt(t, b, "testdb", "feat", "items")
	for i, v := range []string{"november", "oscar", "papa"} {
		insertOne(t, ctx, featColl, mustDoc(t, "_id", int32(20+i), "field", v))
	}
	commitBranch(t, b, "testdb", "feat", "feat: november..papa")

	mergeBranches(t, b, "testdb", "main", "feat")

	merged := collAt(t, b, "testdb", "main", "items")

	// Sanity: the merged primary holds all 7 documents.
	allDocs := drainQuery(t, ctx, merged, &backends.QueryParams{})
	if len(allDocs) != 7 {
		t.Fatalf("merged primary has %d docs, want 7; merge itself is broken", len(allDocs))
	}

	// Index lookups must see both sides.
	cases := []struct {
		value string
		want  []int32
	}{
		{"base", []int32{1}},
		{"alpha", []int32{10}},     // into-side write
		{"november", []int32{20}},  // from-side write -- fails today
		{"papa", []int32{22}},      // from-side write -- fails today
	}
	for _, c := range cases {
		got := equalityLookupIDs(t, ctx, merged, "field", c.value)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("index lookup %q on merged branch = %v, want %v", c.value, got, c.want)
		}
		gotN := indexedCount(t, ctx, merged, "field", c.value)
		if gotN != int64(len(c.want)) {
			t.Errorf("indexed count %q on merged branch = %d, want %d", c.value, gotN, len(c.want))
		}
	}
}

// TestOneSidedIndexCoversMergedDocs is behavior B4: an index created on
// only one branch since the base must, after the merge, cover documents
// written on the other branch.
func TestOneSidedIndexCoversMergedDocs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "base"))
	commitDB(t, b, "testdb", "base: seed, no index")

	branchFrom(t, b, "testdb", "main", "feat")

	// Main creates the index (post-base).
	createIndex(t, ctx, collAt(t, b, "testdb", "main", "items"), "by_field", "field")
	commitBranch(t, b, "testdb", "main", "main: create by_field")

	// Feat writes docs without knowing about the index.
	featColl := collAt(t, b, "testdb", "feat", "items")
	insertOne(t, ctx, featColl, mustDoc(t, "_id", int32(20), "field", "november"))
	commitBranch(t, b, "testdb", "feat", "feat: november")

	mergeBranches(t, b, "testdb", "main", "feat")

	merged := collAt(t, b, "testdb", "main", "items")
	got := equalityLookupIDs(t, ctx, merged, "field", "november")
	want := []int32{20}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("one-sided index lookup %q = %v, want %v", "november", got, want)
	}
	gotN := indexedCount(t, ctx, merged, "field", "november")
	if gotN != 1 {
		t.Errorf("one-sided indexed count = %d, want 1", gotN)
	}
}
