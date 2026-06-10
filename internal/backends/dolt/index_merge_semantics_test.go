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

// Behaviors B2, B5, B6, and C1-C5 of
// docs/design/secondary-index-structural-sharing.md.

import (
	"context"
	"reflect"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

func mergeExpectConflicts(t *testing.T, b *Backend, dbName, into, from, collection string) []backends.ConflictInfo {
	t.Helper()
	ctx := context.Background()
	_, err := b.DumboDBMerge(ctx, &backends.MergeParams{
		DBName: dbName, Into: into, From: from,
		Message: "test merge", Author: "testuser <test@example.com>",
	})
	if err == nil {
		t.Fatalf("DumboDBMerge: expected conflicts, merge completed cleanly")
	}
	res, cerr := b.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: dbName, Branch: into})
	if cerr != nil {
		t.Fatalf("DumboDBConflicts: %v", cerr)
	}
	for _, cc := range res.Collections {
		if cc.Collection == collection {
			return cc.Conflicts
		}
	}
	t.Fatalf("no conflicts reported for collection %q", collection)
	return nil
}

func resolveConflict(t *testing.T, b *Backend, dbName, branch, collection, conflictID, resolution string, value *types.Document) error {
	t.Helper()
	_, err := b.DumboDBResolveConflict(context.Background(), &backends.ResolveConflictParams{
		DBName: dbName, Branch: branch, Collection: collection,
		ConflictID: conflictID, Resolution: resolution, Value: value,
	})
	return err
}

func continueMerge(t *testing.T, b *Backend, dbName, into string) {
	t.Helper()
	if _, err := b.DumboDBMerge(context.Background(), &backends.MergeParams{
		DBName: dbName, Into: into, Continue: true,
		Message: "merge continued", Author: "testuser <test@example.com>",
	}); err != nil {
		t.Fatalf("DumboDBMerge continue: %v", err)
	}
}

// indexConsistentWithScan: index-driven lookups must equal full-scan
// results (the design doc's section 2.6 self-consistency invariant).
func indexConsistentWithScan(t *testing.T, ctx context.Context, coll backends.Collection, field string, values []string) {
	t.Helper()
	for _, v := range values {
		idxIDs := equalityLookupIDs(t, ctx, coll, field, v)
		scanIDs := []int32{}
		for _, doc := range drainQuery(t, ctx, coll, &backends.QueryParams{}) {
			fv, err := doc.Get(field)
			if err == nil && fv == v {
				id, ierr := doc.Get("_id")
				if ierr == nil {
					scanIDs = append(scanIDs, id.(int32))
				}
			}
		}
		if !reflect.DeepEqual(idxIDs, scanIDs) && !(len(idxIDs) == 0 && len(scanIDs) == 0) {
			t.Errorf("index/scan divergence for %q: index=%v scan=%v", v, idxIDs, scanIDs)
		}
	}
}

// B2's neither-parent case: a field-merged document exists on neither
// branch, so a 3-way merge of the index maps could never index it.
func TestMergedIndexReflectsFieldMergedDocs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items",
		mustDoc(t, "_id", int32(1), "field", "alpha", "other", "x"))
	coll := collAt(t, b, "testdb", "main", "items")
	createIndex(t, ctx, coll, "by_field", "field")
	commitDB(t, b, "testdb", "base")

	branchFrom(t, b, "testdb", "main", "feat")

	updateDoc(t, ctx, coll, mustDoc(t, "_id", int32(1), "field", "zulu", "other", "x"))
	commitBranch(t, b, "testdb", "main", "main: field=zulu")

	featColl := collAt(t, b, "testdb", "feat", "items")
	updateDoc(t, ctx, featColl, mustDoc(t, "_id", int32(1), "field", "alpha", "other", "y"))
	commitBranch(t, b, "testdb", "feat", "feat: other=y")

	mergeBranches(t, b, "testdb", "main", "feat")

	merged := collAt(t, b, "testdb", "main", "items")
	if got := equalityLookupIDs(t, ctx, merged, "field", "zulu"); !reflect.DeepEqual(got, []int32{1}) {
		t.Errorf("merged-doc lookup by zulu = %v, want [1]", got)
	}
	if got := equalityLookupIDs(t, ctx, merged, "field", "alpha"); len(got) != 0 {
		t.Errorf("stale lookup by alpha = %v, want empty", got)
	}
	docs := drainQuery(t, ctx, merged, &backends.QueryParams{})
	if len(docs) != 1 {
		t.Fatalf("merged collection has %d docs, want 1", len(docs))
	}
	other, _ := docs[0].Get("other")
	if other != "y" {
		t.Fatalf("field-level merge did not apply (other=%v); test premise broken", other)
	}
}

