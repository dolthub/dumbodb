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

package stdsort

import (
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

type keyed struct{ key, id int }

func byKey(a, b keyed) bool { return a.key < b.key }

func TestSortOrders(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewSource(1))
	for _, n := range []int{0, 1, 2, 15, 16, 17, 100, 1000} {
		s := make([]keyed, n)
		for i := range s {
			s[i] = keyed{key: r.Intn(10), id: i}
		}
		Sort(s, byKey)
		assert.True(t, sort.SliceIsSorted(s, func(i, j int) bool { return s[i].key < s[j].key }), "n=%d", n)
	}
}

func TestSortShortRangesAreStable(t *testing.T) {
	t.Parallel()

	s := []keyed{{1, 0}, {0, 1}, {1, 2}, {0, 3}, {1, 4}}
	Sort(s, byKey)
	assert.Equal(t, []keyed{{0, 1}, {0, 3}, {1, 0}, {1, 2}, {1, 4}}, s)
}

func TestHeapSortFallback(t *testing.T) {
	t.Parallel()

	s := make([]keyed, 300)
	for i := range s {
		s[i] = keyed{key: (i * 37) % 11, id: i}
	}
	heapSort(s, byKey)
	assert.True(t, sort.SliceIsSorted(s, func(i, j int) bool { return s[i].key < s[j].key }))
}
