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
	"fmt"
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
)

// Section E: m1 and m2 merge the same three branches in opposite orders, so their merge bases are p1, p2 and p3:
//
//	c1 -- m1 start -- merge p1 -- merge p2 -- merge p3 -- m1    m1
//	 |\-- m2 start -- merge p3 -- merge p2 -- merge p1 -- m2    m2
//	 |\-- p1
//	 |\-- p2
//	 \-- p3
//
// m1 start and m2 start add documents 10 and 20, with u 10 and 20.

func abItem(id int32, a, b string) func(t *testing.T) *types.Document {
	return func(t *testing.T) *types.Document { return mustDoc(t, "_id", id, "a", a, "b", b) }
}

func setAB(id int32, a, b string) edit { return setTo(abItem(id, a, b)) }

// abItems creates items in c1 with documents 1 to |n|.
func abItems(n int32) func(t *testing.T, b *Backend, mode string) {
	return func(t *testing.T, b *Backend, mode string) {
		createItems(t, b, mode)
		for id := int32(1); id <= n; id++ {
			insertDoc(t, b, "testdb", "items", abItem(id, "seed", "seed")(t))
		}
	}
}

// threeBasesHistory builds the history above, with |setup| building c1. Each of m1 and m2 merges p1, p2 and
// p3 in its order, resolving conflicts with ours and then |resolve|[name of the merged branch], and finally applies
// |m1| or |m2|.
func threeBasesHistory(t *testing.T, b *Backend, mode string, setup func(t *testing.T, b *Backend, mode string), p1, p2, p3 edit, m1Resolve, m2Resolve map[string]edit, m1, m2 edit) {
	t.Helper()
	setup(t, b, mode)
	commitDB(t, b, "testdb", "c1")
	for _, branch := range []string{"p1", "p2", "p3", "m1", "m2"} {
		branchFrom(t, b, "testdb", "main", branch)
	}
	for i, e := range []edit{p1, p2, p3} {
		branch := fmt.Sprintf("p%d", i+1)
		e(t, b, branch)
		commitBranch(t, b, "testdb", branch, branch)
	}
	insertDoc(t, b, "testdb@m1", "items", uDoc(t, 10, 10))
	commitBranch(t, b, "testdb", "m1", "m1 start")
	insertDoc(t, b, "testdb@m2", "items", uDoc(t, 20, 20))
	commitBranch(t, b, "testdb", "m2", "m2 start")
	for _, p := range []string{"p1", "p2", "p3"} {
		mergeResolving(t, b, "m1", p, m1Resolve[p])
	}
	for _, p := range []string{"p3", "p2", "p1"} {
		mergeResolving(t, b, "m2", p, m2Resolve[p])
	}
	m1(t, b, "m1")
	commitBranch(t, b, "testdb", "m1", "m1")
	m2(t, b, "m2")
	commitBranch(t, b, "testdb", "m2", "m2")
	requireMergeBases(t, b, "m1", "m2", "p3", "p2", "p1")
}

// E1 (Dolt 5a): p1 and p2 conflict on document 1's a, and both tips resolve it to "resolved". p1 also sets document
// 2's a, p2 document 3's a, and p3 document 4's b. m1 then edits document 2's b, and m2 document 3's b.
func threeBasesOneConflict(t *testing.T, b *Backend, mode string) {
	t.Helper()
	resolved := setAB(1, "resolved", "seed")
	threeBasesHistory(t, b, mode, abItems(4),
		editAll(setAB(1, "p1", "seed"), setAB(2, "p1", "seed")),
		editAll(setAB(1, "p2", "seed"), setAB(3, "p2", "seed")),
		setAB(4, "seed", "p3"),
		map[string]edit{"p2": resolved}, map[string]edit{"p1": resolved},
		setAB(2, "p1", "m1"), setAB(3, "p2", "m2"))
}

