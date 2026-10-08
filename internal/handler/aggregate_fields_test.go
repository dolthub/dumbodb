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

package handler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestPipelineRootFields(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }
	group := func(spec ...any) *types.Document { return doc("$group", doc(spec...)) }

	for name, tc := range map[string]struct {
		pipeline []any
		want     []string
		ok       bool
	}{
		"match group": {[]any{
			doc("$match", doc("grp", doc("$gte", int32(0)))),
			group("_id", "$grp", "n", doc("$sum", int32(1))),
		}, []string{"grp"}, true},
		"group expressions": {[]any{
			group("_id", doc("a", "$a.x", "b", arr("$b", "lit")), "s", doc("$sum", doc("$multiply", arr("$c", "$$v")))),
		}, []string{"a", "b", "c"}, true},
		"sort limit count": {[]any{
			doc("$sort", doc("d", int32(1))), doc("$limit", int64(5)), doc("$count", "n"),
		}, []string{"d"}, true},
		"top sortBy":      {[]any{group("_id", types.Null, "t", doc("$top", doc("output", "$o", "sortBy", doc("s", int32(1)))))}, []string{"o", "s"}, true},
		"literal":         {[]any{group("_id", doc("$literal", "$notAField"))}, nil, true},
		"root variable":   {[]any{group("_id", types.Null, "all", doc("$push", "$$ROOT"))}, nil, false},
		"current path":    {[]any{group("_id", "$$CURRENT.a")}, nil, false},
		"getField":        {[]any{group("_id", doc("$getField", "a"))}, nil, false},
		"function":        {[]any{group("_id", doc("$function", doc()))}, nil, false},
		"expr in match":   {[]any{doc("$match", doc("$expr", true)), group("_id", "$a")}, nil, false},
		"meta sort":       {[]any{doc("$sort", doc("s", doc("$meta", "textScore"))), group("_id", "$a")}, nil, false},
		"no terminal":     {[]any{doc("$match", doc("a", int32(1)))}, nil, false},
		"project":         {[]any{doc("$project", doc("a", int32(1))), group("_id", "$a")}, nil, false},
		"group then more": {[]any{group("_id", "$a"), doc("$match", doc("_id", int32(1)))}, []string{"a"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := pipelineRootFields(tc.pipeline)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
}
