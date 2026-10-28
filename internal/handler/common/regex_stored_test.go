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

func TestRegexFilterMatchesStoredRegex(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }
	re := types.Regex{Pattern: "^ab"}
	reI := types.Regex{Pattern: "^ab", Options: "i"}
	for _, tc := range []struct {
		name   string
		value  any
		filter *types.Document
		want   bool
	}{
		{"stored regex", re, doc("s", re), true},
		{"stored regex in an array", arr("x", re), doc("s", re), true},
		{"stored regex with other options", arr(reI), doc("s", re), false},
		{"stored regex in a nested array", arr(arr(re)), doc("s", re), false},
		{"string in an array", arr("abc"), doc("s", re), true},
		{"$all regex against a string", "abc", doc("s", doc("$all", arr(re))), true},
		{"$all regex against an array", arr("x", "abc"), doc("s", doc("$all", arr(re, "x"))), true},
		{"$all regex not matched", arr("x"), doc("s", doc("$all", arr(re, "x"))), false},
		{"$all regex against a stored regex", arr(re), doc("s", doc("$all", arr(re))), true},
		{"$elemMatch regex skips a nested array", arr(arr("abc")), doc("s", doc("$elemMatch", doc("$regex", "^ab"))), false},
		{"$elemMatch $in regex skips a nested array", arr(arr("abc")), doc("s", doc("$elemMatch", doc("$in", arr(types.Regex{Pattern: "ab"})))), false},
		{"$elemMatch $not regex on a nested array", arr(arr("abc")), doc("s", doc("$elemMatch", doc("$not", types.Regex{Pattern: "ab"}))), true},
		{"$elemMatch $in compares a nested array whole", arr(arr("abc")), doc("s", doc("$elemMatch", doc("$in", arr("abc")))), false},
		{"$elemMatch $eq an equal array", arr(arr("abc")), doc("s", doc("$elemMatch", doc("$eq", arr("abc")))), true},
		{"$elemMatch $size of a nested array", arr(arr("abc")), doc("s", doc("$elemMatch", doc("$size", int32(1)))), true},
		{"$elemMatch $regex on an element", arr("x", "abc"), doc("s", doc("$elemMatch", doc("$regex", "^ab"))), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FilterDocument(doc("_id", int32(1), "s", tc.value), tc.filter)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
