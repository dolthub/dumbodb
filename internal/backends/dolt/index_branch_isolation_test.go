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

// Tests asserting that secondary index metadata and data are branch-scoped.
// See docs/design/branch-scoped-index-metadata.md section 5.2.
//
// These tests fail today (commit 5f06cd8 and earlier) because dbState's
// index caches (state.indexes, state.secIndexMaps, state.collIndexAMs) have
// no branch dimension. They will pass once the design's resolver path
// (section 3.3) lands and the caches are deleted.

import (
	"context"
	"reflect"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
)

// TestIndexCreated_OnFeat_NotVisibleOnMain demonstrates the cross-branch
// metadata leak: creating an index on one branch must not show up in
// listIndexes on another branch.
func TestIndexCreated_OnFeat_NotVisibleOnMain(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "age", int32(30)))
	commitDB(t, b, "testdb", "seed on main")

	branchFrom(t, b, "testdb", "main", "feat")

	createIndex(t, ctx, collAt(t, b, "testdb", "feat", "items"), "by_age", "age")
	commitBranch(t, b, "testdb", "feat", "feat: add by_age")

	gotMain := listIndexNames(t, ctx, collAt(t, b, "testdb", "main", "items"))
	wantMain := []string{backends.DefaultIndexName}
	if !reflect.DeepEqual(gotMain, wantMain) {
		t.Errorf("listIndexes on main = %v, want %v (feat's by_age leaked)", gotMain, wantMain)
	}

	gotFeat := listIndexNames(t, ctx, collAt(t, b, "testdb", "feat", "items"))
	wantFeat := []string{backends.DefaultIndexName, "by_age"}
	if !reflect.DeepEqual(gotFeat, wantFeat) {
		t.Errorf("listIndexes on feat = %v, want %v", gotFeat, wantFeat)
	}
}

// TestIndexCreated_AfterReopen_VisibleOnOriginatingBranch demonstrates the
// eager-hydrate default-branch bias: an index created on a non-default
// branch must survive a backend reopen and remain visible on that branch.
func TestIndexCreated_AfterReopen_VisibleOnOriginatingBranch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	dir := t.TempDir()
	b := openBackendInDir(t, dir)

	insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", int32(1), "age", int32(30)))
	commitDB(t, b, "testdb", "seed on main")

	branchFrom(t, b, "testdb", "main", "feat")
	createIndex(t, ctx, collAt(t, b, "testdb", "feat", "items"), "by_age", "age")
	insertOne(t, ctx, collAt(t, b, "testdb", "feat", "items"), mustDoc(t, "_id", int32(2), "age", int32(40)))
	commitBranch(t, b, "testdb", "feat", "feat: add by_age and a doc")

	b.Close()

	b2 := openBackendInDir(t, dir)
	t.Cleanup(b2.Close)

	got := listIndexNames(t, ctx, collAt(t, b2, "testdb", "feat", "items"))
	want := []string{backends.DefaultIndexName, "by_age"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after reopen, listIndexes on feat = %v, want %v", got, want)
	}

	ids := equalityLookupIDs(t, ctx, collAt(t, b2, "testdb", "feat", "items"), "age", int32(40))
	wantIDs := []int32{2}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Errorf("after reopen, lookup age=40 on feat = %v, want %v", ids, wantIDs)
	}
}

// TestIndexLookup_OnEachBranch_SeesOnlyOwnData is the canonical "interleaved
// writes diverge" test: two branches each insert a disjoint half of an
// indexed value space. Each branch's indexed lookup must return only that
// branch's data.
func TestIndexLookup_OnEachBranch_SeesOnlyOwnData(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	mainColl := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, mainColl, mustDoc(t, "_id", int32(1), "name", "alpha"))
	createIndex(t, ctx, mainColl, "by_first_letter", "name")
	commitDB(t, b, "testdb", "seed + index on main")

	branchFrom(t, b, "testdb", "main", "am")
	branchFrom(t, b, "testdb", "main", "nz")

	amWords := []string{"bravo", "charlie", "delta", "echo", "foxtrot", "golf",
		"hotel", "india", "juliet", "kilo", "lima", "mike"}
	amColl := collAt(t, b, "testdb", "am", "items")
	for i, w := range amWords {
		insertOne(t, ctx, amColl, mustDoc(t, "_id", int32(100+i), "name", w))
	}
	commitBranch(t, b, "testdb", "am", "am bulk insert")

	nzWords := []string{"november", "oscar", "papa", "quebec", "romeo", "sierra",
		"tango", "uniform", "victor", "whiskey", "xray", "yankee", "zulu"}
	nzColl := collAt(t, b, "testdb", "nz", "items")
	for i, w := range nzWords {
		insertOne(t, ctx, nzColl, mustDoc(t, "_id", int32(200+i), "name", w))
	}
	commitBranch(t, b, "testdb", "nz", "nz bulk insert")

	type tcase struct {
		branch string
		value  string
		want   []int32
	}
	cases := []tcase{
		{branch: "am", value: "mike", want: []int32{111}},
		{branch: "am", value: "zulu", want: nil},
		{branch: "am", value: "alpha", want: []int32{1}},
		{branch: "nz", value: "zulu", want: []int32{212}},
		{branch: "nz", value: "mike", want: nil},
		{branch: "nz", value: "alpha", want: []int32{1}},
		{branch: "main", value: "alpha", want: []int32{1}},
		{branch: "main", value: "mike", want: nil},
		{branch: "main", value: "zulu", want: nil},
	}
	for _, tc := range cases {
		got := equalityLookupIDs(t, ctx, collAt(t, b, "testdb", tc.branch, "items"), "name", tc.value)
		if !equalInt32Slices(got, tc.want) {
			t.Errorf("lookup name=%q on %s = %v, want %v", tc.value, tc.branch, got, tc.want)
		}
	}

	// Count-based assertions are the part that today's primary-fetch
	// disambiguation can NOT mask: tryIndexedCount returns index-entry
	// counts directly, with no primary lookup to filter out cross-branch
	// pollution. If state.secIndexMaps is shared across branches, am's
	// index sees nz's writes and the count for "zulu" on am is non-zero.
	type countCase struct {
		branch string
		value  string
		want   int64
	}
	countCases := []countCase{
		{branch: "am", value: "mike", want: 1},
		{branch: "am", value: "zulu", want: 0},
		{branch: "nz", value: "mike", want: 0},
		{branch: "nz", value: "zulu", want: 1},
		{branch: "main", value: "mike", want: 0},
		{branch: "main", value: "zulu", want: 0},
	}
	for _, tc := range countCases {
		got := indexedCount(t, ctx, collAt(t, b, "testdb", tc.branch, "items"), "name", tc.value)
		if got != tc.want {
			t.Errorf("Count name=%q on %s = %d, want %d (index polluted across branches)",
				tc.value, tc.branch, got, tc.want)
		}
	}
}

