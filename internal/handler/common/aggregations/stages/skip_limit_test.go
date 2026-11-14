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

package stages_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestOptimizeSkipsAndLimits(t *testing.T) {
	t.Parallel()

	set := must.NotFail(types.NewDocument("$set", must.NotFail(types.NewDocument("x", int32(1)))))
	emptyMatch := must.NotFail(types.NewDocument("$match", types.MakeDocument(0)))
	match := must.NotFail(types.NewDocument("$match", must.NotFail(types.NewDocument("a", int32(1)))))
	sort := must.NotFail(types.NewDocument("$sort", must.NotFail(types.NewDocument("a", int32(1)))))
	lookup := must.NotFail(types.NewDocument("$lookup", types.MakeDocument(0)))
	group := must.NotFail(types.NewDocument("$group", types.MakeDocument(0)))
	skip := func(n any) *types.Document { return must.NotFail(types.NewDocument("$skip", n)) }
	limit := func(n any) *types.Document { return must.NotFail(types.NewDocument("$limit", n)) }

	for name, tc := range map[string]struct {
		pipeline        []any
		readsCollection bool
		expected        string
	}{
		"SkipBeforeTransforms": {
			pipeline: []any{set, emptyMatch, skip(int32(2))},
			expected: "$skip:2 $set $match",
		},
		"SkipStopsAtMatch": {
			pipeline: []any{set, match, skip(int32(2))},
			expected: "$set $match $skip:2",
		},
		"SkipStopsAtLookup": {
			pipeline: []any{set, lookup, skip(int32(2))},
			expected: "$set $lookup $skip:2",
		},
		"LimitStays": {
			pipeline: []any{group, set, limit(int32(5)), skip(int32(2))},
			expected: "$group $set $limit:5 $skip:2",
		},
		"InvalidSkipStays": {
			pipeline: []any{set, skip("bad")},
			expected: "$set $skip:bad",
		},
		"QueryLayerTakesSkipAndLimit": {
			pipeline:        []any{set, lookup, limit(int32(5)), set, skip(int32(2)), group},
			readsCollection: true,
			expected:        "$limit:5 $skip:2 $set $lookup $set $group",
		},
		"QueryLayerAfterMatchAndSort": {
			pipeline:        []any{match, sort, set, limit(int32(5)), skip(int32(2))},
			readsCollection: true,
			expected:        "$match $sort $limit:5 $skip:2 $set",
		},
		"QueryLayerStopsAtGroup": {
			pipeline:        []any{group, set, limit(int32(5)), skip(int32(2))},
			readsCollection: true,
			expected:        "$group $set $limit:5 $skip:2",
		},
		"SortTakesLimits": {
			pipeline: []any{group, sort, set, skip(int32(1)), limit(int32(5)), lookup, limit(int32(9)), skip(int32(1))},
			expected: "$group $sort $limit:6 $skip:1 $set $lookup $skip:1",
		},
		"SortKeepsAdjacentLimit": {
			pipeline: []any{group, sort, limit(int32(5)), set},
			expected: "$group $sort $limit:5 $set",
		},
		"SortLimitOverflow": {
			pipeline: []any{group, sort, set, skip(int64(1 << 62)), skip(int64(1 << 62)), limit(int64(1 << 62))},
			expected: "$group $sort $skip:4611686018427387904 $skip:4611686018427387904 $set $limit:4611686018427387904",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var actual []string
			for _, v := range stages.OptimizeSkipsAndLimits(tc.pipeline, tc.readsCollection) {
				d := v.(*types.Document)
				s := d.Command()
				switch arg := must.NotFail(d.Get(s)).(type) {
				case *types.Document:
				default:
					s += fmt.Sprintf(":%v", arg)
				}
				actual = append(actual, s)
			}
			assert.Equal(t, tc.expected, fmt.Sprint(actual)[1:len(fmt.Sprint(actual))-1])
		})
	}
}
