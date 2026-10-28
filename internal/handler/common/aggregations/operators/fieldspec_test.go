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

package operators

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestFieldSpecTrees(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }
	source := func() *types.Document {
		return doc("_id", int32(2), "f", int32(5), "v", arr(doc("a", int32(2), "z", int32(8)), int32(3)))
	}

	got, err := ProjectInclusionTree(source(), doc("v.a", true, "v.c", "$f"))
	require.NoError(t, err)
	require.Equal(t, types.Equal, types.Compare(doc("_id", int32(2), "v", arr(doc("a", int32(2), "c", int32(5)), doc("c", int32(5)))), got), "%v", got)

	got, err = ProjectInclusionTree(source(), doc("_id", false, "v.a", int32(1)))
	require.NoError(t, err)
	require.Equal(t, types.Equal, types.Compare(doc("v", arr(doc("a", int32(2)))), got), "%v", got)

	got, err = ProjectInclusionTree(source(), doc("w.x", "$_id", "w.y", doc("$literal", "l"), "w.n", "$nope"))
	require.NoError(t, err)
	require.Equal(t, types.Equal, types.Compare(doc("_id", int32(2), "w", doc("x", int32(2), "y", "l")), got), "%v", got)

	got, err = AddFieldsTree(source(), doc("v.b", "$f", "v.q", "$nope"))
	require.NoError(t, err)
	require.Equal(t, types.Equal, types.Compare(doc("_id", int32(2), "f", int32(5), "v", arr(doc("a", int32(2), "z", int32(8), "b", int32(5)), doc("b", int32(5)))), got), "%v", got)

	got, err = AddFieldsTree(doc("_id", int32(1), "v", int32(7)), doc("v.n.m", "$_id", "v.one", int32(1)))
	require.NoError(t, err)
	require.Equal(t, types.Equal, types.Compare(doc("_id", int32(1), "v", doc("n", doc("m", int32(1)), "one", int32(1))), got), "%v", got)

	require.True(t, SpecHasComputedPath(doc("v", doc("a", "$f")), true))
	require.True(t, SpecHasComputedPath(doc("v.a", "$f"), true))
	require.False(t, SpecHasComputedPath(doc("v.a", int32(1), "f", "$f"), true))
	require.True(t, SpecHasComputedPath(doc("v.a", int32(1)), false))
}
