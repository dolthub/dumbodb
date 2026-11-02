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

// Package stdsort is libstdc++'s std::sort, so that elements that compare
// equal end up in the same order as in MongoDB, which sorts with it.
package stdsort

import "math/bits"

// threshold is libstdc++'s _S_threshold: shorter ranges are left to the
// final insertion sort.
const threshold = 16

// Sort sorts s by less as std::sort does: an introsort with median-of-three
// pivots that falls back to heapsort, then an insertion sort.
func Sort[T any](s []T, less func(a, b T) bool) {
	if len(s) < 2 {
		return
	}
	introsortLoop(s, 0, len(s), 2*(bits.Len(uint(len(s)))-1), less)
	finalInsertionSort(s, 0, len(s), less)
}

func introsortLoop[T any](s []T, first, last, depthLimit int, less func(a, b T) bool) {
	for last-first > threshold {
		if depthLimit == 0 {
			heapSort(s[first:last], less)
			return
		}
		depthLimit--
		cut := unguardedPartitionPivot(s, first, last, less)
		introsortLoop(s, cut, last, depthLimit, less)
		last = cut
	}
}

func unguardedPartitionPivot[T any](s []T, first, last int, less func(a, b T) bool) int {
	mid := first + (last-first)/2
	moveMedianToFirst(s, first, first+1, mid, last-1, less)
	return unguardedPartition(s, first+1, last, first, less)
}

func moveMedianToFirst[T any](s []T, result, a, b, c int, less func(a, b T) bool) {
	switch {
	case less(s[a], s[b]):
		switch {
		case less(s[b], s[c]):
			s[result], s[b] = s[b], s[result]
		case less(s[a], s[c]):
			s[result], s[c] = s[c], s[result]
		default:
			s[result], s[a] = s[a], s[result]
		}
	case less(s[a], s[c]):
		s[result], s[a] = s[a], s[result]
	case less(s[b], s[c]):
		s[result], s[c] = s[c], s[result]
	default:
		s[result], s[b] = s[b], s[result]
	}
}

func unguardedPartition[T any](s []T, first, last, pivot int, less func(a, b T) bool) int {
	for {
		for less(s[first], s[pivot]) {
			first++
		}
		last--
		for less(s[pivot], s[last]) {
			last--
		}
		if first >= last {
			return first
		}
		s[first], s[last] = s[last], s[first]
		first++
	}
}

func finalInsertionSort[T any](s []T, first, last int, less func(a, b T) bool) {
	if last-first > threshold {
		insertionSort(s, first, first+threshold, less)
		for i := first + threshold; i < last; i++ {
			unguardedLinearInsert(s, i, less)
		}
		return
	}
	insertionSort(s, first, last, less)
}

func insertionSort[T any](s []T, first, last int, less func(a, b T) bool) {
	for i := first + 1; i < last; i++ {
		if less(s[i], s[first]) {
			v := s[i]
			copy(s[first+1:i+1], s[first:i])
			s[first] = v
		} else {
			unguardedLinearInsert(s, i, less)
		}
	}
}

func unguardedLinearInsert[T any](s []T, last int, less func(a, b T) bool) {
	v := s[last]
	next := last - 1
	for less(v, s[next]) {
		s[last] = s[next]
		last = next
		next--
	}
	s[last] = v
}

// heapSort is std::partial_sort over the whole range: make_heap, then
// sort_heap.
func heapSort[T any](s []T, less func(a, b T) bool) {
	n := len(s)
	for parent := (n - 2) / 2; parent >= 0; parent-- {
		adjustHeap(s, parent, n, s[parent], less)
	}
	for last := n - 1; last > 0; last-- {
		v := s[last]
		s[last] = s[0]
		adjustHeap(s, 0, last, v, less)
	}
}

func adjustHeap[T any](s []T, hole, n int, v T, less func(a, b T) bool) {
	top := hole
	child := hole
	for child < (n-1)/2 {
		child = 2 * (child + 1)
		if less(s[child], s[child-1]) {
			child--
		}
		s[hole] = s[child]
		hole = child
	}
	if n&1 == 0 && child == (n-2)/2 {
		child = 2 * (child + 1)
		s[hole] = s[child-1]
		hole = child - 1
	}

	parent := (hole - 1) / 2
	for hole > top && less(s[parent], v) {
		s[hole] = s[parent]
		hole = parent
		parent = (hole - 1) / 2
	}
	s[hole] = v
}
