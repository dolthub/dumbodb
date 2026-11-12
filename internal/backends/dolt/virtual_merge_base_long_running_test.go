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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

// longRunningHistory builds Dolt scenarios 2a and 2b on collection items in |mode|: a long-running feature branch
// and a schema branch that main and feature merge every cycle:
//
//	c0 -- c1 -- main1 -- m1 -- ... -- main6 -- m6                  main
//	 |     \-- feat1 -- f1 -- ... -- sync -- ... -- feat6 -- f6   feature
//	 \-- s1 -- s2 -- s3 -- s4 -- s5 -- s6                        schema
//	m<i> = merge(main<i>, s<i>), f<i> = merge(feat<i>, s<i>), sync = merge(f3, m3)
//
// c1 seeds documents 1 to 3 with v and w "seed". In cycle i, s<i> adds an index on x<i>; main<i> adds document
// 100+i and sets document 1's v to "main <i>"; feat<i> adds document 200+i. main5 also deletes document 2. When
// |conflict| (2b), feat5 sets document 1's v to "feature 5", and main5 and feat5 set document 3's v and w.
func longRunningHistory(t *testing.T, b *Backend, mode string, conflict bool) {
	t.Helper()
	ctx := context.Background()
	item := func(id int32, v, w string) *types.Document { return mustDoc(t, "_id", id, "v", v, "w", w) }
	createItems(t, b, mode)
	commitDB(t, b, "testdb", "c0")
	branchFrom(t, b, "testdb", "main", "schema")
	for id := int32(1); id <= 3; id++ {
		insertDoc(t, b, "testdb", "items", item(id, "seed", "seed"))
	}
	commitDB(t, b, "testdb", "c1")
	branchFrom(t, b, "testdb", "main", "feature")

	for i := int32(1); i <= 6; i++ {
		createIndex(t, ctx, collAt(t, b, "testdb", "schema", "items"), fmt.Sprintf("by_x%d", i), fmt.Sprintf("x%d", i))
		commitBranch(t, b, "testdb", "schema", fmt.Sprintf("s%d", i))

		insertDoc(t, b, "testdb", "items", mustDoc(t, "_id", 100+i, "v", "main"))
		updateDocOnBranch(t, b, "testdb", "main", "items", item(1, fmt.Sprintf("main %d", i), "seed"))
		if i == 5 {
			deleteID(t, b, "testdb", "main", "items", int32(2))
			if conflict {
				updateDocOnBranch(t, b, "testdb", "main", "items", item(3, "main", "seed"))
			}
		}
		commitDB(t, b, "testdb", fmt.Sprintf("main%d", i))
		mergeBranches(t, b, "testdb", "main", "schema")
		if i == 3 {
			branchFrom(t, b, "testdb", "main", "m3")
		}

		insertDoc(t, b, "testdb@feature", "items", mustDoc(t, "_id", 200+i, "v", "feature"))
		if conflict && i == 5 {
			updateDocOnBranch(t, b, "testdb", "feature", "items", item(1, "feature 5", "seed"))
			updateDocOnBranch(t, b, "testdb", "feature", "items", item(3, "seed", "feature"))
		}
		commitBranch(t, b, "testdb", "feature", fmt.Sprintf("feat%d", i))
		mergeBranches(t, b, "testdb", "feature", "schema")
		if i == 3 {
			mergeBranches(t, b, "testdb", "feature", "main")
		}
	}
	requireMergeBases(t, b, "main", "feature", "schema", "m3")
}

// longRunningItems is items after merging the long-running branches, with document 1 at |doc1| and document 3 at
// |doc3|, or absent when nil.
func longRunningItems(t *testing.T, doc1, doc3 *types.Document) map[int32]*types.Document {
	want := map[int32]*types.Document{}
	for _, doc := range []*types.Document{doc1, doc3} {
		if doc != nil {
			id, _ := doc.Get("_id")
			want[id.(int32)] = doc
		}
	}
	for i := int32(1); i <= 6; i++ {
		want[100+i] = mustDoc(t, "_id", 100+i, "v", "main")
		want[200+i] = mustDoc(t, "_id", 200+i, "v", "feature")
	}
	return want
}

func requireItems(t *testing.T, b *Backend, branch string, want map[int32]*types.Document) {
	t.Helper()
	got := virtualBaseItems(t, b, branchHash(t, b, branch))
	require.Len(t, got, len(want), "documents on %s", branch)
	for id, doc := range want {
		requireSameDoc(t, doc, got[id], "document %d on %s", id, branch)
	}
}

// requireWorkingItems checks items in |branch|'s working set.
func requireWorkingItems(t *testing.T, b *Backend, branch string, want map[int32]*types.Document) {
	t.Helper()
	got := drainQuery(t, context.Background(), collAt(t, b, "testdb", branch, "items"), &backends.QueryParams{})
	require.Len(t, got, len(want), "documents on %s", branch)
	for _, doc := range got {
		id, _ := doc.Get("_id")
		requireSameDoc(t, want[id.(int32)], doc, "document %d on %s", id, branch)
	}
}