// The B5 definition-reconciliation case table.
func TestDroppedIndexStaysDroppedAfterMerge(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Case 1: into drops, from keeps and writes docs; drop wins.
	b := newTestBackend(t)
	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "base"))
	createIndex(t, ctx, collAt(t, b, "testdb", "main", "items"), "by_field", "field")
	commitDB(t, b, "testdb", "base")
	branchFrom(t, b, "testdb", "main", "feat")

	mainColl := collAt(t, b, "testdb", "main", "items")
	if _, err := mainColl.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: []string{"by_field"}}); err != nil {
		t.Fatalf("DropIndexes: %v", err)
	}
	commitBranch(t, b, "testdb", "main", "main: drop by_field")

	insertOne(t, ctx, collAt(t, b, "testdb", "feat", "items"), mustDoc(t, "_id", int32(2), "field", "november"))
	commitBranch(t, b, "testdb", "feat", "feat: november")

	mergeBranches(t, b, "testdb", "main", "feat")

	gotNames := listIndexNames(t, ctx, collAt(t, b, "testdb", "main", "items"))
	wantNames := []string{backends.DefaultIndexName}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("case drop-vs-write: indexes after merge = %v, want %v", gotNames, wantNames)
	}

	// Case 2: drop on both sides.
	b2 := newTestBackend(t)
	insertDoc(t, b2, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "base"))
	createIndex(t, ctx, collAt(t, b2, "testdb", "main", "items"), "by_field", "field")
	commitDB(t, b2, "testdb", "base")
	branchFrom(t, b2, "testdb", "main", "feat")
	if _, err := collAt(t, b2, "testdb", "main", "items").DropIndexes(ctx, &backends.DropIndexesParams{Indexes: []string{"by_field"}}); err != nil {
		t.Fatalf("DropIndexes main: %v", err)
	}
	insertOne(t, ctx, collAt(t, b2, "testdb", "main", "items"), mustDoc(t, "_id", int32(3), "field", "x"))
	commitBranch(t, b2, "testdb", "main", "main: drop")
	if _, err := collAt(t, b2, "testdb", "feat", "items").DropIndexes(ctx, &backends.DropIndexesParams{Indexes: []string{"by_field"}}); err != nil {
		t.Fatalf("DropIndexes feat: %v", err)
	}
	insertOne(t, ctx, collAt(t, b2, "testdb", "feat", "items"), mustDoc(t, "_id", int32(4), "field", "y"))
	commitBranch(t, b2, "testdb", "feat", "feat: drop")
	mergeBranches(t, b2, "testdb", "main", "feat")
	gotNames = listIndexNames(t, ctx, collAt(t, b2, "testdb", "main", "items"))
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("case drop-vs-drop: indexes after merge = %v, want %v", gotNames, wantNames)
	}

	// Case 3: into drops and recreates with a different spec; the
	// redefinition wins and covers from's docs.
	b3 := newTestBackend(t)
	insertDoc(t, b3, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "base"))
	createIndex(t, ctx, collAt(t, b3, "testdb", "main", "items"), "by_field", "field")
	commitDB(t, b3, "testdb", "base")
	branchFrom(t, b3, "testdb", "main", "feat")

	mainColl3 := collAt(t, b3, "testdb", "main", "items")
	if _, err := mainColl3.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: []string{"by_field"}}); err != nil {
		t.Fatalf("DropIndexes: %v", err)
	}
	if _, err := mainColl3.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{{
			Name:   "by_field",
			Key:    []backends.IndexKeyPair{{Field: "field"}},
			Sparse: true, // different spec
		}},
	}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}
	commitBranch(t, b3, "testdb", "main", "main: redefine by_field sparse")

	insertOne(t, ctx, collAt(t, b3, "testdb", "feat", "items"), mustDoc(t, "_id", int32(2), "field", "november"))
	commitBranch(t, b3, "testdb", "feat", "feat: november")

	mergeBranches(t, b3, "testdb", "main", "feat")

	merged3 := collAt(t, b3, "testdb", "main", "items")
	res, err := merged3.ListIndexes(ctx, nil)
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
	foundSparse := false
	for _, idx := range res.Indexes {
		if idx.Name == "by_field" && idx.Sparse {
			foundSparse = true
		}
	}
	if !foundSparse {
		t.Errorf("case redefine-vs-untouched: redefined (sparse) index did not win")
	}
	if got := equalityLookupIDs(t, ctx, merged3, "field", "november"); !reflect.DeepEqual(got, []int32{2}) {
		t.Errorf("case redefine-vs-untouched: winner does not cover from's docs: %v", got)
	}
}

