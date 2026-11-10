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

package aggregations

import (
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestMoveMatchesAhead(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(vs ...any) *types.Array { return must.NotFail(types.NewArray(vs...)) }
	match := func(f *types.Document) *types.Document { return doc("$match", f) }
	for _, tc := range []struct {
		name  string
		in    []any
		first *types.Document // the expected first stage; nil when nothing moves
	}{
		{"rename", []any{doc("$project", doc("z", "$a")), match(doc("z", int32(3)))}, match(doc("a", int32(3)))},
		{"rename_dotted", []any{doc("$project", doc("z", "$a.b")), match(doc("z.c", int32(3)))}, match(doc("a.b.c", int32(3)))},
		{"included", []any{doc("$project", doc("a", int32(1))), match(doc("a", int32(3)))}, match(doc("a", int32(3)))},
		{"not_included", []any{doc("$project", doc("a", int32(1))), match(doc("b", int32(3)))}, nil},
		{"excluded", []any{doc("$project", doc("b", int32(0))), match(doc("b", int32(3)))}, nil},
		{"not_excluded", []any{doc("$project", doc("b", int32(0))), match(doc("a", int32(3)))}, match(doc("a", int32(3)))},
		{"computed", []any{doc("$project", doc("z", doc("$add", arr("$a", int32(1))))), match(doc("z", int32(3)))}, nil},
		{"set_untouched", []any{doc("$set", doc("c", int32(1))), match(doc("a", int32(3)))}, match(doc("a", int32(3)))},
		{"set_overwritten", []any{doc("$set", doc("a", doc("$multiply", arr("$a", int32(10))))), match(doc("a", int32(30)))}, nil},
		{"set_rename", []any{doc("$set", doc("z", "$a")), match(doc("z", int32(3)))}, match(doc("a", int32(3)))},
		{"or", []any{doc("$project", doc("z", "$a", "b", int32(1))), match(doc("$or", arr(doc("z", int32(1)), doc("b", int32(2)))))},
			match(doc("$or", arr(doc("a", int32(1)), doc("b", int32(2)))))},
		{"expr", []any{doc("$project", doc("z", "$a")), match(doc("$expr", doc("$eq", arr("$z", int32(1)))))}, nil},
		{"variable", []any{doc("$project", doc("z", "$$ROOT")), match(doc("z", int32(3)))}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := MoveMatchesAhead(tc.in)
			first := out[0].(*types.Document)
			if tc.first == nil {
				if first != tc.in[0] {
					t.Fatalf("moved: %v", types.FormatAnyValue(first))
				}
				return
			}
			if types.FormatAnyValue(first) != types.FormatAnyValue(tc.first) {
				t.Fatalf("first stage = %s, want %s", types.FormatAnyValue(first), types.FormatAnyValue(tc.first))
			}
			if out[1] != tc.in[0] {
				t.Fatal("the shaping stage did not follow the moved $match")
			}
		})
	}
}
