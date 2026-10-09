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
	"io"
	"sort"

	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// sortWalkIndex returns the position in infos of an index whose in-order walk
// yields sort's order: its leading field is the single sort field, and it is
// complete (not sparse, partial, or hidden), exact (not lossy, hashed, text, or
// geo), and binary-collated like the query. sort must hold one non-$natural key.
func sortWalkIndex(infos []backends.IndexInfo, sortDoc *types.Document, queryCollation *types.Document) (int, bool) {
	if !sortIsSingleField(sortDoc) || !collation.Parse(queryCollation).IsSimple() {
		return 0, false
	}
	switch must.NotFail(sortDoc.Get(sortDoc.Keys()[0])).(type) {
	case int32, int64, float64:
	default:
		return 0, false
	}
	field := sortDoc.Keys()[0]
	for i, idx := range infos {
		if len(idx.Key) == 0 || idx.Key[0].Field != field {
			continue
		}
		lead := idx.Key[0]
		if lead.Hashed || lead.Text || lead.Geo2D || lead.Geo2DSphere {
			continue
		}
		if idx.Lossy || idx.Sparse || idx.Hidden || idx.PartialFilterExpression != nil {
			continue
		}
		if !collation.Parse(idx.Collation).IsSimple() {
			continue
		}
		return i, true
	}
	return 0, false
}

// trySortWalk serves a single-field sort by walking the index led by the sort
// field in sort order, fetching documents lazily so a downstream limit stops
// the walk. It declines (false) when no index qualifies, when the query is
// hinted onto another index, or when a multikey index holds array keys, whose
// marker encoding does not sort where MongoDB sorts arrays.
func (c *collection) trySortWalk(ctx context.Context, state *dbState, primary prolly.Map, params *backends.QueryParams) (types.DocumentsIterator, bool, error) {
	if params.Collated || params.OnlyRecordIDs {
		return nil, false, nil
	}
	state.mu.RLock()
	infos, maps, err := resolveIndexes(ctx, c, state)
	state.mu.RUnlock()
	if err != nil {
		return nil, false, err
	}
	i, ok := sortWalkIndex(infos, params.Sort, params.Collation)
	if !ok {
		return nil, false, nil
	}
	if params.Hint != nil && backends.MatchHintedIndex(params.Hint, infos) != infos[i].Name {
		return nil, false, nil
	}
	idx, idxMap := infos[i], maps[i]
	if idx.Multikey {
		holdsArrays, err := idxpkg.HoldsArrayKeys(ctx, idxMap)
		if err != nil || holdsArrays {
			return nil, false, err
		}
	}

	ascending := sortDirectionAscending(params.Sort)
	var mapIter prolly.MapIter
	if ascending {
		mapIter, err = idxMap.IterAll(ctx)
	} else {
		mapIter, err = idxMap.IterAllReverse(ctx)
	}
	if err != nil {
		return nil, false, err
	}

	it := &sortWalkIter{
		ctx:       ctx,
		ns:        state.ns,
		primary:   primary,
		entries:   mapIter,
		field:     idx.Key[0].Field,
		ascending: ascending,
	}
	if idx.Multikey {
		it.seen = make(map[string]struct{})
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

type sortWalkEntry struct {
	prefix []byte
	id     []byte
}

// sortWalkIter yields documents in index order. Subdocument keys share one
// marker in the index, so a marker group is read whole and ordered by value.
// For a multikey index a document is yielded at its first entry, which in an
// ascending walk is its minimum key and in a descending walk its maximum, the
// value MongoDB sorts it by.
type sortWalkIter struct {
	ctx       context.Context
	ns        tree.NodeStore
	primary   prolly.Map
	entries   prolly.MapIter
	field     string
	ascending bool
	seen      map[string]struct{}
	peeked    *sortWalkEntry
	pending   []*types.Document
	release   func()
}

func (it *sortWalkIter) nextEntry() (*sortWalkEntry, error) {
	if it.peeked != nil {
		e := it.peeked
		it.peeked = nil
		return e, nil
	}
	const primaryIDLen = 20
	keyDesc := idxpkg.KeyDescriptor()
	for {
		k, _, err := it.entries.Next(it.ctx)
		if err == io.EOF || (err == nil && k == nil) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		composite, ok := keyDesc.GetBytes(0, k)
		if !ok || len(composite) < primaryIDLen+1 {
			continue
		}
		idStart := len(composite) - primaryIDLen
		return &sortWalkEntry{
			prefix: composite[:idStart-1],
			id:     append([]byte(nil), composite[idStart:]...),
		}, nil
	}
}

// fetch returns the entry's document, or nil when it was already yielded or no
// longer exists.
func (it *sortWalkIter) fetch(e *sortWalkEntry) (*types.Document, error) {
	if it.seen != nil {
		if _, dup := it.seen[string(e.id)]; dup {
			return nil, nil
		}
		it.seen[string(e.id)] = struct{}{}
	}
	doc, err := lookupDocFromPrimary(it.ctx, it.primary, it.ns, e.id)
	if err != nil || doc == nil {
		return nil, err
	}
	doc.SetRecordID(keyBytesToRecordID(e.id))
	return doc, nil
}

func (it *sortWalkIter) Next() (struct{}, *types.Document, error) {
	for {
		if len(it.pending) > 0 {
			doc := it.pending[0]
			it.pending = it.pending[1:]
			return struct{}{}, doc, nil
		}

		e, err := it.nextEntry()
		if err != nil {
			return struct{}{}, nil, err
		}
		if e == nil {
			return struct{}{}, nil, iterator.ErrIteratorDone
		}

		if !idxpkg.LeadingKeyMergesValues(e.prefix) {
			doc, err := it.fetch(e)
			if err != nil {
				return struct{}{}, nil, err
			}
			if doc != nil {
				return struct{}{}, doc, nil
			}
			continue
		}

		if err := it.readMarkerGroup(e); err != nil {
			return struct{}{}, nil, err
		}
	}
}

// readMarkerGroup buffers every document whose leading key shares first's
// marker and orders them by their sort value.
func (it *sortWalkIter) readMarkerGroup(first *sortWalkEntry) error {
	marker := first.prefix[0]
	var group []*types.Document
	for e := first; e != nil; {
		doc, err := it.fetch(e)
		if err != nil {
			return err
		}
		if doc != nil {
			group = append(group, doc)
		}
		if e, err = it.nextEntry(); err != nil {
			return err
		}
		if e != nil && (len(e.prefix) == 0 || e.prefix[0] != marker) {
			it.peeked = e
			break
		}
	}
	order := types.Ascending
	if !it.ascending {
		order = types.Descending
	}
	sort.SliceStable(group, func(i, j int) bool {
		return types.CompareOrderForSort(indexFieldValue(group[i], it.field), indexFieldValue(group[j], it.field), order) == types.Less
	})
	it.pending = group
	return nil
}

func (it *sortWalkIter) Close() {
	if it.release != nil {
		it.release()
	}
}