// B6: same unique key inserted on both branches becomes a conflict
// requiring manual resolution; the merged state keeps ours.
func TestMergeUniqueViolationIsConflict(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "seed"))
	coll := collAt(t, b, "testdb", "main", "items")
	if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{{
			Name:   "by_field_unique",
			Key:    []backends.IndexKeyPair{{Field: "field"}},
			Unique: true,
		}},
	}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}
	commitDB(t, b, "testdb", "base")
	branchFrom(t, b, "testdb", "main", "feat")

	insertOne(t, ctx, collAt(t, b, "testdb", "main", "items"), mustDoc(t, "_id", int32(10), "field", "dup"))
	commitBranch(t, b, "testdb", "main", "main: doc 10 field=dup")

	insertOne(t, ctx, collAt(t, b, "testdb", "feat", "items"), mustDoc(t, "_id", int32(20), "field", "dup"))
	commitBranch(t, b, "testdb", "feat", "feat: doc 20 field=dup")

	conflicts := mergeExpectConflicts(t, b, "testdb", "main", "feat", "items")
	if len(conflicts) != 1 {
		t.Fatalf("unique violation produced %d conflicts, want 1", len(conflicts))
	}

	merged := collAt(t, b, "testdb", "main", "items")
	if got := equalityLookupIDs(t, ctx, merged, "field", "dup"); !reflect.DeepEqual(got, []int32{10}) {
		t.Errorf("post-merge owner of dup = %v, want [10]", got)
	}
	if conflicts[0].Theirs == nil {
		t.Fatalf("conflict entry missing theirs document")
	}

	if err := resolveConflict(t, b, "testdb", "main", "items", conflicts[0].ConflictID, "ours", nil); err != nil {
		t.Fatalf("resolve ours: %v", err)
	}
	continueMerge(t, b, "testdb", "main")
	indexConsistentWithScan(t, ctx, merged, "field", []string{"dup", "seed"})
}

// mergeWithDocConflict sets up a divergent-modify conflict on doc 1
// with an index on "field".
func mergeWithDocConflict(t *testing.T) (*Backend, []backends.ConflictInfo) {
	t.Helper()
	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items",
		mustDoc(t, "_id", int32(1), "field", "alpha", "status", "active"))
	coll := collAt(t, b, "testdb", "main", "items")
	createIndex(t, ctx, coll, "by_field", "field")
	commitDB(t, b, "testdb", "base")
	branchFrom(t, b, "testdb", "main", "feat")

	updateDoc(t, ctx, coll, mustDoc(t, "_id", int32(1), "field", "ours-val", "status", "active"))
	commitBranch(t, b, "testdb", "main", "main: ours-val")
	updateDoc(t, ctx, collAt(t, b, "testdb", "feat", "items"),
		mustDoc(t, "_id", int32(1), "field", "theirs-val", "status", "active"))
	commitBranch(t, b, "testdb", "feat", "feat: theirs-val")

	conflicts := mergeExpectConflicts(t, b, "testdb", "main", "feat", "items")
	if len(conflicts) != 1 {
		t.Fatalf("setup: %d conflicts, want 1", len(conflicts))
	}
	return b, conflicts
}

