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

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
)

func TestNestedDocumentMergeMatrix(t *testing.T) {
	d := func(pairs ...any) *types.Document { return doc(t, pairs...) }
	wrap := func(value any, depth int) *types.Document {
		for level := 1; level < depth; level++ {
			value = d("inner", value)
		}
		return d("_id", int32(1), "meta", value)
	}
	array := func(values ...any) *types.Array {
		result, err := types.NewArray(values...)
		require.NoError(t, err)
		return result
	}
	cases := []struct {
		name                    string
		base, left, right, want *types.Document
		conflicts               [4]bool
		mergeConflict           bool
	}{
		{"disjoint edits", d("a", "old", "b", "old"), d("a", "new", "b", "old"), d("a", "old", "b", "new"), d("a", "new", "b", "new"), [4]bool{true, false, false, true}, false},
		{"empty subdocument disjoint additions", types.MakeDocument(0), d("a", "new"), d("b", "new"), d("a", "new", "b", "new"), [4]bool{true, false, false, true}, false},
		{"disjoint additions", d("seed", "old"), d("seed", "old", "a", "new"), d("seed", "old", "b", "new"), d("seed", "old", "a", "new", "b", "new"), [4]bool{true, false, false, true}, false},
		{"disjoint deletions", d("a", "old", "b", "old"), d("a", "old"), d("b", "old"), types.MakeDocument(0), [4]bool{true, false, false, true}, false},
		{"same edit", d("a", "old"), d("a", "new"), d("a", "new"), d("a", "new"), [4]bool{true, true, false, false}, false},
		{"same edit plus disjoint", d("a", "old", "b", "old"), d("a", "new", "b", "new"), d("a", "new", "b", "old"), d("a", "new", "b", "new"), [4]bool{true, true, false, true}, false},
		{"same addition", d("seed", "old"), d("seed", "old", "a", "new"), d("seed", "old", "a", "new"), d("seed", "old", "a", "new"), [4]bool{true, true, false, false}, false},
		{"same deletion", d("a", "old", "b", "old"), d("b", "old"), d("b", "old"), d("b", "old"), [4]bool{true, true, false, false}, false},
		{"different edits", d("a", "old"), d("a", "left"), d("a", "right"), nil, [4]bool{true, true, true, true}, true},
		{"delete versus modify", d("a", "old", "b", "old"), d("b", "old"), d("a", "new", "b", "old"), nil, [4]bool{true, true, true, true}, true},
		{"nested id disjoint", d("_id", "old", "b", "old"), d("_id", "new", "b", "old"), d("_id", "old", "b", "new"), d("_id", "new", "b", "new"), [4]bool{true, false, false, true}, false},
		{"nested id same edit", d("_id", "old"), d("_id", "new"), d("_id", "new"), d("_id", "new"), [4]bool{true, true, false, false}, false},
		{"three levels", d("inner", d("deep", d("a", "old", "b", "old"))), d("inner", d("deep", d("a", "new", "b", "old"))), d("inner", d("deep", d("a", "old", "b", "new"))), d("inner", d("deep", d("a", "new", "b", "new"))), [4]bool{true, false, false, true}, false},
		{"arrays atomic", d("list", array("old", "old")), d("list", array("new", "old")), d("list", array("old", "new")), nil, [4]bool{true, true, true, true}, true},
		{"documents inside arrays atomic", d("list", array(d("a", "old", "b", "old"))), d("list", array(d("a", "new", "b", "old"))), d("list", array(d("a", "old", "b", "new"))), nil, [4]bool{true, true, true, true}, true},
	}

	for _, depth := range []int{1, 2, 4} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("depth_%d/%s", depth, tc.name), func(t *testing.T) {
				var want *types.Document
				if !tc.mergeConflict {
					want = wrap(tc.want, depth)
				}
				assertNestedMergeCase(t, wrap(tc.base, depth), wrap(tc.left, depth), wrap(tc.right, depth), want, tc.conflicts, tc.mergeConflict)
			})
		}
	}
}