var longRunningDirections = []struct{ into, from string }{{"feature", "main"}, {"main", "feature"}}

// A3 (Dolt 2a): the long-running branches merge cleanly in every mode.
func TestVirtualMergeBase_LongRunning(t *testing.T) {
	for _, mode := range mergeModes {
		for _, d := range longRunningDirections {
			t.Run(mode+"/merge "+d.from+" into "+d.into, func(t *testing.T) {
				useTestCommitClock(t)
				b := newTestBackend(t)
				longRunningHistory(t, b, mode, false)
				mergeBranches(t, b, "testdb", d.into, d.from)
				want := longRunningItems(t,
					mustDoc(t, "_id", int32(1), "v", "main 6", "w", "seed"),
					mustDoc(t, "_id", int32(3), "v", "seed", "w", "seed"))
				requireItems(t, b, d.into, want)
			})
		}
	}
}

// A4 and A5 (Dolt 2b): document 1 conflicts in every mode. Document 3, where main set v and feature set w, conflicts
// in the document modes and merges in the field modes. A6: the conflict on document 1 resolves with ours, theirs or
// a custom document, or the merge aborts.
func TestVirtualMergeBase_LongRunningConflict(t *testing.T) {
	resolutions := []string{"ours", "theirs", "custom", "abort"}
	for _, mode := range mergeModes {
		for _, d := range longRunningDirections {
			for _, resolution := range resolutions {
				t.Run(mode+"/merge "+d.from+" into "+d.into+"/"+resolution, func(t *testing.T) {
					useTestCommitClock(t)
					b := newTestBackend(t)
					ctx := context.Background()
					longRunningHistory(t, b, mode, true)
					doc1 := map[string]*types.Document{
						"main":    mustDoc(t, "_id", int32(1), "v", "main 6", "w", "seed"),
						"feature": mustDoc(t, "_id", int32(1), "v", "feature 5", "w", "seed"),
					}
					doc3 := map[string]*types.Document{
						"main":    mustDoc(t, "_id", int32(3), "v", "main", "w", "seed"),
						"feature": mustDoc(t, "_id", int32(3), "v", "seed", "w", "feature"),
					}
					before := branchHash(t, b, d.into)
					beforeItems := virtualBaseItems(t, b, before)

					_, err := b.DumboDBMerge(ctx, &backends.MergeParams{
						DBName: "testdb", Into: d.into, From: d.from, Message: "merge " + d.from, Author: "testuser <test@example.com>",
					})
					require.Error(t, err, "the merge must conflict")
					c1 := conflictOnDoc(t, b, d.into, 1)
					requireSameDoc(t, doc1[d.into], c1.Ours, "ours of document 1")
					requireSameDoc(t, doc1[d.from], c1.Theirs, "theirs of document 1")
					documentMode := mode == string(MergeModeDocumentTouched) || mode == string(MergeModeDocumentDivergent)
					res, err := b.DumboDBConflicts(ctx, &backends.ConflictsParams{DBName: "testdb", Branch: d.into})
					require.NoError(t, err)
					require.Len(t, res.Collections, 1)
					if documentMode {
						require.Len(t, res.Collections[0].Conflicts, 2, "documents 1 and 3 must conflict")
						conflictOnDoc(t, b, d.into, 3)
					} else {
						require.Len(t, res.Collections[0].Conflicts, 1, "only document 1 must conflict")
					}

					if resolution == "abort" {
						_, err := b.DumboDBMerge(ctx, &backends.MergeParams{DBName: "testdb", Into: d.into, Abort: true})
						require.NoError(t, err)
						require.Equal(t, before, branchHash(t, b, d.into))
						requireWorkingItems(t, b, d.into, beforeItems)
						return
					}

					want1 := map[string]*types.Document{
						"ours": doc1[d.into], "theirs": doc1[d.from], "custom": mustDoc(t, "_id", int32(1), "v", "custom"),
					}[resolution]
					var value *types.Document
					if resolution == "custom" {
						value = want1
					}
					require.NoError(t, resolveConflict(t, b, "testdb", d.into, "items", c1.ConflictID, resolution, value))
					want3 := mustDoc(t, "_id", int32(3), "v", "main", "w", "feature")
					if documentMode {
						c3 := conflictOnDoc(t, b, d.into, 3)
						require.NoError(t, resolveConflict(t, b, "testdb", d.into, "items", c3.ConflictID, "ours", nil))
						want3 = doc3[d.into]
					}
					_, err = b.DumboDBMerge(ctx, &backends.MergeParams{
						DBName: "testdb", Into: d.into, Continue: true, Message: "merge continued", Author: "testuser <test@example.com>",
					})
					require.NoError(t, err)
					requireItems(t, b, d.into, longRunningItems(t, want1, want3))
				})
			}
		}
	}
}
