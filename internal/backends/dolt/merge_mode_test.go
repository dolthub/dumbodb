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

// doc builds a document from alternating key/value pairs. A nil return means
// the document is absent on that side of the merge.
func doc(t *testing.T, pairs ...any) *types.Document {
	t.Helper()
	if pairs == nil {
		return nil
	}
	d, err := types.NewDocument(pairs...)
	require.NoError(t, err)
	return d
}

// The scenario matrix of docs/design/merge-strictness.md, over documents.
// Column order is documentTouched, fieldTouched, fieldDivergent,
// documentDivergent.
func TestMergeModeScenarioMatrix(t *testing.T) {
	modes := []MergeMode{
		MergeModeDocumentTouched,
		MergeModeFieldTouched,
		MergeModeFieldDivergent,
		MergeModeDocumentDivergent,
	}

	cases := []struct {
		name              string
		base, left, right *types.Document
		conflicts         [4]bool
	}{
		{
			name:      "only one side changed the document",
			base:      doc(t, "a", int32(0), "b", int32(0)),
			left:      doc(t, "a", int32(1), "b", int32(0)),
			right:     doc(t, "a", int32(0), "b", int32(0)),
			conflicts: [4]bool{false, false, false, false},
		},
		{
			name:      "different fields, same document",
			base:      doc(t, "a", int32(0), "b", int32(0)),
			left:      doc(t, "a", int32(1), "b", int32(0)),
			right:     doc(t, "a", int32(0), "b", int32(2)),
			conflicts: [4]bool{true, false, false, true},
		},
		{
			// The compare-and-swap race: both sides move the same field to the
			// same value, so the documents are identical and the differ would
			// call this convergent.
			name:      "same field, same value",
			base:      doc(t, "v", int32(1)),
			left:      doc(t, "v", int32(2)),
			right:     doc(t, "v", int32(2)),
			conflicts: [4]bool{true, true, false, false},
		},
		{
			name:      "same field same value, plus a disjoint edit",
			base:      doc(t, "v", int32(1), "b", int32(0)),
			left:      doc(t, "v", int32(2), "b", int32(5)),
			right:     doc(t, "v", int32(2), "b", int32(0)),
			conflicts: [4]bool{true, true, false, true},
		},
		{
			name:      "same field, different values",
			base:      doc(t, "v", int32(1)),
			left:      doc(t, "v", int32(2)),
			right:     doc(t, "v", int32(3)),
			conflicts: [4]bool{true, true, true, true},
		},
		{
			name:      "modify vs delete",
			base:      doc(t, "a", int32(0)),
			left:      doc(t, "a", int32(1)),
			right:     nil,
			conflicts: [4]bool{true, true, true, true},
		},
		{
			name:      "add/add identical content",
			base:      nil,
			left:      doc(t, "a", int32(1)),
			right:     doc(t, "a", int32(1)),
			conflicts: [4]bool{true, true, false, false},
		},
		{
			name:      "add/add different content",
			base:      nil,
			left:      doc(t, "a", int32(1)),
			right:     doc(t, "a", int32(9)),
			conflicts: [4]bool{true, true, true, true},
		},
		{
			// Owner ruling 2026-08-28: the Touched modes include deletion.
			name:      "both sides deleted",
			base:      doc(t, "a", int32(0)),
			left:      nil,
			right:     nil,
			conflicts: [4]bool{true, true, false, false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i, mode := range modes {
				got := mode.conflicts(tc.base, tc.left, tc.right)
				require.Equal(t, tc.conflicts[i], got,
					"%s on %q: got conflict=%t", mode, tc.name, got)
			}
		})
	}
}

// fieldTouched and documentDivergent are incomparable: neither implies the
// other, so no ordering of the four can be read as strict-to-loose.
func TestMergeModeIncomparablePair(t *testing.T) {
	// Same field, same value: fieldTouched conflicts, documentDivergent merges.
	base := doc(t, "a", int32(0), "b", int32(0))
	left := doc(t, "a", int32(0), "b", int32(1))
	right := doc(t, "a", int32(0), "b", int32(1))
	require.True(t, MergeModeFieldTouched.conflicts(base, left, right))
	require.False(t, MergeModeDocumentDivergent.conflicts(base, left, right))

	// Disjoint fields: documentDivergent conflicts, fieldTouched merges.
	left = doc(t, "a", int32(0), "b", int32(1))
	right = doc(t, "a", int32(1), "b", int32(0))
	require.False(t, MergeModeFieldTouched.conflicts(base, left, right))
	require.True(t, MergeModeDocumentDivergent.conflicts(base, left, right))
}

// The default has to be the mode under which the compare-and-swap works.
func TestDefaultMergeModeRefusesTheCASRace(t *testing.T) {
	require.Equal(t, MergeModeFieldTouched, DefaultMergeMode)
	base := doc(t, "_id", "counter", "version", int64(0))
	left := doc(t, "_id", "counter", "version", int64(1))
	right := doc(t, "_id", "counter", "version", int64(1))
	require.True(t, DefaultMergeMode.conflicts(base, left, right),
		"two acknowledged CAS increments from the same version must not both land")
	require.False(t, MergeModeFieldDivergent.conflicts(base, left, right),
		"precondition: today's rule accepts them, which is the defect")
}

func TestMergeModeUnknownFallsBackToDefault(t *testing.T) {
	require.False(t, MergeMode("nonsense").valid())
	require.True(t, DefaultMergeMode.valid())
}
