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

	"github.com/dolthub/dumbodb/internal/types"
)

// absentField marks a field as missing on one side of the three-way merge.
const absentField = "\x00absent"

// mergeCase is one cell of the field-level merge matrix.
type mergeCase struct {
	name         string
	base         string
	left         string
	right        string
	wantConflict bool
	wantValue    string
}

// Every reachable cell of the field-level three-way merge. A key reaches
// mergeBSONDoc only by appearing in base, left, or right, so absent-on-all-
// three is unreachable and the 14 cells below are exhaustive.
//
// Modify on one side against delete on the other must conflict: the two sides
// disagree about whether the value should exist, and either answer discards an
// intention. Those two cells fail today (issue 59).
var mergeMatrix = []mergeCase{
	// base absent: neither side can delete.
	{name: "ours inserts only", base: absentField, left: "review", right: absentField,
		wantValue: "review"},
	{name: "theirs inserts only", base: absentField, left: absentField, right: "review",
		wantValue: "review"},
	{name: "both insert same value", base: absentField, left: "review", right: "review",
		wantValue: "review"},
	{name: "both insert different values", base: absentField, left: "review", right: "final",
		wantConflict: true, wantValue: absentField},

	// base present, field survives on both sides.
	{name: "neither side touches", base: "draft", left: "draft", right: "draft",
		wantValue: "draft"},
	{name: "ours modifies, theirs untouched", base: "draft", left: "review", right: "draft",
		wantValue: "review"},
	{name: "theirs modifies, ours untouched", base: "draft", left: "draft", right: "review",
		wantValue: "review"},
	{name: "both modify same value", base: "draft", left: "review", right: "review",
		wantValue: "review"},
	{name: "both modify different values", base: "draft", left: "review", right: "final",
		wantConflict: true, wantValue: absentField},

	// base present, field deleted on one or both sides.
	{name: "ours deletes, theirs untouched", base: "draft", left: absentField, right: "draft",
		wantValue: absentField},
	{name: "theirs deletes, ours untouched", base: "draft", left: "draft", right: absentField,
		wantValue: absentField},
	{name: "both delete", base: "draft", left: absentField, right: absentField,
		wantValue: absentField},
	{name: "ours modifies, theirs deletes", base: "draft", left: "review", right: absentField,
		wantConflict: true, wantValue: absentField},
	{name: "theirs modifies, ours deletes", base: "draft", left: absentField, right: "review",
		wantConflict: true, wantValue: absentField},
}

func TestMergeBSONDocFieldMatrix(t *testing.T) {
	for _, tc := range mergeMatrix {
		t.Run(tc.name, func(t *testing.T) {
			merged, conflict := mergeBSONDoc(
				docWithStatus(tc.base), docWithStatus(tc.left), docWithStatus(tc.right))

			if conflict != tc.wantConflict {
				t.Fatalf("conflict = %v, want %v", conflict, tc.wantConflict)
			}
			if conflict {
				if merged != nil {
					t.Errorf("conflict must return a nil document, got keys %v", merged.Keys())
				}
				return
			}
			if got, ok := merged.Get("_id"); ok != nil || got != int32(1) {
				t.Errorf("_id = %v (err %v), want 1", got, ok)
			}
			if tc.wantValue == absentField {
				if merged.Has("status") {
					got, _ := merged.Get("status")
					t.Errorf("status = %v, want absent", got)
				}
				return
			}
			got, err := merged.Get("status")
			if err != nil {
				t.Fatalf("status missing, want %q", tc.wantValue)
			}
			if got != tc.wantValue {
				t.Errorf("status = %v, want %q", got, tc.wantValue)
			}
		})
	}
}

// The matrix must stay exhaustive over the reachable presence combinations.
func TestMergeBSONDocFieldMatrixCoversEveryCell(t *testing.T) {
	presence := func(v string) string {
		if v == absentField {
			return "absent"
		}
		return "present"
	}

	seen := make(map[string]bool)
	for _, tc := range mergeMatrix {
		seen[presence(tc.base)+"/"+presence(tc.left)+"/"+presence(tc.right)] = true
	}

	for _, combo := range []string{
		"absent/present/absent", "absent/absent/present", "absent/present/present",
		"present/present/present", "present/absent/present",
		"present/present/absent", "present/absent/absent",
	} {
		if !seen[combo] {
			t.Errorf("no matrix case covers base/left/right = %s", combo)
		}
	}
}

func docWithStatus(status string) *types.Document {
	d := types.MakeDocument(2)
	d.Set("_id", int32(1))
	if status != absentField {
		d.Set("status", status)
	}
	return d
}
