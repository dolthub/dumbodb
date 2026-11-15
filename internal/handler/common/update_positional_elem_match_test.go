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

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// The positional $ after an $elemMatch on the array names the first element
// that satisfies the $elemMatch expression.
func TestFindFirstMatchingIndexElemMatch(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(vs ...any) *types.Array { return must.NotFail(types.NewArray(vs...)) }
	em := func(expr *types.Document) *types.Document { return doc("$elemMatch", expr) }
	for name, tc := range map[string]struct {
		path   string
		target *types.Document
		filter *types.Document
		want   int
	}{
		"scalars":           {"a", doc("a", arr(int32(1), int32(5))), doc("a", em(doc("$gt", int32(3)))), 1},
		"documents":         {"a", doc("a", arr(doc("x", int32(1)), doc("x", int32(5)))), doc("a", em(doc("x", doc("$gt", int32(3))))), 1},
		"dotted path":       {"b.c", doc("b", doc("c", arr(int32(1), int32(5), int32(7)))), doc("b.c", em(doc("$gt", int32(3)))), 1},
		"beside $size":      {"a", doc("a", arr(int32(1), int32(5), int32(9))), doc("a", doc("$elemMatch", doc("$gt", int32(3)), "$size", int32(3))), 1},
		"array element":     {"a", doc("a", arr(int32(2), arr(int32(1)), int32(6))), doc("a", em(doc("$gt", types.MinKey, "$type", "array"))), 1},
		"no element":        {"a", doc("a", arr(int32(5))), doc("a", em(doc("$gt", types.MaxKey))), -1},
		"without elemMatch": {"a", doc("a", arr(int32(1), int32(5))), doc("a", int32(5)), 1},
		"empty":             {"a", doc("a", arr(int32(1), arr(), doc())), doc("a", em(doc())), 1},
		"empty, document":   {"a", doc("a", arr(int32(1), doc(), arr())), doc("a", em(doc())), 1},
	} {
		got, err := positionalMatchIndex(tc.path, tc.target, tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: index %d, want %d", name, got, tc.want)
		}
	}
}

// With several predicates on the array, the positional $ names the element of
// the last predicate to match one, in mongod's evaluation order: $elemMatch,
// then equality, $lte, $lt, $gt, $gte, $regex, $mod, $exists, $in, bit tests and
// $type, ties by path, then filter order.
func TestPositionalMatchIndexOrder(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(vs ...any) *types.Array { return must.NotFail(types.NewArray(vs...)) }
	i := func(v int32) int32 { return v }
	em := func(expr *types.Document) *types.Document { return doc("$elemMatch", expr) }
	nums := doc("a", arr(i(1), i(5), i(9)))
	xy := doc("a", arr(doc("x", i(5), "y", i(0)), doc("x", i(1), "y", i(1)), doc("x", i(6), "y", i(1))))

	for name, tc := range map[string]struct {
		target *types.Document
		filter *types.Document
		want   int
	}{
		"$elemMatch, $lt":            {nums, doc("a", doc("$elemMatch", doc("$gt", i(3)), "$lt", i(6))), 0},
		"$elemMatch, $eq":            {nums, doc("a", doc("$elemMatch", doc("$gt", i(6)), "$eq", i(1))), 0},
		"$elemMatch, $all":           {nums, doc("a", doc("$elemMatch", doc("$gt", i(3)), "$all", arr(i(1)))), 0},
		"$elemMatch, $gt":            {nums, doc("a", doc("$elemMatch", doc("$gt", i(3)), "$gt", i(8))), 2},
		"$elemMatch, one $in":        {nums, doc("a", doc("$elemMatch", doc("$gt", i(3)), "$in", arr(i(9)))), 2},
		"$elemMatch, a.y":            {xy, doc("a", em(doc("x", doc("$gt", i(3)))), "a.y", i(1)), 1},
		"a.y, $elemMatch":            {xy, doc("a.y", i(1), "a", em(doc("x", doc("$gt", i(3))))), 1},
		"a.x $gt, a.y":               {xy, doc("a.x", doc("$gt", i(3)), "a.y", i(1)), 0},
		"a.x $gt, a $elemMatch":      {xy, doc("a.x", doc("$gt", i(3)), "a", em(doc("y", i(1)))), 0},
		"a.y, a.x":                   {xy, doc("a.y", i(1), "a.x", i(6)), 1},
		"one $in, $gt":               {nums, doc("a", doc("$in", arr(i(5)), "$gt", i(8))), 2},
		"$in, $gt":                   {nums, doc("a", doc("$in", arr(i(5), i(1)), "$gt", i(8))), 0},
		"$gt, $lt":                   {nums, doc("a", doc("$gt", i(3), "$lt", i(9))), 1},
		"$all 9 1":                   {nums, doc("a", doc("$all", arr(i(9), i(1)))), 0},
		"$all 1 9":                   {nums, doc("a", doc("$all", arr(i(1), i(9)))), 2},
		"$exists, $gt":               {nums, doc("a", doc("$exists", true, "$gt", i(3))), 0},
		"$type, $in":                 {nums, doc("a", doc("$type", "number", "$in", arr(i(5), i(9)))), 0},
		"$mod, $gt":                  {nums, doc("a", doc("$mod", arr(i(2), i(1)), "$gt", i(3))), 0},
		"$not, $lt":                  {doc("a", arr(i(7), i(5), i(9))), doc("a", doc("$not", doc("$gt", i(100)), "$lt", i(6))), 1},
		"$size, $gt":                 {nums, doc("a", doc("$size", i(3), "$gt", i(3))), 1},
		"whole array, $gt":           {nums, doc("a", doc("$eq", arr(i(1), i(5), i(9)), "$gt", i(3))), 1},
		"$regex, $gte":               {doc("a", arr("x", "b")), doc("a", doc("$regex", types.Regex{Pattern: "b"}, "$gte", "a")), 1},
		"$elemMatch, $all elemMatch": {nums, doc("a", doc("$elemMatch", doc("$gt", i(3)), "$all", arr(em(doc("$gt", i(6)))))), 2},
		"object, value $elemMatch":   {doc("a", arr(doc("y", i(1)), doc("y", i(2)))), doc("a", doc("$elemMatch", doc("y", i(2)), "$all", arr(em(doc("$type", "object"))))), 0},
	} {
		got, err := positionalMatchIndex("a", tc.target, tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: index %d, want %d", name, got, tc.want)
		}
	}
}
