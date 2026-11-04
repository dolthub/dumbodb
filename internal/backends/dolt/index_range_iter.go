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
	"slices"
	"strings"

	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// streamIndexRanges serves params' filter by reading its index ranges lazily,
// so a downstream limit stops the reads. It declines when no index serves the
// filter or the ranges are not worth reading (indexStreamWorthReading).
func streamIndexRanges(ctx context.Context, state *dbState, primary prolly.Map, idxInfos []backends.IndexInfo, idxMaps []prolly.Map, params *backends.QueryParams) (types.DocumentsIterator, backends.IndexInfo, bool, error) {
	if params.Filter.Len() == 0 || params.Filter.Has("$or") {
		return nil, backends.IndexInfo{}, false, nil
	}
	choice, ok := chooseIndexRange(idxInfos, params.Filter, params.Hint, params.Collation, params.Fields)
	if !ok {
		return nil, backends.IndexInfo{}, false, nil
	}
	info, idxMap := idxInfos[choice.mapIdx], idxMaps[choice.mapIdx]
	ids := newIndexIDStream(ctx, idxMap, choice.ranges(), false, info.Multikey)
	worth, err := indexStreamWorthReading(ctx, primary, idxMaps, choice, info, params, ids)
	if err != nil || !worth {
		return nil, backends.IndexInfo{}, false, err
	}
	return &indexRangeIter{
		ctx:     ctx,
		ns:      state.ns,
		primary: primary,
		ids:     ids,
		fields:  params.Fields,
		covered: coveredKeysFor(info, params.Fields),
		release: pinRoots(state, primary, idxMap),
	}, info, true, nil
}

// indexStreamWorthReading reports whether streaming choice's ranges, one point
// lookup per entry, beats a collection scan: always under a hint, when the
// read is covered by the index so it looks nothing up, or under a limit when
// every filter field is a key of the index so each entry read is a match;
// otherwise only when the ranges hold at most half the collection. It reads
// ahead into ids to size a narrow range.
func indexStreamWorthReading(ctx context.Context, primary prolly.Map, idxMaps []prolly.Map, choice indexRangeChoice, info backends.IndexInfo, params *backends.QueryParams, ids *indexIDStream) (bool, error) {
	if params.Hint != nil || coveredKeysFor(info, params.Fields) != nil ||
		(params.LimitHint > 0 && filterFieldsAreIndexKeys(params.Filter, info)) {
		return true, nil
	}
	return indexRangesWithinHalf(primary, ids.holdsAtMost, func() (uint64, error) {
		return indexRangeEntries(ctx, idxMaps, choice)
	})
}

// pinRoots keeps maps' chunks from background GC until the returned release
// runs; it returns nil when nothing collects them.
func pinRoots(state *dbState, maps ...prolly.Map) func() {
	if state.backend == nil || state.backend.backgroundRP == nil {
		return nil
	}
	releases := make([]func(), len(maps))
	for i, m := range maps {
		releases[i] = state.backend.backgroundRP.pinRoot(state.name, m.HashOf())
	}
	return func() {
		for _, release := range releases {
			release()
		}
	}
}

// indexRangeIter fetches the documents of an index id stream, one point
// lookup per id as it is read.
type indexRangeIter struct {
	ctx     context.Context
	ns      tree.NodeStore
	primary prolly.Map
	ids     *indexIDStream
	fields  []string
	// covered, when set, builds documents from their index entries.
	covered *coveredKeys
	release func()
}

func (it *indexRangeIter) Next() (struct{}, *types.Document, error) {
	for {
		e, err := it.ids.next()
		if err != nil {
			return struct{}{}, nil, err
		}
		if e.id == nil {
			return struct{}{}, nil, iterator.ErrIteratorDone
		}
		doc, err := entryDocument(it.ctx, it.primary, it.ns, e, it.fields, it.covered)
		if err != nil {
			return struct{}{}, nil, err
		}
		if doc != nil {
			return struct{}{}, doc, nil
		}
	}
}

// entryDocument returns the document of index entry e: built from the entry
// when covered can, otherwise fetched, nil when it is gone.
func entryDocument(ctx context.Context, primary prolly.Map, ns tree.NodeStore, e indexEntry, fields []string, covered *coveredKeys) (*types.Document, error) {
	doc, ok := covered.document(e)
	if !ok {
		var err error
		if doc, err = lookupDocFromPrimary(ctx, primary, ns, e.id, fields); err != nil || doc == nil {
			return nil, err
		}
	}
	doc.SetRecordID(keyBytesToRecordID(e.id))
	return doc, nil
}

// indexEntry is an index entry as streams yield it: its encoded values, their
// type tags, and its document's primary id, nil after the last entry.
type indexEntry struct {
	values, tags, id []byte
	// gapKeys are the keys examined outside the ranges before this entry.
	gapKeys int
}

// coveredKeys builds a document's requested fields from its index entry, as
// MongoDB's PROJECTION_COVERED reads no document.
type coveredKeys struct {
	keys   int
	fields []string
	// positions holds each field's position among the index keys.
	positions []int
}

