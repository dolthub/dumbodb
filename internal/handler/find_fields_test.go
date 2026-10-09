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

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestFindRootFields(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }

	for name, tc := range map[string]struct {
		params common.FindParams
		want   []string
		ok     bool
	}{
		"inclusion": {common.FindParams{Projection: doc("i", int32(1), "tag", true)}, []string{"_id", "i", "tag"}, true},
		"filter and sort": {common.FindParams{
			Filter:     doc("grp", int32(3)),
			Sort:       doc("s.x", int32(-1)),
			Projection: doc("i", int32(1), "_id", int32(0)),
		}, []string{"grp", "_id", "s", "i"}, true},
		"dotted and positional": {common.FindParams{Projection: doc("a.b", int32(1), "c.$", int32(1))}, []string{"_id", "a", "c"}, true},
		"slice and elemMatch":   {common.FindParams{Projection: doc("a", doc("$slice", int32(2)), "b", doc("$elemMatch", doc("x", int32(1))))}, []string{"_id", "a", "b"}, true},
		"expressions":           {common.FindParams{Projection: doc("x", "$a", "y", doc("$add", arr("$b", int32(1))), "z", "literal")}, []string{"_id", "a", "b"}, true},
		"nested inclusion":      {common.FindParams{Projection: doc("a", doc("b", int32(1), "c", "$d"))}, []string{"_id", "a", "b", "d"}, true},
		"natural sort":          {common.FindParams{Sort: doc("$natural", int32(1)), Projection: doc("i", int32(1))}, []string{"_id", "i"}, true},
		"no projection":         {common.FindParams{}, nil, false},
		"only _id excluded":     {common.FindParams{Projection: doc("_id", int32(0))}, nil, false},
		"exclusion":             {common.FindParams{Projection: doc("payload", int32(0))}, nil, false},
		"nested exclusion":      {common.FindParams{Projection: doc("a", doc("b", int32(0)))}, nil, false},
		"meta":                  {common.FindParams{Projection: doc("s", doc("$meta", "textScore"))}, nil, false},
		"root variable":         {common.FindParams{Projection: doc("all", "$$ROOT")}, nil, false},
		"expr filter":           {common.FindParams{Filter: doc("$expr", true), Projection: doc("i", int32(1))}, nil, false},
		"meta sort":             {common.FindParams{Sort: doc("s", doc("$meta", "textScore")), Projection: doc("i", int32(1))}, nil, false},
		"returnKey":             {common.FindParams{ReturnKey: true, Projection: doc("i", int32(1))}, nil, false},
		"min":                   {common.FindParams{Min: doc("i", int32(1)), Projection: doc("i", int32(1))}, nil, false},
		"decimal flag":          {common.FindParams{Projection: doc("i", types.Decimal128{L: 1})}, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := findRootFields(&tc.params)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
}
