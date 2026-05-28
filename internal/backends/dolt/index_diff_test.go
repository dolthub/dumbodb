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

// Tests for index-level surfacing in DumboDBStatus and DumboDBDiff.
// See workspace-gag.

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
)

// TestStatus_IndexAddedSurfaces: creating an index in the working set
// surfaces in dumboStatus as IndexesAdded.
func TestStatus_IndexAddedSurfaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "age", int32(30)))
	commitDB(t, b, "testdb", "seed")

	createIndex(t, ctx, collAt(t, b, "testdb", "main", "items"), "by_age", "age")

	res, err := b.DumboDBStatus(ctx, &backends.VersioningStatusParams{DBName: "testdb", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBStatus: %v", err)
	}
	if len(res.Tables) != 1 {
		t.Fatalf("expected one modified collection, got %d", len(res.Tables))
	}
	got := res.Tables[0]
	if got.Status != "modified" || got.Added != 0 || got.Modified != 0 || got.Deleted != 0 {
		t.Errorf("expected modified with zero doc changes, got %+v", got)
	}
	if !reflect.DeepEqual(got.AddedIndexes, []string{"by_age"}) {
		t.Errorf("AddedIndexes = %v, want [by_age]", got.AddedIndexes)
	}
	if len(got.ModifiedIndexes) != 0 || len(got.RemovedIndexes) != 0 {
		t.Errorf("expected no modified/removed indexes, got modified=%v removed=%v", got.ModifiedIndexes, got.RemovedIndexes)
	}
}

// TestStatus_IndexDeletedSurfaces: dropping an index in the working set
// surfaces as IndexesDeleted.
func TestStatus_IndexDeletedSurfaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newTestBackend(t)

	coll := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(1), "age", int32(30)))
	createIndex(t, ctx, coll, "by_age", "age")
	commitDB(t, b, "testdb", "seed + by_age")

	if _, err := coll.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: []string{"by_age"}}); err != nil {
		t.Fatalf("DropIndexes: %v", err)
	}

	res, err := b.DumboDBStatus(ctx, &backends.VersioningStatusParams{DBName: "testdb", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBStatus: %v", err)
	}
	if len(res.Tables) != 1 {
		t.Fatalf("expected one modified collection, got %d", len(res.Tables))
	}
	got := res.Tables[0]
	if !reflect.DeepEqual(got.RemovedIndexes, []string{"by_age"}) {
		t.Errorf("RemovedIndexes = %v, want [by_age]", got.RemovedIndexes)
	}
}

// TestStatus_IndexChangedSurfaces: drop+recreate with the same name and
// a different definition surfaces as IndexesChanged. This is the case
// that disproved "indexes are immutable by name" for diff purposes.
func TestStatus_IndexChangedSurfaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newTestBackend(t)

	coll := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(1), "age", int32(30), "name", "alpha"))
	createIndex(t, ctx, coll, "by_x", "age")
	commitDB(t, b, "testdb", "seed + by_x on age")

	if _, err := coll.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: []string{"by_x"}}); err != nil {
		t.Fatalf("DropIndexes: %v", err)
	}
	createIndex(t, ctx, coll, "by_x", "name")

	res, err := b.DumboDBStatus(ctx, &backends.VersioningStatusParams{DBName: "testdb", Branch: "main"})
	if err != nil {
		t.Fatalf("DumboDBStatus: %v", err)
	}
	if len(res.Tables) != 1 {
		t.Fatalf("expected one modified collection, got %d", len(res.Tables))
	}
	got := res.Tables[0]
	if !reflect.DeepEqual(got.ModifiedIndexes, []string{"by_x"}) {
		t.Errorf("ModifiedIndexes = %v, want [by_x]", got.ModifiedIndexes)
	}
	if len(got.AddedIndexes) != 0 || len(got.RemovedIndexes) != 0 {
		t.Errorf("expected only ModifiedIndexes; got added=%v removed=%v", got.AddedIndexes, got.RemovedIndexes)
	}
}