func assertNestedMergeCase(t *testing.T, base, left, right, want *types.Document, conflicts [4]bool, mergeConflict bool) {
	t.Helper()
	modes := []MergeMode{MergeModeDocumentTouched, MergeModeFieldTouched, MergeModeFieldDivergent, MergeModeDocumentDivergent}
	for i, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			require.Equal(t, conflicts[i], mode.conflicts(base, left, right))
			require.Equal(t, conflicts[i], mode.conflicts(base, right, left), "reversed branches")
		})
	}
	encode := func(document *types.Document) []byte {
		if document == nil {
			return nil
		}
		encoded, err := docToBSON(document)
		require.NoError(t, err)
		return encoded
	}
	originals := [][]byte{encode(base), encode(left), encode(right)}
	for _, branches := range [][2]*types.Document{{left, right}, {right, left}} {
		merged, conflict := mergeBSONDoc(base, branches[0], branches[1])
		require.Equal(t, mergeConflict, conflict)
		if conflict {
			require.Nil(t, merged)
		} else {
			require.Equal(t, encode(want), encode(merged))
		}
		require.Equal(t, originals, [][]byte{encode(base), encode(left), encode(right)}, "merge mutated an input")
	}
}

func TestNestedDocumentLifecycleConflicts(t *testing.T) {
	d := func(pairs ...any) *types.Document { return doc(t, pairs...) }
	modes := []MergeMode{MergeModeDocumentTouched, MergeModeFieldTouched, MergeModeFieldDivergent, MergeModeDocumentDivergent}
	cases := []struct {
		name              string
		base, left, right *types.Document
	}{
		{"intermediate parent delete versus deep edit", d("meta", d("inner", d("deep", d("value", "old")))), d("meta", types.MakeDocument(0)), d("meta", d("inner", d("deep", d("value", "new"))))},
		{"intermediate scalar replacement versus deep edit", d("meta", d("inner", d("deep", d("value", "old")))), d("meta", d("inner", d("deep", "scalar"))), d("meta", d("inner", d("deep", d("value", "new"))))},
		{"parent delete versus new child", d("meta", d("a", "old")), types.MakeDocument(0), d("meta", d("a", "old", "new", "value"))},
		{"parent delete versus child delete", d("meta", d("a", "old")), types.MakeDocument(0), d("meta", types.MakeDocument(0))},
		{"document versus scalar", d("meta", d("a", "old")), d("meta", "scalar"), d("meta", d("a", "new"))},
		{"concurrent subdocument creation", types.MakeDocument(0), d("meta", d("a", "left")), d("meta", d("b", "right"))},
		{"scalar replaced by two documents", d("meta", "scalar"), d("meta", d("a", "left")), d("meta", d("b", "right"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range modes {
				require.True(t, mode.conflicts(tc.base, tc.left, tc.right), string(mode))
				require.True(t, mode.conflicts(tc.base, tc.right, tc.left), string(mode)+" reversed")
			}
			merged, conflict := mergeBSONDoc(tc.base, tc.left, tc.right)
			require.True(t, conflict)
			require.Nil(t, merged)
			merged, conflict = mergeBSONDoc(tc.base, tc.right, tc.left)
			require.True(t, conflict)
			require.Nil(t, merged)
		})
	}
}

func TestNestedDocumentMixedLevelsAndInserts(t *testing.T) {
	d := func(pairs ...any) *types.Document { return doc(t, pairs...) }
	mixed := func(shallow, deep string) *types.Document {
		return d("meta", d("x", shallow, "inner", d("deep", d("b", deep))))
	}
	top := func(title, leaf string) *types.Document {
		return d("title", title, "meta", d("inner", d("deep", d("value", leaf))))
	}
	siblings := func(a, b string) *types.Document { return d("m", d("a", d("x", a), "b", d("y", b))) }
	cases := []struct {
		name                    string
		base, left, right, want *types.Document
		conflicts               [4]bool
		mergeConflict           bool
	}{
		{"shallow versus deep", mixed("old", "old"), mixed("new", "old"), mixed("old", "new"), mixed("new", "new"), [4]bool{true, false, false, true}, false},
		{"top versus depth four", top("old", "old"), top("new", "old"), top("old", "new"), top("new", "new"), [4]bool{true, false, false, true}, false},
		{"sibling subdocuments", siblings("old", "old"), siblings("new", "old"), siblings("old", "new"), siblings("new", "new"), [4]bool{true, false, false, true}, false},
		{"nil base identical nested inserts", nil, d("_id", int32(1), "meta", d("inner", d("value", "same"))), d("_id", int32(1), "meta", d("inner", d("value", "same"))), d("_id", int32(1), "meta", d("inner", d("value", "same"))), [4]bool{true, true, false, false}, false},
		{"nil base disjoint nested inserts", nil, d("_id", int32(1), "meta", d("a", "left")), d("_id", int32(1), "meta", d("b", "right")), nil, [4]bool{true, true, true, true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertNestedMergeCase(t, tc.base, tc.left, tc.right, tc.want, tc.conflicts, tc.mergeConflict)
		})
	}
}
