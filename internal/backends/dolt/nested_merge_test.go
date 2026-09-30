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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
)

func TestNestedDocumentMergeMatrix(t *testing.T) {
	d := func(pairs ...any) *types.Document { return doc(t, pairs...) }
	wrap := func(value any) *types.Document { return d("_id", int32(1), "meta", value) }
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
	modes := []MergeMode{MergeModeDocumentTouched, MergeModeFieldTouched, MergeModeFieldDivergent, MergeModeDocumentDivergent}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, left, right := wrap(tc.base), wrap(tc.left), wrap(tc.right)
			for i, mode := range modes {
				t.Run(string(mode), func(t *testing.T) { require.Equal(t, tc.conflicts[i], mode.conflicts(base, left, right)) })
			}
			originals := make([][]byte, 3)
			for i, input := range []*types.Document{base, left, right} {
				encoded, err := docToBSON(input)
				require.NoError(t, err)
				originals[i] = encoded
			}
			merged, conflict := mergeBSONDoc(base, left, right)
			for i, input := range []*types.Document{base, left, right} {
				encoded, err := docToBSON(input)
				require.NoError(t, err)
				require.Equal(t, originals[i], encoded, "merge mutated an input")
			}
			require.Equal(t, tc.mergeConflict, conflict)
			if conflict {
				require.Nil(t, merged)
				return
			}
			encoded, err := docToBSON(merged)
			require.NoError(t, err)
			expected, err := docToBSON(wrap(tc.want))
			require.NoError(t, err)
			require.Equal(t, expected, encoded)
			reversed, reverseConflict := mergeBSONDoc(base, right, left)
			require.False(t, reverseConflict)
			reversedBytes, err := docToBSON(reversed)
			require.NoError(t, err)
			require.Equal(t, expected, reversedBytes)
		})
	}
}

func TestNestedDocumentLifecycleConflicts(t *testing.T) {
	d := func(pairs ...any) *types.Document { return doc(t, pairs...) }
	modes := []MergeMode{MergeModeDocumentTouched, MergeModeFieldTouched, MergeModeFieldDivergent, MergeModeDocumentDivergent}
	cases := []struct {
		name              string
		base, left, right *types.Document
	}{
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