// C1.
func TestResolveTheirsReindexes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	b, conflicts := mergeWithDocConflict(t)
	if err := resolveConflict(t, b, "testdb", "main", "items", conflicts[0].ConflictID, "theirs", nil); err != nil {
		t.Fatalf("resolve theirs: %v", err)
	}
	continueMerge(t, b, "testdb", "main")

	coll := collAt(t, b, "testdb", "main", "items")
	if got := equalityLookupIDs(t, ctx, coll, "field", "theirs-val"); !reflect.DeepEqual(got, []int32{1}) {
		t.Errorf("lookup by theirs-val = %v, want [1]", got)
	}
	if got := equalityLookupIDs(t, ctx, coll, "field", "ours-val"); len(got) != 0 {
		t.Errorf("lookup by ours-val = %v, want empty", got)
	}
}

// C2.
func TestResolveCustomReindexes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	b, conflicts := mergeWithDocConflict(t)
	custom := mustDoc(t, "_id", int32(1), "field", "custom-val", "status", "active")
	if err := resolveConflict(t, b, "testdb", "main", "items", conflicts[0].ConflictID, "custom", custom); err != nil {
		t.Fatalf("resolve custom: %v", err)
	}
	continueMerge(t, b, "testdb", "main")

	coll := collAt(t, b, "testdb", "main", "items")
	if got := equalityLookupIDs(t, ctx, coll, "field", "custom-val"); !reflect.DeepEqual(got, []int32{1}) {
		t.Errorf("lookup by custom-val = %v, want [1]", got)
	}
	for _, stale := range []string{"ours-val", "theirs-val", "alpha"} {
		if got := equalityLookupIDs(t, ctx, coll, "field", stale); len(got) != 0 {
			t.Errorf("lookup by %s = %v, want empty", stale, got)
		}
	}
}

// C1, deletion half.
func TestResolveTheirsDeleteUnindexes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "alpha"))
	coll := collAt(t, b, "testdb", "main", "items")
	createIndex(t, ctx, coll, "by_field", "field")
	commitDB(t, b, "testdb", "base")
	branchFrom(t, b, "testdb", "main", "feat")

	updateDoc(t, ctx, coll, mustDoc(t, "_id", int32(1), "field", "ours-val"))
	commitBranch(t, b, "testdb", "main", "main: modify")
	deleteByID(t, ctx, collAt(t, b, "testdb", "feat", "items"), int32(1))
	commitBranch(t, b, "testdb", "feat", "feat: delete")

	conflicts := mergeExpectConflicts(t, b, "testdb", "main", "feat", "items")
	if len(conflicts) != 1 {
		t.Fatalf("%d conflicts, want 1", len(conflicts))
	}
	if err := resolveConflict(t, b, "testdb", "main", "items", conflicts[0].ConflictID, "theirs", nil); err != nil {
		t.Fatalf("resolve theirs(delete): %v", err)
	}
	continueMerge(t, b, "testdb", "main")

	for _, v := range []string{"alpha", "ours-val"} {
		if got := equalityLookupIDs(t, ctx, coll, "field", v); len(got) != 0 {
			t.Errorf("lookup by %s after theirs-delete = %v, want empty", v, got)
		}
	}
}