// TestWrite_OnFeat_DoesNotCorruptMainDTBL demonstrates that a write on one
// branch must not corrupt another branch's on-disk DTBL by inlining the
// wrong secondary_indexes AddressMap.
func TestWrite_OnFeat_DoesNotCorruptMainDTBL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	mainColl := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, mainColl, mustDoc(t, "_id", int32(1), "age", int32(30), "name", "alpha"))
	createIndex(t, ctx, mainColl, "by_age", "age")
	commitDB(t, b, "testdb", "main: seed + by_age")

	branchFrom(t, b, "testdb", "main", "feat")

	featColl := collAt(t, b, "testdb", "feat", "items")
	createIndex(t, ctx, featColl, "by_name", "name")
	insertOne(t, ctx, featColl, mustDoc(t, "_id", int32(2), "age", int32(40), "name", "beta"))
	commitBranch(t, b, "testdb", "feat", "feat: by_name and second doc")

	// Force a DTBL re-write on main. With the cross-branch cache leak,
	// this is the step that bakes feat's index AM into main's on-disk
	// DTBL.
	insertOne(t, ctx, collAt(t, b, "testdb", "main", "items"), mustDoc(t, "_id", int32(3), "age", int32(31), "name", "gamma"))
	commitDB(t, b, "testdb", "main: another doc")

	mainOnDisk := indexAMOnDisk(t, ctx, b, "testdb", "main", "items")
	gotMain := indexNamesInAM(t, ctx, mainOnDisk)
	wantMain := []string{"by_age"}
	if !reflect.DeepEqual(gotMain, wantMain) {
		t.Errorf("main's on-disk DTBL secondary_indexes = %v, want %v "+
			"(feat's by_name leaked into main's DTBL)", gotMain, wantMain)
	}

	featOnDisk := indexAMOnDisk(t, ctx, b, "testdb", "feat", "items")
	gotFeat := indexNamesInAM(t, ctx, featOnDisk)
	wantFeat := []string{"by_name"}
	if !reflect.DeepEqual(gotFeat, wantFeat) {
		t.Errorf("feat's on-disk DTBL secondary_indexes = %v, want %v", gotFeat, wantFeat)
	}
}

// TestIndexes_Within_SameSession_BranchSwitch exercises the
// switch-branch-mid-session path explicitly: a single backend, getting
// branch-pinned database handles in sequence, must surface each branch's
// own indexes.
func TestIndexes_Within_SameSession_BranchSwitch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	b := newTestBackend(t)

	mainColl := collAt(t, b, "testdb", "main", "items")
	insertOne(t, ctx, mainColl, mustDoc(t, "_id", int32(1), "age", int32(30), "name", "alpha"))
	createIndex(t, ctx, mainColl, "by_age", "age")
	commitDB(t, b, "testdb", "main: seed + by_age")

	branchFrom(t, b, "testdb", "main", "feat")

	featColl := collAt(t, b, "testdb", "feat", "items")
	if _, err := featColl.DropIndexes(ctx, &backends.DropIndexesParams{Indexes: []string{"by_age"}}); err != nil {
		t.Fatalf("DropIndexes on feat: %v", err)
	}
	createIndex(t, ctx, featColl, "by_name", "name")
	commitBranch(t, b, "testdb", "feat", "feat: swap by_age for by_name")

	gotMain := listIndexNames(t, ctx, collAt(t, b, "testdb", "main", "items"))
	wantMain := []string{backends.DefaultIndexName, "by_age"}
	if !reflect.DeepEqual(gotMain, wantMain) {
		t.Errorf("listIndexes on main after feat changes = %v, want %v", gotMain, wantMain)
	}

	gotFeat := listIndexNames(t, ctx, collAt(t, b, "testdb", "feat", "items"))
	wantFeat := []string{backends.DefaultIndexName, "by_name"}
	if !reflect.DeepEqual(gotFeat, wantFeat) {
		t.Errorf("listIndexes on feat = %v, want %v", gotFeat, wantFeat)
	}
}

// equalInt32Slices returns true if a and b contain the same elements (both
// empty / nil are treated as equal).
func equalInt32Slices(a, b []int32) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}
