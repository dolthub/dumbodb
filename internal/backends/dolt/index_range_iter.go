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
	"context"

	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// streamIndexRanges serves params' filter by reading its index ranges lazily,
// so a downstream limit stops the reads. It declines when no index serves the
// filter, or when the ranges hold more than half the collection unless a hint
// or a limit on a filter the index fully keys asks for the index.
func streamIndexRanges(ctx context.Context, state *dbState, primary prolly.Map, idxInfos []backends.IndexInfo, idxMaps []prolly.Map, params *backends.QueryParams) (types.DocumentsIterator, bool, error) {
	if params.Filter.Len() == 0 || params.Filter.Has("$or") {
		return nil, false, nil
	}
	choice, ok := chooseIndexRange(idxInfos, params.Filter, params.Hint, params.Collation)
	if !ok {
		return nil, false, nil
	}
	info, idxMap := idxInfos[choice.mapIdx], idxMaps[choice.mapIdx]
	it := &indexRangeIter{
		ctx:     ctx,
		ns:      state.ns,
		primary: primary,
		idxMap:  idxMap,
		ranges:  choice.ranges(),
		fields:  params.Fields,
	}
	if info.Multikey || len(it.ranges) > 1 {
		it.seen = make(map[string]struct{})
	}
	if params.Hint == nil && !(params.LimitHint > 0 && filterFieldsAreIndexKeys(params.Filter, info)) {
		within, err := indexRangesWithinHalf(primary, it.holdsAtMost, func() (uint64, error) {
			return indexRangeEntries(ctx, idxMaps, choice)
		})
		if err != nil || !within {
			return nil, false, err
		}
	}
	if state.backend != nil && state.backend.backgroundRP != nil {
		releasePrimary := state.backend.backgroundRP.pinRoot(state.name, primary.HashOf())
		releaseIndex := state.backend.backgroundRP.pinRoot(state.name, idxMap.HashOf())
		it.release = func() {
			releasePrimary()
			releaseIndex()
		}
	}
	return it, true, nil
}

// indexRangeIter fetches the documents of its index ranges in index order,
// one point lookup per entry as it is read.
type indexRangeIter struct {
	ctx     context.Context
	ns      tree.NodeStore
	primary prolly.Map
	idxMap  prolly.Map
	ranges  [][2][]byte
	current *idxpkg.RangeIDIter
	// buffered holds ids read ahead by holdsAtMost, yielded before current's.
	buffered [][]byte
	// seen holds the ids already yielded when an id can repeat: a multikey
	// index, or more than one range.
	seen    map[string]struct{}
	fields  []string
	release func()
}

func (it *indexRangeIter) Next() (struct{}, *types.Document, error) {
	for {
		id, err := it.nextID()
		if err != nil {
			return struct{}{}, nil, err
		}
		if id == nil {
			return struct{}{}, nil, iterator.ErrIteratorDone
		}
		if it.seen != nil {
			if _, dup := it.seen[string(id)]; dup {
				continue
			}
			it.seen[string(id)] = struct{}{}
		}
		doc, err := lookupDocFromPrimary(it.ctx, it.primary, it.ns, id, it.fields)
		if err != nil {
			return struct{}{}, nil, err
		}
		if doc != nil {
			doc.SetRecordID(keyBytesToRecordID(id))
			return struct{}{}, doc, nil
		}
	}
}

// holdsAtMost reads ahead up to n+1 entries and reports whether the ranges
// end within n.
func (it *indexRangeIter) holdsAtMost(n int) (bool, error) {
	for len(it.buffered) <= n {
		id, err := it.readID()
		if err != nil || id == nil {
			return id == nil, err
		}
		it.buffered = append(it.buffered, id)
	}
	return false, nil
}

// nextID returns the next entry's primary id, or nil after the last.
func (it *indexRangeIter) nextID() ([]byte, error) {
	var id []byte
	if len(it.buffered) > 0 {
		id = it.buffered[0]
		it.buffered = it.buffered[1:]
	} else {
		var err error
		if id, err = it.readID(); err != nil {
			return nil, err
		}
	}
	if id != nil {
		countKeysExamined(1)
	}
	return id, nil
}

// readID reads the next entry's primary id across the ranges, or nil after
// the last.
func (it *indexRangeIter) readID() ([]byte, error) {
	for {
		if it.current == nil {
			if len(it.ranges) == 0 {
				return nil, nil
			}
			r := it.ranges[0]
			it.ranges = it.ranges[1:]
			current, err := idxpkg.NewRangeIDIter(it.ctx, it.idxMap, r[0], r[1], false)
			if err != nil {
				return nil, err
			}
			it.current = current
		}
		id, err := it.current.Next(it.ctx)
		if err != nil || id != nil {
			return id, err
		}
		it.current = nil
	}
}

func (it *indexRangeIter) Close() {
	if it.release != nil {
		it.release()
		it.release = nil
	}
}
