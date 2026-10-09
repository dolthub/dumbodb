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
		"top sortBy":            {[]any{group("_id", types.Null, "t", doc("$top", doc("output", "$o", "sortBy", doc("s", int32(1)))))}, []string{"o", "s"}, true},
		"literal":               {[]any{group("_id", doc("$literal", "$notAField"))}, nil, true},
		"root variable":         {[]any{group("_id", types.Null, "all", doc("$push", "$$ROOT"))}, nil, false},
		"current path":          {[]any{group("_id", "$$CURRENT.a")}, nil, false},
		"getField":              {[]any{group("_id", doc("$getField", "a"))}, nil, false},
		"function":              {[]any{group("_id", doc("$function", doc()))}, nil, false},
		"expr in match":         {[]any{doc("$match", doc("$expr", true)), group("_id", "$a")}, nil, false},
		"meta sort":             {[]any{doc("$sort", doc("s", doc("$meta", "textScore"))), group("_id", "$a")}, nil, false},
		"no terminal":           {[]any{doc("$match", doc("a", int32(1)))}, nil, false},
		"project then group":    {[]any{doc("$project", doc("a", int32(1))), group("_id", "$a")}, []string{"_id", "a"}, true},
		"match project":         {[]any{doc("$match", doc("g", int32(1))), doc("$project", doc("a", int32(1), "b", true))}, []string{"g", "_id", "a", "b"}, true},
		"project no _id":        {[]any{doc("$project", doc("_id", int32(0), "a", int32(1)))}, []string{"_id", "a"}, true},
		"project expressions":   {[]any{doc("$project", doc("x", "$a.b", "y", doc("$slice", arr("$arr", int32(1))), "z", "lit"))}, []string{"_id", "a", "arr"}, true},
		"project nested":        {[]any{doc("$project", doc("a", doc("b", int32(1), "c", "$d")))}, []string{"_id", "a", "b", "d"}, true},
		"project literal types": {[]any{doc("$project", doc("a", int32(1), "n", types.Null))}, []string{"_id", "a"}, true},
		"project only _id off":  {[]any{doc("$project", doc("_id", int32(0)))}, nil, false},
		"project exclusion":     {[]any{doc("$project", doc("payload", int32(0)))}, nil, false},
		"project nested excl":   {[]any{doc("$project", doc("a", doc("b", int32(0))))}, nil, false},
		"project root":          {[]any{doc("$project", doc("all", "$$ROOT"))}, nil, false},
		"project meta":          {[]any{doc("$project", doc("s", doc("$meta", "textScore")))}, nil, false},
		"project decimal":       {[]any{doc("$project", doc("a", types.Decimal128{L: 1}))}, nil, false},
		"addFields":             {[]any{doc("$addFields", doc("a", int32(1)))}, nil, false},
		"group then more":       {[]any{group("_id", "$a"), doc("$match", doc("_id", int32(1)))}, []string{"a"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := pipelineRootFields(tc.pipeline)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
}
