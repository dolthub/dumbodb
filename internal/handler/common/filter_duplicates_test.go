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

func TestNormalizeDuplicateFilterKeys(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }

	for _, tc := range []struct {
		name string
		cmd  *types.Document
		path []string
		want *types.Document
	}{{
		name: "find top level",
		cmd:  doc("find", "c", "filter", doc("y", int32(1), "y", int32(2))),
		path: []string{"filter"},
		want: doc("$and", arr(doc("y", int32(1)), doc("y", int32(2)))),
	}, {
		name: "repeated operator",
		cmd:  doc("count", "c", "query", doc("x", int32(0), "y", doc("$gt", int32(0), "$gt", int32(1)))),
		path: []string{"query"},
		want: doc("$and", arr(doc("x", int32(0)), doc("y", doc("$gt", int32(0))), doc("y", doc("$gt", int32(1))))),
	}, {
		name: "or clause",
		cmd:  doc("find", "c", "filter", doc("$or", arr(doc("y", int32(1), "y", int32(2)), doc("z", int32(3))))),
		path: []string{"filter"},
		want: doc("$or", arr(doc("$and", arr(doc("y", int32(1)), doc("y", int32(2)))), doc("z", int32(3)))),
	}, {
		name: "elemMatch query",
		cmd:  doc("find", "c", "filter", doc("a", doc("$elemMatch", doc("b", int32(1), "b", int32(2))))),
		path: []string{"filter"},
		want: doc("a", doc("$elemMatch", doc("$and", arr(doc("b", int32(1)), doc("b", int32(2)))))),
	}, {
		name: "regex stays",
		cmd:  doc("find", "c", "filter", doc("y", doc("$regex", "a", "$options", "i", "$options", "m"))),
		path: []string{"filter"},
		want: doc("y", doc("$regex", "a", "$options", "i", "$options", "m")),
	}, {
		name: "literal stays",
		cmd:  doc("find", "c", "filter", doc("y", doc("q", int32(1), "q", int32(2)))),
		path: []string{"filter"},
		want: doc("y", doc("q", int32(1), "q", int32(2))),
	}, {
		name: "unrelated command",
		cmd:  doc("insert", "c", "filter", doc("y", int32(1), "y", int32(2))),
		path: []string{"filter"},
		want: doc("y", int32(1), "y", int32(2)),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			NormalizeDuplicateFilterKeys(tc.cmd)
			got := must.NotFail(tc.cmd.Get(tc.path[0]))
			require.Equal(t, types.FormatAnyValue(tc.want), types.FormatAnyValue(got))
		})
	}

	t.Run("statements and pipelines", func(t *testing.T) {
		dup := func() *types.Document { return doc("y", int32(1), "y", int32(2)) }
		want := types.FormatAnyValue(doc("$and", arr(doc("y", int32(1)), doc("y", int32(2)))))

		update := doc("update", "c", "updates", arr(doc("q", dup(), "u", doc("$set", doc("y", int32(1), "y", int32(2))))))
		NormalizeDuplicateFilterKeys(update)
		statement := must.NotFail(must.NotFail(update.Get("updates")).(*types.Array).Get(0)).(*types.Document)
		require.Equal(t, want, types.FormatAnyValue(must.NotFail(statement.Get("q"))))
		_, modifierRepeats := must.NotFail(must.NotFail(statement.Get("u")).(*types.Document).Get("$set")).(*types.Document).FindDuplicateKey()
		require.True(t, modifierRepeats)

		agg := doc("explain", doc("aggregate", "c", "pipeline", arr(
			doc("$facet", doc("f", arr(doc("$match", dup())))),
		)))
		NormalizeDuplicateFilterKeys(agg)
		facet := must.NotFail(must.NotFail(must.NotFail(must.NotFail(agg.Get("explain")).(*types.Document).Get("pipeline")).(*types.Array).Get(0)).(*types.Document).Get("$facet")).(*types.Document)
		stage := must.NotFail(must.NotFail(facet.Get("f")).(*types.Array).Get(0)).(*types.Document)
		require.Equal(t, want, types.FormatAnyValue(must.NotFail(stage.Get("$match"))))
	})
}

// Values Go cannot compare with ==, such as Binary, must not panic.
func TestNormalizeDuplicateFilterKeysUncomparableValues(t *testing.T) {
	bin := types.Binary{Subtype: types.BinaryGeneric, B: []byte{1, 2, 3, 4}}
	filter := must.NotFail(types.NewDocument("_id", bin, "a", must.NotFail(types.NewDocument("$in", must.NotFail(types.NewArray(bin))))))
	cmd := must.NotFail(types.NewDocument("find", "c", "filter", filter))
	require.NotPanics(t, func() { NormalizeDuplicateFilterKeys(cmd) })
	require.Same(t, filter, must.NotFail(cmd.Get("filter")))
}