// C4: a colliding resolution is rejected; the conflict stays
// unresolved.
func TestResolveUniqueCollisionRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "field", "alpha"))
	coll := collAt(t, b, "testdb", "main", "items")
	if _, err := coll.CreateIndexes(ctx, &backends.CreateIndexesParams{
		Indexes: []backends.IndexInfo{{
			Name:   "by_field_unique",
			Key:    []backends.IndexKeyPair{{Field: "field"}},
			Unique: true,
		}},
	}); err != nil {
		t.Fatalf("CreateIndexes: %v", err)
	}
	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(2), "field", "taken"))
	commitDB(t, b, "testdb", "base")
	branchFrom(t, b, "testdb", "main", "feat")

	updateDoc(t, ctx, coll, mustDoc(t, "_id", int32(1), "field", "ours-val"))
	commitBranch(t, b, "testdb", "main", "main: ours-val")
	updateDoc(t, ctx, collAt(t, b, "testdb", "feat", "items"), mustDoc(t, "_id", int32(1), "field", "theirs-val"))
	commitBranch(t, b, "testdb", "feat", "feat: theirs-val")

	conflicts := mergeExpectConflicts(t, b, "testdb", "main", "feat", "items")
	if len(conflicts) != 1 {
		t.Fatalf("%d conflicts, want 1", len(conflicts))
	}

	bad := mustDoc(t, "_id", int32(1), "field", "taken")
	if err := resolveConflict(t, b, "testdb", "main", "items", conflicts[0].ConflictID, "custom", bad); err == nil {
		t.Fatalf("colliding custom resolution unexpectedly succeeded")
	}

	good := mustDoc(t, "_id", int32(1), "field", "fresh")
	if err := resolveConflict(t, b, "testdb", "main", "items", conflicts[0].ConflictID, "custom", good); err != nil {
		t.Fatalf("non-colliding custom resolution failed: %v", err)
	}
	continueMerge(t, b, "testdb", "main")

	if got := equalityLookupIDs(t, ctx, coll, "field", "fresh"); !reflect.DeepEqual(got, []int32{1}) {
		t.Errorf("lookup by fresh = %v, want [1]", got)
	}
}

// C5: after a mixed resolution sequence and the concluding commit,
// every index describes exactly the committed documents.
func TestResolvedMergeCommitIndexConsistency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newTestBackend(t)

	for i := int32(1); i <= 4; i++ {
		insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", i, "field", "base", "n", i))
	}
	coll := collAt(t, b, "testdb", "main", "items")
	createIndex(t, ctx, coll, "by_field", "field")
	commitDB(t, b, "testdb", "base")
	branchFrom(t, b, "testdb", "main", "feat")

	for i := int32(1); i <= 4; i++ {
		updateDoc(t, ctx, coll, mustDoc(t, "_id", i, "field", "ours", "n", i))
	}
	commitBranch(t, b, "testdb", "main", "main: ours")
	featColl := collAt(t, b, "testdb", "feat", "items")
	for i := int32(1); i <= 4; i++ {
		updateDoc(t, ctx, featColl, mustDoc(t, "_id", i, "field", "theirs", "n", i))
	}
	commitBranch(t, b, "testdb", "feat", "feat: theirs")

	conflicts := mergeExpectConflicts(t, b, "testdb", "main", "feat", "items")
	if len(conflicts) != 4 {
		t.Fatalf("%d conflicts, want 4", len(conflicts))
	}

	resolutions := []struct {
		res   string
		value *types.Document
	}{
		{"ours", nil},
		{"theirs", nil},
		{"custom", nil}, // value filled below per conflict
		{"theirs", nil},
	}
	for i, c := range conflicts {
		r := resolutions[i%len(resolutions)]
		val := r.value
		if r.res == "custom" {
			var id int32
			if c.Ours != nil {
				v, _ := c.Ours.Get("_id")
				id = v.(int32)
			} else if c.Theirs != nil {
				v, _ := c.Theirs.Get("_id")
				id = v.(int32)
			}
			val = mustDoc(t, "_id", id, "field", "custom", "n", id)
		}
		if err := resolveConflict(t, b, "testdb", "main", "items", c.ConflictID, r.res, val); err != nil {
			t.Fatalf("resolve %s: %v", r.res, err)
		}
	}
	continueMerge(t, b, "testdb", "main")

	indexConsistentWithScan(t, ctx, coll, "field", []string{"base", "ours", "theirs", "custom"})
}
