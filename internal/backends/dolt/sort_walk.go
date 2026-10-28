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
	"context"
	"errors"
	"io"
	"slices"
	"sort"

	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/commonpath"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// sortWalkIndex returns the position in infos of an index whose in-order walk
// yields sort's order: its leading field is the single sort field, and it is
// complete (not sparse, partial, or hidden), exact (not lossy, hashed, text, or
// geo), and collated like the query. A collated index must not be multikey: its
// in-order walk yields a document at its collation-least element, which the
// in-memory sort does not pick. sort must hold one non-$natural key.
func sortWalkIndex(infos []backends.IndexInfo, sortDoc *types.Document, queryCollation *types.Document) (int, bool) {
	if !sortIsSingleField(sortDoc) {
		return 0, false
	}
	queryColl := collation.Parse(queryCollation)
	queryIdentity := queryColl.Identity()
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
		if collation.Parse(idx.Collation).Identity() != queryIdentity {
			continue
		}
		if !queryColl.IsSimple() && idx.Multikey {
			continue
		}
		return i, true
	}
	return 0, false
}

// trySortWalk serves a single-field sort by walking the index led by the sort
// field in sort order, fetching documents lazily so a downstream limit stops
// the walk. It declines (false) when no index qualifies or when the query is
// hinted onto another index.
func (c *collection) trySortWalk(ctx context.Context, state *dbState, primary prolly.Map, params *backends.QueryParams) (*sortWalkIter, bool, error) {
	if (params.Collated && params.Collation == nil) || params.OnlyRecordIDs {
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

	ascending := sortDirectionAscending(params.Sort)
	path, err := types.NewPathFromString(idx.Key[0].Field)
	if err != nil {
		return nil, false, nil
	}
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
		index:   idx.Name,
		ctx:     ctx,
		ns:      state.ns,
		primary: primary,
		entries: mapIter,
		path:    path,
		order:   types.Ascending,
		fields:  params.Fields,
	}
	if !ascending {
		it.order = types.Descending
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

// sortWalkWinsTrial returns the index Query walks for params when its trial
// prefers that walk to the index serving the filter.
func (c *collection) sortWalkWinsTrial(ctx context.Context, params *backends.QueryParams) (string, bool) {
	primary, exists, state, err := c.getMap(ctx)
	if err != nil || !exists {
		return "", false
	}
	plan, err := c.planIndexLookup(ctx, state, primary, params.Filter, params.Hint, params.Collation, false)
	if err != nil || plan == nil {
		return "", false
	}
	iter, outcome, err := c.trySortWalkTrial(ctx, state, primary, params, plan)
	if err != nil || outcome != trialWalkWon {
		return "", false
	}
	iter.Close()
	return iter.rest.index, true
}

// trialMaxResults is the result count at which MongoDB's multi-planner ends
// its trial period.
const trialMaxResults = 101

// trialOutcome is what a sort-walk trial decided.
type trialOutcome int

const (
	// trialNotRun: no competing walk, or the filter plan fits the target.
	trialNotRun trialOutcome = iota
	trialFilterWon
	trialWalkWon
)

// trySortWalkTrial races the sort walk against plan, a filter served by a
// different index, the way MongoDB's multi-planner trial does: plan finishes
// after scanning its keys and then emitting its sorted documents up to the
// trial target, while the walk examines one document per step, filtering as
// it goes. The walk wins when it reaches the target, or its end, first; the
// documents it examined are replayed ahead of the rest of the walk, returned
// with trialWalkWon.
func (c *collection) trySortWalkTrial(ctx context.Context, state *dbState, primary prolly.Map, params *backends.QueryParams, plan *indexLookupPlan) (*replayIter, trialOutcome, error) {
	if params.Hint != nil || plan.union {
		return nil, trialNotRun, nil
	}
	target := trialTarget(params)
	// A filter plan holding no more documents than the target already costs
	// at most its keys plus an in-memory sort of target documents, so the
	// trial cannot pay for itself.
	if len(plan.ids) <= target {
		return nil, trialNotRun, nil
	}
	walk, ok, err := c.trySortWalk(ctx, state, primary, params)
	if err != nil || !ok {
		return nil, trialNotRun, err
	}
	if walk.index == plan.index {
		walk.Close()
		return nil, trialNotRun, nil
	}

	iter, run, err := runWalkWithin(walk, params, filterPlanWorks(plan, params)-1)
	switch {
	case err != nil:
		return nil, trialNotRun, err
	case run == walkReachedTarget:
		return iter, trialWalkWon, nil
	case run == walkOverBudget:
		return nil, trialFilterWon, nil
	}
	return nil, trialNotRun, nil
}

// filterPlanWorks is the trial steps plan takes: its keys, then its sorted
// documents up to the trial target.
func filterPlanWorks(plan *indexLookupPlan, params *backends.QueryParams) int {
	return plan.keys + 1 + min(len(plan.ids), trialTarget(params))
}

// walkRun is how a budgeted walk ended.
type walkRun int

const (
	walkReachedTarget walkRun = iota // the target matched, or the walk ended
	walkOverBudget
	walkFilterFailed // the filter could not be evaluated on a document
)

// runWalkWithin examines walk's documents, filtering as it goes, until the
// trial target matches or the walk ends, and returns them replayed ahead of
// the rest of the walk. After maxSteps documents, or a filter error, it
// closes walk instead.
func runWalkWithin(walk *sortWalkIter, params *backends.QueryParams, maxSteps int) (*replayIter, walkRun, error) {
	target := trialTarget(params)
	var examined []*types.Document
	matched := 0
	for len(examined) < maxSteps {
		_, doc, err := walk.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return &replayIter{docs: examined, rest: walk}, walkReachedTarget, nil
		}
		if err != nil {
			walk.Close()
			return nil, walkFilterFailed, err
		}
		examined = append(examined, doc)
		match, err := backends.MatchPartialFilter(doc, params.Filter)
		if err != nil {
			walk.Close()
			return nil, walkFilterFailed, nil
		}
		if match {
			if matched++; matched == target {
				return &replayIter{docs: examined, rest: walk}, walkReachedTarget, nil
			}
		}
	}
	walk.Close()
	return nil, walkOverBudget, nil
}

// replayIter yields docs, then the rest of the iterator they were read from.
type replayIter struct {
	docs []*types.Document
	rest *sortWalkIter
}

func (it *replayIter) Next() (struct{}, *types.Document, error) {
	if len(it.docs) > 0 {
		doc := it.docs[0]
		it.docs = it.docs[1:]
		return struct{}{}, doc, nil
	}
	return it.rest.Next()
}

func (it *replayIter) Close() {
	it.rest.Close()
}

type sortWalkEntry struct {
	prefix []byte
	id     []byte
}

// sortWalkIter yields documents in index order. Subdocument keys share one
// marker in the index, so a marker group is read whole and ordered by value.
// For a multikey index a document is yielded at its first entry, which in an
// ascending walk is its minimum key and in a descending walk its maximum, the
// value MongoDB sorts it by. Empty arrays have their own marker below every
// value; nested arrays share one marker group, ordered by value like
// subdocuments.
type sortWalkIter struct {
	index   string
	ctx     context.Context
	ns      tree.NodeStore
	primary prolly.Map
	entries prolly.MapIter
	path    types.Path
	order   types.SortType
	fields  []string
	seen    map[string]struct{}
	peeked  *sortWalkEntry
	pending []*types.Document
	release func()
}

func (it *sortWalkIter) sortsBefore(a, b *types.Document) bool {
	return types.CompareOrderForSort(commonpath.SortKeyValue(a, it.path), commonpath.SortKeyValue(b, it.path), it.order) == types.Less
}

func (it *sortWalkIter) sortByValue(docs []*types.Document) {
	sort.SliceStable(docs, func(i, j int) bool { return it.sortsBefore(docs[i], docs[j]) })
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
		countKeysExamined(1)
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
	doc, err := lookupDocFromPrimary(it.ctx, it.primary, it.ns, e.id, it.fields)
	if err != nil || doc == nil {
		return nil, err
	}
	doc.SetRecordID(keyBytesToRecordID(e.id))
	return doc, nil
}

func (it *sortWalkIter) Next() (struct{}, *types.Document, error) {
	doc, err := it.nextWalked()
	if err != nil {
		return struct{}{}, nil, err
	}
	if doc == nil {
		return struct{}{}, nil, iterator.ErrIteratorDone
	}
	return struct{}{}, doc, nil
}

// nextWalked returns the next document in index order, or nil at the end.
func (it *sortWalkIter) nextWalked() (*types.Document, error) {
	for {
		if len(it.pending) > 0 {
			doc := it.pending[0]
			it.pending = it.pending[1:]
			return doc, nil
		}

		e, err := it.nextEntry()
		if err != nil || e == nil {
			return nil, err
		}
		if !idxpkg.LeadingKeyMergesValues(e.prefix) {
			doc, err := it.fetch(e)
			if err != nil {
				return nil, err
			}
			if doc != nil {
				return doc, nil
			}
			continue
		}

		if err := it.readMarkerGroup(e); err != nil {
			return nil, err
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
	it.sortByValue(group)
	it.pending = group
	return nil
}

func (it *sortWalkIter) Close() {
	if it.release != nil {
		it.release()
	}
}

// tryStreamSortedRange serves a limited single-field sort from the range of
// the index chosen for the filter when that range is already in sort order,
// reading index entries and documents only as the limit consumes them.
func tryStreamSortedRange(ctx context.Context, state *dbState, primary prolly.Map, idxInfos []backends.IndexInfo, idxMaps []prolly.Map, params *backends.QueryParams) (types.DocumentsIterator, bool, error) {
	if params.LimitHint <= 0 || params.Filter == nil || params.Filter.Has("$or") {
		return nil, false, nil
	}
	choice, ok := chooseIndexRange(idxInfos, params.Filter, params.Hint, params.Collation)
	if !ok || choice.pointRanges != nil {
		return nil, false, nil
	}
	info := idxInfos[choice.mapIdx]
	reverse, ok := indexScanSortOrder(info, params.Filter, params.Sort, params.Collation)
	if !ok {
		return nil, false, nil
	}
	// A filter field outside the index may reject most of the range, which
	// the stream would then fetch document by document; past half the
	// collection the sequential scan is cheaper.
	if !filterFieldsAreIndexKeys(params.Filter, info) {
		entries, err := idxpkg.RangeCardinality(ctx, idxMaps[choice.mapIdx], choice.startKey, choice.stopKey)
		if err != nil {
			return nil, false, err
		}
		docs, err := primary.Count()
		if err != nil {
			return nil, false, err
		}
		if entries > uint64(docs)/2 {
			return nil, false, nil
		}
	}
	stream, err := idxpkg.NewRangeIDIter(ctx, idxMaps[choice.mapIdx], choice.startKey, choice.stopKey, reverse)
	if err != nil {
		return nil, false, err
	}
	it := newPlanSortedIter(ctx, state, primary, nil, params.Sort, false, params.Fields).(*planSortedIter)
	it.stream = stream
	if it.release != nil {
		releasePrimary := it.release
		releaseIndex := state.backend.backgroundRP.pinRoot(state.name, idxMaps[choice.mapIdx].HashOf())
		it.release = func() {
			releasePrimary()
			releaseIndex()
		}
	}
	return it, true, nil
}

func filterFieldsAreIndexKeys(filter *types.Document, idx backends.IndexInfo) bool {
	for _, f := range filter.Keys() {
		if !slices.ContainsFunc(idx.Key, func(k backends.IndexKeyPair) bool { return k.Field == f }) {
			return false
		}
	}
	return true
}

// sortedIter fetches the plan's documents lazily in index order, or reversed,
// for a scan indexScanSortOrder found to yield sort order, so a downstream
// limit stops the fetches.
func (plan *indexLookupPlan) sortedIter(ctx context.Context, state *dbState, primary prolly.Map, sortDoc *types.Document, reverse bool, fields []string) types.DocumentsIterator {
	return newPlanSortedIter(ctx, state, primary, plan.ids, sortDoc, reverse, fields)
}

// keySortedIter yields the plan's documents in sortDoc's order by sorting its
// entries on their encoded value at index position pos, so a document is
// fetched only when read. An entry whose values do not split is ordered by its
// fetched document's value, encoded the same way.
func (plan *indexLookupPlan) keySortedIter(ctx context.Context, state *dbState, primary prolly.Map, sortDoc *types.Document, pos int, fields []string) (types.DocumentsIterator, error) {
	path := must.NotFail(types.NewPathFromString(sortDoc.Keys()[0]))
	sortKeys := make([][]byte, len(plan.ids))
	for i, values := range plan.values {
		if split, ok := idxpkg.SplitKeyValues(values, len(plan.info.Key)); ok {
			sortKeys[i] = split[pos]
			continue
		}
		doc, err := lookupDocFromPrimary(ctx, primary, state.ns, plan.ids[i], fields)
		if err != nil {
			return nil, err
		}
		if doc != nil {
			sortKeys[i] = idxpkg.EncodeValue(commonpath.SortKeyValue(doc, path))
		}
	}
	order := make([]int, len(plan.ids))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return bytes.Compare(sortKeys[a], sortKeys[b]) })
	ids := make([][]byte, len(order))
	for i, o := range order {
		ids[i] = plan.ids[o]
	}
	return newPlanSortedIter(ctx, state, primary, ids, sortDoc, !sortDirectionAscending(sortDoc), fields), nil
}

// newPlanSortedIter yields the documents of ids, which are in ascending sort
// order, in sortDoc's order: backward when reverse.
func newPlanSortedIter(ctx context.Context, state *dbState, primary prolly.Map, ids [][]byte, sortDoc *types.Document, reverse bool, fields []string) types.DocumentsIterator {
	if reverse {
		ids = slices.Clone(ids)
		slices.Reverse(ids)
	}
	it := &planSortedIter{
		ctx:     ctx,
		ns:      state.ns,
		primary: primary,
		ids:     ids,
		path:    must.NotFail(types.NewPathFromString(sortDoc.Keys()[0])),
		order:   types.Ascending,
		fields:  fields,
	}
	if !sortDirectionAscending(sortDoc) {
		it.order = types.Descending
	}
	if state.backend != nil && state.backend.backgroundRP != nil {
		it.release = state.backend.backgroundRP.pinRoot(state.name, primary.HashOf())
	}
	return it
}

// planSortedIter yields documents in id order. Subdocument sort values share
// one index key, so a run of them is read whole and ordered by value.
type planSortedIter struct {
	ctx     context.Context
	ns      tree.NodeStore
	primary prolly.Map
	ids     [][]byte
	stream  *idxpkg.RangeIDIter
	path    types.Path
	order   types.SortType
	fields  []string
	peeked  *types.Document
	pending []*types.Document
	release func()
}

func (it *planSortedIter) nextFetched() (*types.Document, error) {
	if it.peeked != nil {
		doc := it.peeked
		it.peeked = nil
		return doc, nil
	}
	for {
		id, err := it.nextID()
		if err != nil || id == nil {
			return nil, err
		}
		doc, err := lookupDocFromPrimary(it.ctx, it.primary, it.ns, id, it.fields)
		if err != nil {
			return nil, err
		}
		if doc != nil {
			doc.SetRecordID(keyBytesToRecordID(id))
			return doc, nil
		}
	}
}

func (it *planSortedIter) nextID() ([]byte, error) {
	if it.stream != nil {
		id, err := it.stream.Next(it.ctx)
		if id != nil {
			countKeysExamined(1)
		}
		return id, err
	}
	if len(it.ids) == 0 {
		return nil, nil
	}
	id := it.ids[0]
	it.ids = it.ids[1:]
	return id, nil
}

func (it *planSortedIter) isSubdocument(doc *types.Document) bool {
	_, ok := commonpath.SortKeyValue(doc, it.path).(*types.Document)
	return ok
}

func (it *planSortedIter) Next() (struct{}, *types.Document, error) {
	if len(it.pending) > 0 {
		doc := it.pending[0]
		it.pending = it.pending[1:]
		return struct{}{}, doc, nil
	}
	doc, err := it.nextFetched()
	if err != nil {
		return struct{}{}, nil, err
	}
	if doc == nil {
		return struct{}{}, nil, iterator.ErrIteratorDone
	}
	if !it.isSubdocument(doc) {
		return struct{}{}, doc, nil
	}
	group := []*types.Document{doc}
	for {
		next, err := it.nextFetched()
		if err != nil {
			return struct{}{}, nil, err
		}
		if next == nil {
			break
		}
		if !it.isSubdocument(next) {
			it.peeked = next
			break
		}
		group = append(group, next)
	}
	sort.SliceStable(group, func(i, j int) bool {
		return types.CompareOrderForSort(commonpath.SortKeyValue(group[i], it.path), commonpath.SortKeyValue(group[j], it.path), it.order) == types.Less
	})
	it.pending = group[1:]
	return struct{}{}, group[0], nil
}

func (it *planSortedIter) Close() {
	if it.release != nil {
		it.release()
	}
}
