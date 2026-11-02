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

func TestValidateFilter(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }
	for _, tc := range []struct {
		name   string
		filter *types.Document
		msg    string
	}{
		{"$in operand", doc("f", doc("$in", int32(1))), "$in needs an array"},
		{"after a field", doc("a", int32(1), "f", doc("$in", int32(1))), "$in needs an array"},
		{"after an operator", doc("f", doc("$gt", int32(1), "$mod", arr(int32(0), int32(0)))), "divisor cannot be 0"},
		{"inside $and", doc("$and", arr(doc("a", int32(1)), doc("f", doc("$in", int32(1))))), "$in needs an array"},
		{"inside $nor", doc("$nor", arr(doc("f", doc("$in", int32(1))))), "$in needs an array"},
		{"inside $not", doc("f", doc("$not", doc("$gt", int32(1), "$in", int32(1)))), "$in needs an array"},
		{"inside operator $elemMatch", doc("f", doc("$elemMatch", doc("$gt", int32(1), "$in", int32(1)))), "$in needs an array"},
		{"inside query $elemMatch", doc("f", doc("$elemMatch", doc("a", int32(1), "b", doc("$in", int32(1))))), "$in needs an array"},
		{"empty $and", doc("$and", arr()), "$and/$or/$nor must be a nonempty array"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, ValidateFilter(tc.filter, nil), tc.msg)
		})
	}

	for _, filter := range []*types.Document{
		nil,
		doc(),
		doc("f", int32(1), "g", doc("$in", arr(int32(1))), "h", doc("$regex", "^a", "$options", "i")),
		doc("f", doc("$exists", false), "g", doc("$type", "string"), "h", doc("$size", int32(2))),
		doc("$or", arr(doc("a", int32(1)), doc("b", doc("$gt", int32(2))))),
		doc("f", doc("$elemMatch", doc("a", int32(1), "b", doc("$lt", int32(2))))),
		doc("f", doc("$not", doc("$regex", "^a"))),
		doc("$expr", doc("$divide", arr("$a", int32(0)))),
		doc("f", doc("a", int32(1))),
	} {
		require.NoError(t, ValidateFilter(filter, nil), "filter %v", filter)
	}
}
