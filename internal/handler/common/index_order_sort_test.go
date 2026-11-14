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

package common

import (
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// TestSortInIndexOrderTypeArrayBesideRange: a two-sided range beside a $type
// naming arrays still bounds the walk, and sorting by it must not fail.
func TestSortInIndexOrderTypeArrayBesideRange(t *testing.T) {
	for _, typ := range []any{"array", must.NotFail(types.NewArray("array", "number"))} {
		docs := []*types.Document{
			must.NotFail(types.NewDocument("_id", int32(1), "f", must.NotFail(types.NewArray(int32(2), int32(3))))),
			must.NotFail(types.NewDocument("_id", int32(2), "f", must.NotFail(types.NewArray(int32(0), int32(4))))),
		}
		filter := must.NotFail(types.NewDocument("f", must.NotFail(types.NewDocument("$gt", int32(1), "$lt", int32(5), "$type", typ))))
		pattern := must.NotFail(types.NewDocument("f", int32(1)))
		if err := sortDocumentsInIndexOrder(docs, pattern, filter, nil); err != nil {
			t.Fatalf("$type %v: %v", typ, err)
		}
		if id := must.NotFail(docs[0].Get("_id")); id != int32(1) {
			t.Errorf("$type %v: first _id = %v, want 1 (first key in the range)", typ, id)
		}
	}
}

func TestWithSingleBranchesInlined(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(vals ...any) *types.Array { return must.NotFail(types.NewArray(vals...)) }
	rangeCond := doc("$gte", int32(-1), "$lt", types.MaxKey)

	for name, tc := range map[string]struct{ in, want *types.Document }{
		"or":            {doc("$or", arr(doc("a", rangeCond))), doc("a", rangeCond)},
		"and":           {doc("$and", arr(doc("a", rangeCond))), doc("a", rangeCond)},
		"orOfAnd":       {doc("$or", arr(doc("$and", arr(doc("a", rangeCond, "b", int32(1)))))), doc("a", rangeCond, "b", int32(1))},
		"insideTwo":     {doc("$or", arr(doc("$or", arr(doc("a", int32(1)))), doc("b", int32(2)))), doc("$or", arr(doc("a", int32(1)), doc("b", int32(2))))},
		"fieldClash":    {doc("a", int32(1), "$or", arr(doc("a", int32(2)))), doc("a", int32(1), "$or", arr(doc("a", int32(2))))},
		"branchesClash": {doc("$and", arr(doc("a", int32(1))), "$or", arr(doc("a", int32(2)))), doc("a", int32(1), "$or", arr(doc("a", int32(2))))},
		"twoBranches":   {doc("$or", arr(doc("a", int32(1)), doc("a", int32(2)))), doc("$or", arr(doc("a", int32(1)), doc("a", int32(2))))},
	} {
		if got := WithSingleBranchesInlined(tc.in); !types.Identical(got, tc.want) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}

	unchanged := doc("$or", arr(doc("a", int32(1)), doc("a", int32(2))))
	if WithSingleBranchesInlined(unchanged) != unchanged {
		t.Errorf("an unchanged filter must be returned as is")
	}
}
