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

package projection

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestProjectTopLevelInclusionMatchesProjectDocument(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }

	docs := []*types.Document{
		doc("_id", int32(1), "a", "x", "b", int64(2), "c", doc("d", int32(3)), "e", arr(int32(1), doc("f", "g"))),
		doc("b", int64(2), "_id", "s", "a", types.Null),
		doc("c", arr(), "a", 1.5),
		doc(),
	}
	projections := []*types.Document{
		doc("a", int32(1)),
		doc("b", true, "a", int32(1)),
		doc("c", int32(1), "e", int32(1), "_id", int32(0)),
		doc("_id", false, "missing", int32(1)),
		doc("_id", int32(1), "a", int32(1)),
		doc("_id", "literal", "a", int32(1)),
		doc("_id", doc("$add", arr(int32(1), int32(2))), "b", int32(1)),
		doc("e", int32(1), "_id", true),
	}

	for _, projection := range projections {
		validated, inclusion, err := ValidateProjection(projection)
		require.NoError(t, err)
		fields, ok := topLevelInclusionFields(validated, inclusion)
		require.True(t, ok, "projection %v", projection)

		for _, d := range docs {
			want, err := ProjectDocument(d, validated, inclusion)
			require.NoError(t, err)
			got, err := projectTopLevelInclusion(d, validated, fields)
			require.NoError(t, err)
			require.Equal(t, want.Keys(), got.Keys(), "projection %v doc %v", projection, d)
			require.Equal(t, types.Equal, types.Compare(want, got), "projection %v doc %v", projection, d)
		}
	}
}

func TestTopLevelInclusionFieldsRejects(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }

	for _, projection := range []*types.Document{
		doc("a.b", int32(1)),
		doc("a", "$b"),
		doc("a", "literal"),
		doc("a", doc("$add", must.NotFail(types.NewArray("$b", int32(1))))),
		doc("a", int32(0)),
		doc("_id", int32(0)),
	} {
		validated, inclusion, err := ValidateProjection(projection)
		require.NoError(t, err)
		_, ok := topLevelInclusionFields(validated, inclusion)
		require.False(t, ok, "projection %v", projection)
	}
}
