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

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestPullRemovesMatchingElements(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }
	// Embedded documents are stored with sorted keys.
	stored := func() *types.Array {
		return arr(doc("a", int32(0)), doc("a", int32(3)), doc("a", int32(1), "b", int32(2)),
			doc("$id", int32(1), "$ref", "x"), int32(5))
	}
	unsortedDoc := doc("b", int32(2), "a", int32(1))
	dbRef := doc("$ref", "x", "$id", int32(1))

	for _, tc := range []struct {
		name      string
		condition any
		removed   []int
	}{
		{"field operator on a subfield", doc("a", doc("$gt", int32(1))), []int{1}},
		{"$not on a subfield", doc("a", doc("$not", doc("$gt", int32(1)))), []int{0, 2, 3}},
		{"literal subfield matches a wider document", doc("a", int32(1)), []int{2}},
		{"$nor", doc("$nor", arr(doc("a", int32(1)))), []int{0, 1, 3}},
		{"empty matches every document", doc(), []int{0, 1, 2, 3}},
		{"element operator", doc("$gt", int32(4)), []int{4}},
		{"document in any key order", unsortedDoc, []int{2}},
		{"$eq document in any key order", doc("$eq", unsortedDoc), []int{2}},
		{"$in document in any key order", doc("$in", arr(unsortedDoc)), []int{2}},
		{"DBRef", dbRef, []int{3}},
		{"$eq DBRef", doc("$eq", dbRef), []int{3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := doc("_id", int32(1), "arr", stored())
			_, err := processPullArrayUpdateExpression("update", target, "arr", tc.condition)
			require.NoError(t, err)
			require.Equal(t, remaining(stored(), tc.removed), must.NotFail(target.Get("arr")))
		})
	}

	t.Run("$pullAll in any key order", func(t *testing.T) {
		target := doc("_id", int32(1), "arr", stored())
		_, err := processPullAllArrayUpdateExpression("update", target, "arr", arr(unsortedDoc, dbRef))
		require.NoError(t, err)
		require.Equal(t, remaining(stored(), []int{2, 3}), must.NotFail(target.Get("arr")))
	})
}

func remaining(array *types.Array, removed []int) *types.Array {
	for i := len(removed) - 1; i >= 0; i-- {
		array.Remove(removed[i])
	}
	return array
}