// TestDiff_IndexAddedSurfaces: a create-index-only working set must
// produce a non-empty diff with the index's full definition.
func TestDiff_IndexAddedSurfaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newTestBackend(t)

	coll := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(1), "age", int32(30)))
	commitDB(t, b, "testdb", "seed")

	createIndex(t, ctx, coll, "by_age", "age")

	res, err := b.DumboDBDiff(ctx, &backends.DiffParams{DBName: "testdb", ConnRootish: "main"})
	if err != nil {
		t.Fatalf("DumboDBDiff: %v", err)
	}
	if len(res.Collections) != 1 {
		t.Fatalf("expected one collection in diff, got %d: %+v", len(res.Collections), res.Collections)
	}
	got := res.Collections[0]
	if got.Name != "items" || got.Status != "modified" {
		t.Errorf("collection = %+v, want name=items status=modified", got)
	}
	if len(got.AddedIndexes) != 1 {
		t.Fatalf("expected one added index, got %d", len(got.AddedIndexes))
	}
	if len(got.ModifiedIndexes) != 0 || len(got.RemovedIndexes) != 0 {
		t.Errorf("expected only AddedIndexes; got modified=%v removed=%v", got.ModifiedIndexes, got.RemovedIndexes)
	}
	idx := got.AddedIndexes[0]
	if idx.Name != "by_age" || len(idx.Key) != 1 || idx.Key[0].Field != "age" {
		t.Errorf("added index = %+v, want by_age on field age", idx)
	}
}

// TestDiff_IndexChangedShowsBothDefinitions: a same-name drop+recreate
// with a different spec must surface as a ModifiedIndexes entry
// carrying both From (old spec) and To (new spec).
func TestDiff_IndexChangedShowsBothDefinitions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newTestBackend(t)

	coll := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(1), "age", int32(30), "name", "alpha"))
	createIndex(t, ctx, coll, "by_x", "age")
	commitDB(t, b, "testdb", "seed + by_x on age")

	if _, err := coll.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: []string{"by_x"}}); err != nil {
		t.Fatalf("DropIndexes: %v", err)
	}
	createIndex(t, ctx, coll, "by_x", "name")

	res, err := b.DumboDBDiff(ctx, &backends.DiffParams{DBName: "testdb", ConnRootish: "main"})
	if err != nil {
		t.Fatalf("DumboDBDiff: %v", err)
	}
	if len(res.Collections) != 1 {
		t.Fatalf("expected one collection in diff, got %d", len(res.Collections))
	}
	got := res.Collections[0]
	if len(got.AddedIndexes) != 0 || len(got.RemovedIndexes) != 0 {
		t.Errorf("expected only ModifiedIndexes; got added=%v removed=%v", got.AddedIndexes, got.RemovedIndexes)
	}
	if len(got.ModifiedIndexes) != 1 {
		t.Fatalf("expected one modified index, got %d", len(got.ModifiedIndexes))
	}
	ch := got.ModifiedIndexes[0]
	if ch.From.Name != "by_x" || ch.To.Name != "by_x" {
		t.Errorf("change names = (%q, %q), want both by_x", ch.From.Name, ch.To.Name)
	}
	if len(ch.From.Key) != 1 || ch.From.Key[0].Field != "age" {
		t.Errorf("From key = %+v, want field=age", ch.From.Key)
	}
	if len(ch.To.Key) != 1 || ch.To.Key[0].Field != "name" {
		t.Errorf("To key = %+v, want field=name", ch.To.Key)
	}
}

