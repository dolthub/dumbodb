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

package commonpath

import (
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func doc(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
func arr(vals ...any) *types.Array     { return must.NotFail(types.NewArray(vals...)) }

// Expectations were recorded against MongoDB 8.0 with {<path>: null} queries.
func TestHasMissingBranch(t *testing.T) {
	cases := []struct {
		name string
		doc  *types.Document
		path string
		want bool
	}{
		{"present", doc("a", doc("b", int32(1))), "a.b", false},
		{"explicit null", doc("a", doc("b", types.Null)), "a.b", false},
		{"missing leaf", doc("a", doc("c", int32(1))), "a.b", true},
		{"missing root", doc(), "a.b", true},
		{"scalar intermediate", doc("a", int32(5)), "a.b", true},
		{"empty array", doc("a", arr()), "a.b", false},
		{"array of scalars", doc("a", arr(int32(1), int32(2))), "a.b", false},
		{"array elem lacks field", doc("a", arr(doc("b", int32(5)), doc("c", int32(1)))), "a.b", true},
		{"nested array skipped", doc("a", arr(arr(doc("c", int32(1))))), "a.b", false},
		{"null elem skipped", doc("a", arr(types.Null, doc("b", int32(2)))), "a.b", false},
		{"leaf empty array", doc("a", doc("b", arr())), "a.b", false},
		{"scalar reached through array", doc("a", arr(int32(1), doc("b", int32(2)))), "a.b.c", true},
		{"position out of range", doc("a", arr()), "a.0", false},
		{"position in range", doc("a", arr(int32(1), int32(2))), "a.0", false},
		{"doc elem lacks numeric field", doc("a", arr(int32(1), doc("b", int32(2)))), "a.0", true},
		{"numeric field on doc", doc("a", doc("b", arr(int32(1), int32(3)))), "a.0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasMissingBranch(tc.doc, must.NotFail(types.NewPathFromString(tc.path))); got != tc.want {
				t.Fatalf("HasMissingBranch(%s) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