// E2 and E3 (Dolt 6f1, 6f2): p1 sets document 1's a and deletes document 2; p2 sets document 1's a and b and
// document 3's b; p3 sets document 1's b and document 2's a. So p1 and p2 conflict on document 1's a, p2 and p3 on
// its b, and p1 and p3 on document 2. m1 deletes document 2 and m2 keeps c1's; then they set document 1 to
// |m1Doc1| and |m2Doc1|.
func threeBasesFoldedConflicts(m1Doc1, m2Doc1 func(t *testing.T) *types.Document) func(t *testing.T, b *Backend, mode string) {
	return func(t *testing.T, b *Backend, mode string) {
		t.Helper()
		threeBasesHistory(t, b, mode, abItems(3),
			editAll(setAB(1, "p1", "seed"), deleteItem(2)),
			editAll(setAB(1, "p2", "p2"), setAB(3, "seed", "p2")),
			editAll(setAB(1, "seed", "p3"), setAB(2, "p3", "seed")),
			map[string]edit{"p2": setAB(1, "p1", "seed"), "p3": deleteItem(2)},
			map[string]edit{"p2": setAB(1, "seed", "p3"), "p1": setAB(2, "p3", "seed")},
			setTo(m1Doc1), editAll(setTo(m2Doc1), setAB(2, "seed", "seed")))
	}
}

func TestVirtualMergeBase_ThreeMergeBases(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T, b *Backend, mode string)
		want  func(t *testing.T) map[string]outerResult
	}{
		{
			name:  "E1 two merge bases conflict, tips resolve alike",
			build: threeBasesOneConflict,
			want: func(t *testing.T) map[string]outerResult {
				resolved := merges(abItem(1, "resolved", "seed")(t))
				return outcomes(conflicts(), conflicts(), resolved, resolved)
			},
		},
		{
			name:  "E2 conflicts in different folds, tips disagree on a",
			build: threeBasesFoldedConflicts(abItem(1, "m1", "both"), abItem(1, "seed", "both")),
			want: func(t *testing.T) map[string]outerResult {
				return outcomes(conflicts(), conflicts(), conflicts(), conflicts())
			},
		},
		{
			name:  "E3 conflicts in different folds, tips disagree on b",
			build: threeBasesFoldedConflicts(abItem(1, "both", "seed"), abItem(1, "both", "m2")),
			want: func(t *testing.T) map[string]outerResult {
				return outcomes(conflicts(), conflicts(), conflicts(), conflicts())
			},
		},
	}
	for _, tt := range tests {
		for _, mode := range mergeModes {
			for _, d := range []struct{ into, from string }{{"m1", "m2"}, {"m2", "m1"}} {
				t.Run(tt.name+"/"+mode+"/merge "+d.from+" into "+d.into, func(t *testing.T) {
					useTestCommitClock(t)
					b := newTestBackend(t)
					tt.build(t, b, mode)
					requireOuterResult(t, b, d.into, d.from, 1, tt.want(t)[mode])
				})
			}
		}
	}
}

// E1's clean merges keep every other document's one-sided changes.
func TestVirtualMergeBase_ThreeMergeBasesKeepOneSidedChanges(t *testing.T) {
	for _, mode := range []string{string(MergeModeFieldDivergent), string(MergeModeDocumentDivergent)} {
		for _, d := range []struct{ into, from string }{{"m1", "m2"}, {"m2", "m1"}} {
			t.Run(mode+"/merge "+d.from+" into "+d.into, func(t *testing.T) {
				useTestCommitClock(t)
				b := newTestBackend(t)
				threeBasesOneConflict(t, b, mode)
				mergeBranches(t, b, "testdb", d.into, d.from)
				requireItems(t, b, d.into, map[int32]*types.Document{
					1:  abItem(1, "resolved", "seed")(t),
					2:  abItem(2, "p1", "m1")(t),
					3:  abItem(3, "p2", "m2")(t),
					4:  abItem(4, "seed", "p3")(t),
					10: uDoc(t, 10, 10),
					20: uDoc(t, 20, 20),
				})
			})
		}
	}
}