// TestLog_StatAndPatch_IncludeIndexChanges: a commit that only changes
// indexes must surface in both dumboLog --stat and --patch outputs.
// Before this fix, the patch filter dropped it because the doc diff
// was empty.
func TestLog_StatAndPatch_IncludeIndexChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newTestBackend(t)

	coll := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(1), "age", int32(30), "name", "alpha"))
	commitDB(t, b, "testdb", "c1: seed")

	// c2: index-only commit. No documents change. by_age added.
	createIndex(t, ctx, coll, "by_age", "age")
	commitDB(t, b, "testdb", "c2: add by_age")

	// c3: drop+recreate by_age with a different field. Indexes "modified".
	if _, err := coll.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: []string{"by_age"}}); err != nil {
		t.Fatalf("DropIndexes: %v", err)
	}
	createIndex(t, ctx, coll, "by_age", "name")
	commitDB(t, b, "testdb", "c3: by_age now on name")

	res, err := b.DumboDBLog(ctx, &backends.LogParams{
		DBName: "testdb", Branch: "main", Limit: 3, Stat: true, Patch: true,
	})
	if err != nil {
		t.Fatalf("DumboDBLog: %v", err)
	}
	if len(res.Commits) < 3 {
		t.Fatalf("expected at least 3 commits, got %d", len(res.Commits))
	}

	// Commits are returned newest-first. res.Commits[0] is c3, [1] is c2.
	c3 := res.Commits[0]
	c2 := res.Commits[1]

	// c3: modified by_age.
	if len(c3.Stat) != 1 {
		t.Fatalf("c3 stat: expected one collection, got %d", len(c3.Stat))
	}
	if !reflect.DeepEqual(c3.Stat[0].ModifiedIndexes, []string{"by_age"}) {
		t.Errorf("c3 stat ModifiedIndexes = %v, want [by_age]", c3.Stat[0].ModifiedIndexes)
	}
	if len(c3.Diff) != 1 || len(c3.Diff[0].ModifiedIndexes) != 1 {
		t.Fatalf("c3 diff: expected one collection with one modified index, got %+v", c3.Diff)
	}
	ch := c3.Diff[0].ModifiedIndexes[0]
	if ch.From.Name != "by_age" || ch.To.Name != "by_age" {
		t.Errorf("c3 modified change names = (%q, %q), want both by_age", ch.From.Name, ch.To.Name)
	}

	// c2: added by_age. This is the bug fix case -- previously the patch
	// filter dropped it because addedDocs/removedDocs/modifiedDocs were
	// all empty.
	if len(c2.Stat) != 1 || !reflect.DeepEqual(c2.Stat[0].AddedIndexes, []string{"by_age"}) {
		t.Errorf("c2 stat = %+v, want AddedIndexes=[by_age]", c2.Stat)
	}
	if len(c2.Diff) != 1 {
		t.Fatalf("c2 diff: expected one collection in patch (was dropped before the fix), got %d", len(c2.Diff))
	}
	if len(c2.Diff[0].AddedIndexes) != 1 {
		t.Fatalf("c2 diff AddedIndexes = %+v, want one entry", c2.Diff[0].AddedIndexes)
	}
	if c2.Diff[0].AddedIndexes[0].Name != "by_age" {
		t.Errorf("c2 added entry name = %q, want by_age", c2.Diff[0].AddedIndexes[0].Name)
	}
}

// TestDiff_MultipleIndexChanges: combinations of added, deleted, and
// modified are all surfaced in stable name order.
func TestDiff_MultipleIndexChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newTestBackend(t)

	coll := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, coll, mustDoc(t, "_id", int32(1), "age", int32(30), "name", "alpha"))
	createIndex(t, ctx, coll, "by_age", "age")
	createIndex(t, ctx, coll, "by_swap", "age")
	commitDB(t, b, "testdb", "seed + by_age + by_swap")

	// Working-set changes: drop by_age, drop+recreate by_swap with new
	// field, add by_name.
	if _, err := coll.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: []string{"by_age", "by_swap"}}); err != nil {
		t.Fatalf("DropIndexes: %v", err)
	}
	createIndex(t, ctx, coll, "by_swap", "name")
	createIndex(t, ctx, coll, "by_name", "name")

	res, err := b.DumboDBDiff(ctx, &backends.DiffParams{DBName: "testdb", ConnRootish: "main"})
	if err != nil {
		t.Fatalf("DumboDBDiff: %v", err)
	}
	if len(res.Collections) != 1 {
		t.Fatalf("expected one collection in diff, got %d", len(res.Collections))
	}
	got := res.Collections[0]

	// Each list is itself sorted by name. AddedIndexes carries
	// "by_name", RemovedIndexes carries "by_age", ModifiedIndexes
	// carries "by_swap".
	addedNames := indexInfoNames(got.AddedIndexes)
	removedNames := indexInfoNames(got.RemovedIndexes)
	modifiedNames := indexChangeNames(got.ModifiedIndexes)
	if !sort.StringsAreSorted(addedNames) {
		t.Errorf("added not sorted: %v", addedNames)
	}
	if !sort.StringsAreSorted(removedNames) {
		t.Errorf("removed not sorted: %v", removedNames)
	}
	if !sort.StringsAreSorted(modifiedNames) {
		t.Errorf("modified not sorted: %v", modifiedNames)
	}
	if !reflect.DeepEqual(addedNames, []string{"by_name"}) {
		t.Errorf("added = %v, want [by_name]", addedNames)
	}
	if !reflect.DeepEqual(removedNames, []string{"by_age"}) {
		t.Errorf("removed = %v, want [by_age]", removedNames)
	}
	if !reflect.DeepEqual(modifiedNames, []string{"by_swap"}) {
		t.Errorf("modified = %v, want [by_swap]", modifiedNames)
	}
}

// indexInfoNames extracts names from a []IndexInfo for test assertions.
func indexInfoNames(infos []backends.IndexInfo) []string {
	out := make([]string, len(infos))
	for i, info := range infos {
		out[i] = info.Name
	}
	return out
}

// indexChangeNames extracts From.Name from each IndexChange.
func indexChangeNames(changes []backends.IndexChange) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = c.From.Name
	}
	return out
}
