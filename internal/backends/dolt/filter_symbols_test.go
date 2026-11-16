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

package dolt

import (
	"context"
	"errors"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestCheckFilterSymbols(t *testing.T) {
	en2 := must.NotFail(types.NewDocument("locale", "en", "strength", int32(2)))
	idxInfos := []backends.IndexInfo{
		{Name: "v_1", Key: []backends.IndexKeyPair{{Field: "v"}}, Collation: en2},
		{Name: "w_1", Key: []backends.IndexKeyPair{{Field: "w"}}},
		{Name: "a_1_c_1", Key: []backends.IndexKeyPair{{Field: "a"}, {Field: "c"}}, Collation: en2},
		{Name: "h_1", Key: []backends.IndexKeyPair{{Field: "h"}}, Collation: en2, Hidden: true},
		{Name: "s_1", Key: []backends.IndexKeyPair{{Field: "s"}}, Collation: en2, Sparse: true},
		{Name: "x_hashed", Key: []backends.IndexKeyPair{{Field: "x", Hashed: true}}, Collation: en2},
	}
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }
	sym := types.Symbol("ABC")

	for name, tc := range map[string]struct {
		filter, collation *types.Document
		refused           bool
	}{
		"equality":                      {doc("v", sym), nil, true},
		"equality under the collation":  {doc("v", sym), en2, true},
		"$ne":                           {doc("v", doc("$ne", sym)), nil, true},
		"$in with a string":             {doc("v", doc("$in", arr("x", sym))), nil, false},
		"$in with a string, collated":   {doc("v", doc("$in", arr("x", sym))), en2, true},
		"inside $or":                    {doc("$or", arr(doc("v", sym), doc("w", int32(1)))), nil, true},
		"field without a collation":     {doc("w", sym), nil, false},
		"field without an index":        {doc("y", sym), nil, false},
		"compound, trailing field only": {doc("c", sym), nil, false},
		"compound with leading field":   {doc("c", sym, "a", int32(1)), nil, true},
		"hidden index":                  {doc("h", sym), nil, false},
		"sparse equality":               {doc("s", sym), nil, true},
		"sparse $ne":                    {doc("s", doc("$ne", sym)), nil, false},
		"$nor":                          {doc("$nor", arr(doc("v", sym))), nil, true},
		"sparse $nor":                   {doc("$nor", arr(doc("s", sym))), nil, false},
		"hashed equality":               {doc("x", sym), nil, true},
		"hashed $gt":                    {doc("x", doc("$gt", sym)), nil, false},
		"$expr $eq":                     {doc("$expr", doc("$eq", arr("$v", sym))), nil, true},
		"$expr $eq reversed":            {doc("$expr", doc("$eq", arr(sym, "$v"))), nil, true},
		"$expr $gt on hashed":           {doc("$expr", doc("$gt", arr("$x", sym))), nil, false},
		"$expr on sparse":               {doc("$expr", doc("$eq", arr("$s", sym))), nil, false},
		"JavaScript":                    {doc("v", types.JavaScript("abc")), nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkFilterSymbols(context.Background(), idxInfos, tc.filter, tc.collation, nil)
			if got := errors.Is(err, idxpkg.ErrSymbolQueryWithCollation); got != tc.refused {
				t.Fatalf("refused = %v (%v), want %v", got, err, tc.refused)
			}
		})
	}
}
