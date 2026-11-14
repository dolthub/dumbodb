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
// $maxN do. In the classic engine a value that only ties the worst kept
// value is refused, and a better one evicts the worst, which for the
// smallest-keeping sets is the latest of the tied worst values and for the
// largest-keeping sets the earliest. In SBE the heap layout decides (sbe).
type rankedSet struct {
	entries []rankedEntry
	n       int64
	seq     int64
	compare func(a, b any) int
	largest bool
	// sbe keeps entries in the binary heap mongod's SBE engine uses, whose
	// layout decides which ties survive and the order of tied results.
	sbe bool
	// all keeps every entry, as mongod's window accumulators that can remove
	// values do; the n best are chosen from them in order.
	all bool
}

type rankedEntry struct {
	key   any
	value any
	seq   int64
}

func newRankedSet(n int64, compare func(a, b any) int, largest bool, sbe bool) *rankedSet {
	return &rankedSet{n: n, compare: compare, largest: largest, sbe: sbe}
}

// add offers value under key, charging the kept value to mem and releasing
// the one it evicts.
func (s *rankedSet) add(key, value any, mem *keptMemory, op string) error {
	e := rankedEntry{key: key, value: value, seq: s.seq}
	s.seq++
	switch {
	case s.all:
		s.entries = append(s.entries, e)
		return mem.keep(value, op)
	case s.sbe:
		return s.addSBE(e, mem, op)
	}
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

// better reports whether a ranks before b, so that b is evicted first.
func (s *rankedSet) better(a, b rankedEntry) bool {
	c := s.compare(a.key, b.key)
	if s.largest {
		return c > 0
	}
	return c < 0
}

// addSBE is SBE's insertion: below n entries it pushes onto a heap whose root
// is the worst entry; at n it replaces the root only with a strictly better
// entry. The heap operations follow libstdc++'s push_heap and pop_heap.
func (s *rankedSet) addSBE(e rankedEntry, mem *keptMemory, op string) error {
	if int64(len(s.entries)) < s.n {
		s.entries = append(s.entries, e)
		s.pushHeap(len(s.entries)-1, 0, e)
		return mem.keep(e.value, op)
	}
	if !s.better(e, s.entries[0]) {
		return nil
	}
	mem.release(s.entries[0].value)
	last := len(s.entries) - 1
	moved := s.entries[last]
	s.entries[last] = s.entries[0]
	s.adjustHeap(0, last, moved)
	s.entries[last] = e
	s.pushHeap(last, 0, e)
	return mem.keep(e.value, op)
}

// pushHeap is libstdc++'s __push_heap: it moves e up from hole.
func (s *rankedSet) pushHeap(hole, top int, e rankedEntry) {
	parent := (hole - 1) / 2
	for hole > top && s.better(s.entries[parent], e) {
		s.entries[hole] = s.entries[parent]
		hole = parent
		parent = (hole - 1) / 2
	}
	s.entries[hole] = e
}

// adjustHeap is libstdc++'s __adjust_heap over the first length entries.
func (s *rankedSet) adjustHeap(hole, length int, e rankedEntry) {
	top, second := hole, hole
	for second < (length-1)/2 {
		second = 2 * (second + 1)
		if s.better(s.entries[second], s.entries[second-1]) {
			second--
		}
		s.entries[hole] = s.entries[second]
		hole = second
	}
	if length&1 == 0 && second == (length-2)/2 {
		second = 2 * (second + 1)
		s.entries[hole] = s.entries[second-1]
		hole = second - 1
	}
	s.pushHeap(hole, top, e)
}

// sorted returns the kept values in ascending key order, ties in arrival
// order or, for SBE, in heap order.
func (s *rankedSet) sorted() []any {
	if s.sbe {
		return s.sortedSBE(false)
	}
	if s.all {
		// A stable sort of every entry in arrival order: the smallest-keeping
		// sets take the first n, the largest-keeping ones the last n.
		entries := slices.Clone(s.entries)
		slices.SortStableFunc(entries, func(a, b rankedEntry) int { return s.compare(a.key, b.key) })
		if int64(len(entries)) > s.n {
			if s.largest {
				entries = entries[int64(len(entries))-s.n:]
			} else {
				entries = entries[:s.n]
			}
		}
		out := make([]any, len(entries))
		for i, e := range entries {
			out[i] = e.value
		}
		return out
	}
	entries := slices.Clone(s.entries)
	slices.SortFunc(entries, s.order)
	out := make([]any, len(entries))
	for i, e := range entries {
		out[i] = e.value
	}
	return out
}

// sortedSBE returns the kept values in key order, descending if asked, as
// SBE's stable sort of its heap leaves them.
func (s *rankedSet) sortedSBE(descending bool) []any {
	entries := slices.Clone(s.entries)
	slices.SortStableFunc(entries, func(a, b rankedEntry) int {
		if descending {
			return s.compare(b.key, a.key)
		}
		return s.compare(a.key, b.key)
	})
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
