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
	"bytes"
	"container/heap"
	"context"

	"github.com/dolthub/dolt/go/store/prolly"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// tryStreamMergedRanges serves an $in on a compound index's leading field
// sorted on its second, as MongoDB's SORT_MERGE does: each $in value's range
// is already in sort order, so merging them streams the sort, and a limit
// stops after about its documents' keys.
func tryStreamMergedRanges(ctx context.Context, state *dbState, primary prolly.Map, idxInfos []backends.IndexInfo, idxMaps []prolly.Map, params *backends.QueryParams) (types.DocumentsIterator, *explainStage, bool, error) {
	if params.Filter == nil || params.Filter.Has("$or") {
		return nil, nil, false, nil
	}
	choice, ok := chooseIndexRange(idxInfos, params.Filter, params.Hint, params.Collation, params.Fields)
	if !ok || len(choice.pointRanges) < 2 || len(choice.pointRanges) > maxMergedScans {
		return nil, nil, false, nil
	}
	info, idxMap := idxInfos[choice.mapIdx], idxMaps[choice.mapIdx]
	if !mergesSortedRanges(info, params) {
		return nil, nil, false, nil
	}
	reverse := !sortDirectionAscending(params.Sort)
	worth, err := indexStreamWorthReading(ctx, primary, idxMaps, choice, info, params, newIndexIDStream(ctx, idxMap, choice.ranges(), reverse, false))
	if err != nil || !worth {
		return nil, nil, false, err
	}
	stream, err := newMergedIDStream(ctx, idxMap, choice.pointRanges, len(info.Key), reverse)
	if err != nil {
		return nil, nil, false, err
	}
	it := newPlanSortedIter(ctx, state, primary, nil, params.Sort, false, params.Fields).(*planSortedIter)
	it.stream = stream
	it.covered = coveredKeysFor(info, params.Fields)
	if releasePrimary, releaseIndex := it.release, pinRoots(state, idxMap); releasePrimary != nil {
		it.release = func() {
			releasePrimary()
			releaseIndex()
		}
	}
	merge := &explainStage{stage: "SORT_MERGE"}
	for range choice.pointRanges {
		merge.inputs = append(merge.inputs, ixscanStage(info))
	}
	return it, fetchStage(merge), true, nil
}

// maxMergedScans caps the index scans one SORT_MERGE reads, as MongoDB's
// internalQueryMaxScansToExplode does; a larger $in sorts in memory.
const maxMergedScans = 200

// mergesSortedRanges reports whether params' ranges of info, one per $in value
// of its leading field, are each in the order of params' sort: the sort is on
// the second key, both keys ascending, and only the leading field is filtered.
func mergesSortedRanges(info backends.IndexInfo, params *backends.QueryParams) bool {
	if len(info.Key) < 2 || info.Key[0].Descending || info.Key[1].Descending || params.Filter.Len() != 1 {
		return false
	}
	pos, ok := indexKeySortPosition(info, params.Filter, params.Sort, params.Collation)
	if !ok || pos != 1 {
		return false
	}
	in, _ := must.NotFail(params.Filter.Get(info.Key[0].Field)).(*types.Document)
	if in == nil || in.Len() != 1 || in.Keys()[0] != "$in" {
		return false
	}
	values, _ := must.NotFail(in.Get("$in")).(*types.Array)
	for i := 0; values != nil && i < values.Len(); i++ {
		if !scalarOK(must.NotFail(values.Get(i))) {
			return false
		}
	}
	return values != nil
}

// mergedIDStream yields the ids of several index ranges in the order of the
// second index key, taking the lowest (or, reversed, the highest) head next.
type mergedIDStream struct {
	examined examinedCounter
	ctx      context.Context
	reverse  bool
	keys     int
	heads    []mergeHead
	// taken reports that the top head's id was yielded last; it advances on
	// the next call, so a limit stops before reading past what it returns.
	taken bool
}

type mergeHead struct {
	branch  int
	iter    *idxpkg.RangeIDIter
	sortKey []byte
	entry   indexEntry
}

func newMergedIDStream(ctx context.Context, idxMap prolly.Map, ranges [][2][]byte, keys int, reverse bool) (*mergedIDStream, error) {
	s := &mergedIDStream{ctx: ctx, reverse: reverse, keys: keys, taken: false}
	for i, r := range ranges {
		iter, err := idxpkg.NewRangeIDIter(ctx, idxMap, r[0], r[1], reverse)
		if err != nil {
			return nil, err
		}
		head := mergeHead{branch: i, iter: iter}
		if err := s.advance(&head); err != nil {
			return nil, err
		}
		if head.entry.id != nil {
			s.heads = append(s.heads, head)
		}
	}
	heap.Init(s)
	return s, nil
}

// The stream is a heap of its heads, lowest (or, reversed, highest) sort key
// first; equal sort keys keep range order, so a run of them stays together.

func (s *mergedIDStream) Len() int { return len(s.heads) }

func (s *mergedIDStream) Less(i, j int) bool {
	c := bytes.Compare(s.heads[i].sortKey, s.heads[j].sortKey)
	if s.reverse {
		c = -c
	}
	if c != 0 {
		return c < 0
	}
	return s.heads[i].branch < s.heads[j].branch
}

func (s *mergedIDStream) Swap(i, j int) { s.heads[i], s.heads[j] = s.heads[j], s.heads[i] }

func (s *mergedIDStream) Push(x any) { s.heads = append(s.heads, x.(mergeHead)) }

func (s *mergedIDStream) Pop() any {
	last := s.heads[len(s.heads)-1]
	s.heads = s.heads[:len(s.heads)-1]
	return last
}

// advance reads head's next entry, leaving a nil id at its end.
func (s *mergedIDStream) advance(head *mergeHead) error {
	values, tags, id, err := head.iter.NextEntry(s.ctx)
	if err != nil || id == nil {
		head.entry = indexEntry{}
		return err
	}
	s.examined.keys(s.ctx, 1)
	backends.AddBranchExamined(s.ctx, head.branch, 1, 1)
	split, ok := idxpkg.SplitKeyValues(values, s.keys)
	if !ok {
		split = [][]byte{nil, nil}
	}
	head.sortKey, head.entry = split[1], indexEntry{values: values, tags: tags, id: id}
	return nil
}

// next returns the next entry in sort order, with a nil id after the last.
func (s *mergedIDStream) next() (indexEntry, error) {
	if s.taken {
		s.taken = false
		if err := s.advance(&s.heads[0]); err != nil {
			return indexEntry{}, err
		}
		if s.heads[0].entry.id == nil {
			heap.Pop(s)
		} else {
			heap.Fix(s, 0)
		}
	}
	if len(s.heads) == 0 {
		return indexEntry{}, nil
	}
	s.taken = true
	return s.heads[0].entry, nil
}