// coveredKeysFor returns the coveredKeys for reading fields, top-level fields
// the caller reads, from info's entries, or nil when some field is not a key
// or info is multikey.
func coveredKeysFor(info backends.IndexInfo, fields []string) *coveredKeys {
	if len(fields) == 0 || info.Multikey {
		return nil
	}
	// Geo, hashed and text keys hold no value the field had.
	for _, k := range info.Key {
		if isGeoKey(k) || k.Hashed || k.Text {
			return nil
		}
	}
	c := &coveredKeys{keys: len(info.Key), fields: fields}
	for _, f := range fields {
		pos := slices.IndexFunc(info.Key, func(k backends.IndexKeyPair) bool { return k.Field == f })
		if pos < 0 || strings.Contains(f, ".") {
			return nil
		}
		c.positions = append(c.positions, pos)
	}
	return c
}

// document builds e's document, or reports false when its entry cannot
// restore every field. A null key cannot tell a null from a missing field.
func (c *coveredKeys) document(e indexEntry) (*types.Document, bool) {
	if c == nil || len(e.tags) != c.keys {
		return nil, false
	}
	split, ok := idxpkg.SplitKeyValues(e.values, c.keys)
	if !ok {
		return nil, false
	}
	doc := types.MakeDocument(len(c.fields))
	for i, f := range c.fields {
		v, ok := idxpkg.DecodeValue(split[c.positions[i]], e.tags[c.positions[i]])
		switch v.(type) {
		case types.NullType, types.UndefinedType:
			ok = false
		}
		if !ok {
			return nil, false
		}
		doc.Set(f, v)
	}
	return doc, true
}

func (it *indexRangeIter) Close() {
	if it.release != nil {
		it.release()
		it.release = nil
	}
}

// indexIDStream yields the primary ids of index ranges lazily, in index order
// or reversed, counting each index key as it is consumed.
type indexIDStream struct {
	ctx     context.Context
	idxMap  prolly.Map
	ranges  [][2][]byte
	reverse bool
	current *idxpkg.RangeIDIter
	// buffered holds entries read ahead by holdsAtMost, yielded before
	// current's.
	buffered []indexEntry
	// seen holds the ids already yielded when an id can repeat: a multikey
	// index, or more than one range.
	seen map[string]struct{}
	gaps *intervalGaps
	// reading is the range current reads.
	reading [2][]byte
	// gapKeys are keys examined outside the ranges, carried by the next
	// entry read.
	gapKeys int
}

func newIndexIDStream(ctx context.Context, idxMap prolly.Map, ranges [][2][]byte, reverse, multikey bool) *indexIDStream {
	s := &indexIDStream{ctx: ctx, idxMap: idxMap, ranges: ranges, reverse: reverse, gaps: newIntervalGaps(ctx, idxMap, ranges, reverse)}
	if multikey || len(ranges) > 1 {
		s.seen = make(map[string]struct{})
	}
	return s
}

// holdsAtMost reads ahead up to n+1 entries and reports whether the ranges
// end within n.
func (s *indexIDStream) holdsAtMost(n int) (bool, error) {
	for len(s.buffered) <= n {
		e, err := s.readEntry()
		if err != nil || e.id == nil {
			s.gapKeys += e.gapKeys
			return e.id == nil, err
		}
		s.buffered = append(s.buffered, e)
	}
	return false, nil
}

// next returns the next entry whose id is not yet yielded, with a nil id
// after the last.
func (s *indexIDStream) next() (indexEntry, error) {
	for {
		var e indexEntry
		if len(s.buffered) > 0 {
			e = s.buffered[0]
			s.buffered = s.buffered[1:]
		} else {
			var err error
			if e, err = s.readEntry(); err != nil {
				return indexEntry{}, err
			}
		}
		countKeysExamined(s.ctx, e.gapKeys)
		if e.id == nil {
			return e, nil
		}
		countKeysExamined(s.ctx, 1)
		if s.seen == nil {
			return e, nil
		}
		if _, dup := s.seen[string(e.id)]; !dup {
			s.seen[string(e.id)] = struct{}{}
			return e, nil
		}
	}
}

// readEntry reads the next entry across the ranges, with a nil id after the
// last.
func (s *indexIDStream) readEntry() (indexEntry, error) {
	for {
		if s.current == nil {
			if len(s.ranges) == 0 {
				return s.takeGapKeys(indexEntry{}), nil
			}
			var r [2][]byte
			if s.reverse {
				r, s.ranges = s.ranges[len(s.ranges)-1], s.ranges[:len(s.ranges)-1]
			} else {
				r, s.ranges = s.ranges[0], s.ranges[1:]
			}
			current, err := idxpkg.NewRangeIDIter(s.ctx, s.idxMap, r[0], r[1], s.reverse)
			if err != nil {
				return indexEntry{}, err
			}
			s.current, s.reading = current, r
		}
		values, tags, id, err := s.current.NextEntry(s.ctx)
		if err != nil || id != nil {
			return s.takeGapKeys(indexEntry{values: values, tags: tags, id: id}), err
		}
		s.current = nil
		outside, err := s.gaps.after(s.ctx, s.reading, s.ranges)
		if err != nil {
			return indexEntry{}, err
		}
		if outside {
			s.gapKeys++
		}
	}
}

func (s *indexIDStream) takeGapKeys(e indexEntry) indexEntry {
	e.gapKeys, s.gapKeys = s.gapKeys, 0
	return e
}
