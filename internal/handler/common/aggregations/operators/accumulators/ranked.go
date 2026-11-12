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

package accumulators

import (
	"container/heap"
	"slices"
)

// rankedSet keeps the n best values as mongod's $topN, $bottomN, $minN and
// $maxN do. A value that only ties the worst kept value is refused. A better
// one evicts the worst, which for the smallest-keeping sets is the latest of
// the tied worst values and for the largest-keeping sets the earliest.
type rankedSet struct {
	entries []rankedEntry
	n       int64
	seq     int64
	compare func(a, b any) int
	largest bool
}

type rankedEntry struct {
	key   any
	value any
	seq   int64
}

func newRankedSet(n int64, compare func(a, b any) int, largest bool) *rankedSet {
	return &rankedSet{n: n, compare: compare, largest: largest}
}

// add offers value under key, charging the kept value to mem and releasing
// the one it evicts.
func (s *rankedSet) add(key, value any, mem *keptMemory, op string) error {
	e := rankedEntry{key: key, value: value, seq: s.seq}
	s.seq++
	if int64(len(s.entries)) < s.n {
		heap.Push(s, e)
		return mem.keep(value, op)
	}
	c := s.compare(key, s.entries[0].key)
	if (s.largest && c > 0) || (!s.largest && c < 0) {
		mem.release(s.entries[0].value)
		s.entries[0] = e
		heap.Fix(s, 0)
		return mem.keep(value, op)
	}
	return nil
}

// sorted returns the kept values in ascending key order, ties in arrival
// order.
func (s *rankedSet) sorted() []any {
	entries := slices.Clone(s.entries)
	slices.SortFunc(entries, s.order)
	out := make([]any, len(entries))
	for i, e := range entries {
		out[i] = e.value
	}
	return out
}

func (s *rankedSet) order(a, b rankedEntry) int {
	if c := s.compare(a.key, b.key); c != 0 {
		return c
	}
	return int(a.seq - b.seq)
}

// The heap's root is the worst kept entry.

func (s *rankedSet) Len() int { return len(s.entries) }

func (s *rankedSet) Less(i, j int) bool {
	c := s.order(s.entries[i], s.entries[j])
	if s.largest {
		return c < 0
	}
	return c > 0
}

func (s *rankedSet) Swap(i, j int) { s.entries[i], s.entries[j] = s.entries[j], s.entries[i] }

func (s *rankedSet) Push(x any) { s.entries = append(s.entries, x.(rankedEntry)) }

func (s *rankedSet) Pop() any {
	e := s.entries[len(s.entries)-1]
	s.entries = s.entries[:len(s.entries)-1]
	return e
}
