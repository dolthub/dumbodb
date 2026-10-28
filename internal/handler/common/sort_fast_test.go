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
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/handler/commonpath"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestSortDocumentsMatchesStableCompareOrder(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	values := []any{
		int32(1), int32(-1), int64(1), int64(1 << 40), 1.0, 1.5, -0.0, 0.0, math.NaN(), math.Inf(1), math.Inf(-1),
		"a", "B", "", "ab", types.Null, true, false,
		must.NotFail(types.NewArray(int32(3), "z")), must.NotFail(types.NewArray()),
		must.NotFail(types.NewDocument("x", int32(1))),
	}
	for _, dirs := range [][2]int32{{1, 1}, {-1, 1}, {1, -1}, {-1, -1}} {
		docs := make([]*types.Document, 300)
		for i := range docs {
			d := must.NotFail(types.NewDocument("_id", int32(i), "a", values[r.Intn(len(values))]))
			if r.Intn(5) > 0 {
				d.Set("b", values[r.Intn(len(values))])
			}
			docs[i] = d
		}
		sortDoc := must.NotFail(types.NewDocument("a", dirs[0], "b", dirs[1]))

		want := append([]*types.Document(nil), docs...)
		orders := []types.SortType{types.SortType(dirs[0]), types.SortType(dirs[1])}
		paths := []string{"a", "b"}
		sort.SliceStable(want, func(i, j int) bool {
			for k, p := range paths {
				path := must.NotFail(types.NewPathFromString(p))
				av, bv := commonpath.SortKeyValue(want[i], path), commonpath.SortKeyValue(want[j], path)
				switch types.CompareOrderForSort(av, bv, orders[k]) {
				case types.Less:
					return true
				case types.Greater:
					return false
				}
			}
			return false
		})

		require.NoError(t, SortDocuments(docs, sortDoc))
		for i := range docs {
			require.Equal(t, must.NotFail(want[i].Get("_id")), must.NotFail(docs[i].Get("_id")), "dirs %v position %d", dirs, i)
		}
	}
}
