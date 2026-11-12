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
	"container/list"
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// queryShape returns the plan cache key of params: everything that decides
// which plan Query takes, without the filter's values. Queries differing only
// in filter values share a shape; field order is kept because the planner
// picks the first indexed field.
func queryShape(params *backends.QueryParams) string {
	return filterSortShape(params.Filter, params.Sort) + planOptionsShape(params) + fmt.Sprintf("|t:%d", trialTarget(params))
}

// filterSortShape is the part of a shape planCacheClear names.
func filterSortShape(filter, sort *types.Document) string {
	var b strings.Builder
	b.WriteString("f:")
	writeFilterShape(&b, filter)
	b.WriteString("|s:")
	if sort != nil {
		for _, k := range sort.Keys() {
			fmt.Fprintf(&b, "%q=%v,", k, valueOrNil(sort, k))
		}
	}
	return b.String()
}

func planOptionsShape(params *backends.QueryParams) string {
	return fmt.Sprintf("|h:%s|c:%t:%s", hintShape(params.Hint), params.Collated, collation.Parse(params.Collation).Identity())
}

// trialTarget is the result count at which the sort-walk trial ends for params.
func trialTarget(params *backends.QueryParams) int {
	if params.LimitHint > 0 && params.LimitHint < trialMaxResults {
		return int(params.LimitHint)
	}
	return trialMaxResults
}

func valueOrNil(doc *types.Document, key string) any {
	v, err := doc.Get(key)
	if err != nil {
		return nil
	}
	return v
}

func writeFilterShape(b *strings.Builder, filter *types.Document) {
	b.WriteByte('{')
	if filter != nil {
		for _, k := range filter.Keys() {
			b.WriteString(strconv.Quote(k))
			b.WriteByte(':')
			writeValueShape(b, valueOrNil(filter, k))
			b.WriteByte(',')
		}
	}
	b.WriteByte('}')
}

// writeValueShape writes v's structure: operator documents and arrays keep
// their operators and element shapes, other values become their type class.
// A $in or $nin list collapses to the set of its element classes.
func writeValueShape(b *strings.Builder, v any) {
	switch val := v.(type) {
	case *types.Document:
		keys := val.Keys()
		if len(keys) == 0 || !strings.HasPrefix(keys[0], "$") {
			b.WriteString("object")
			return
		}
		b.WriteByte('{')
		for _, op := range keys {
			b.WriteString(strconv.Quote(op))
			b.WriteByte(':')
			operand := valueOrNil(val, op)
			if arr, ok := operand.(*types.Array); ok && (op == "$in" || op == "$nin") {
				writeElementClasses(b, arr)
			} else {
				writeValueShape(b, operand)
			}
			b.WriteByte(',')
		}
		b.WriteByte('}')
	case *types.Array:
		b.WriteByte('[')
		for i := 0; i < val.Len(); i++ {
			elem, _ := val.Get(i)
			if doc, ok := elem.(*types.Document); ok {
				writeFilterShape(b, doc)
			} else {
				writeValueShape(b, elem)
			}
			b.WriteByte(',')
		}
		b.WriteByte(']')
	default:
		b.WriteString(valueClass(v))
	}
}

func writeElementClasses(b *strings.Builder, arr *types.Array) {
	seen := map[string]bool{}
	var classes []string
	for i := 0; i < arr.Len(); i++ {
		elem, _ := arr.Get(i)
		var eb strings.Builder
		writeValueShape(&eb, elem)
		if c := eb.String(); !seen[c] {
			seen[c] = true
			classes = append(classes, c)
		}
	}
	fmt.Fprintf(b, "in(%s)", strings.Join(classes, ","))
}

// valueClass groups values the planner bounds alike; numbers share a class.
func valueClass(v any) string {
	switch v.(type) {
	case int32, int64, float64, types.Decimal128:
		return "number"
	case string:
		return "string"
	case nil, types.NullType:
		return "null"
	case bool:
		return "bool"
	case types.ObjectID:
		return "objectId"
	case time.Time:
		return "date"
	case types.Binary:
		return "binData"
	case types.Timestamp:
		return "timestamp"
	case types.Regex:
		return "regex"
	case types.MinKeyType:
		return "minKey"
	case types.MaxKeyType:
		return "maxKey"
	case types.UndefinedType:
		return "undefined"
	}
	return fmt.Sprintf("%T", v)
}

func hintShape(hint any) string {
	switch h := hint.(type) {
	case nil:
		return ""
	case *types.Document:
		var b strings.Builder
		for _, k := range h.Keys() {
			fmt.Fprintf(&b, "%q=%v,", k, valueOrNil(h, k))
		}
		return b.String()
	}
	return fmt.Sprintf("%q", fmt.Sprint(hint))
}

// planCacheMaxEntries bounds the plan cache across all collections.
const planCacheMaxEntries = 10000

// planCacheReplanFactor is how many times its trial works a cached plan may
// take before it is evicted and the query replanned, as in MongoDB.
const planCacheReplanFactor = 10

// planCacheEntry is the cached outcome of a sort-walk trial: the index the
// walk won on, or empty when the filter's index won, and the trial steps the
// winner took. The rest describes the entry to $planCacheStats.
type planCacheEntry struct {
	walkIndex   string
	filterIndex string
	works       int

	filterSort string
	shapeHash  string
	keyHash    string
	filter     *types.Document
	sort       *types.Document
	limit      int64
	created    time.Time
}

// budget is how many steps a cached plan may take for params before replanning.
func (e planCacheEntry) budget(params *backends.QueryParams) int {
	return max(e.works*planCacheReplanFactor, trialTarget(params))
}

// planCache maps a query's collection, index set, and shape to the plan its
// trial chose, so later queries of that shape skip the trial. Least recently
// used entries are evicted past planCacheMaxEntries. The zero value is ready.
type planCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     list.List
}

type planCacheItem struct {
	key   string
	entry planCacheEntry
}

func (pc *planCache) get(key string) (planCacheEntry, bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	el, ok := pc.entries[key]
	if !ok {
		return planCacheEntry{}, false
	}
	pc.lru.MoveToFront(el)
	return el.Value.(*planCacheItem).entry, true
}

func (pc *planCache) put(key string, entry planCacheEntry) {
	if key == "" {
		return
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.entries == nil {
		pc.entries = make(map[string]*list.Element)
	}
	if el, ok := pc.entries[key]; ok {
		el.Value.(*planCacheItem).entry = entry
		pc.lru.MoveToFront(el)
		return
	}
	pc.entries[key] = pc.lru.PushFront(&planCacheItem{key: key, entry: entry})
	for pc.lru.Len() > planCacheMaxEntries {
		oldest := pc.lru.Back()
		pc.lru.Remove(oldest)
		delete(pc.entries, oldest.Value.(*planCacheItem).key)
	}
}

func (pc *planCache) evict(key string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if el, ok := pc.entries[key]; ok {
		pc.lru.Remove(el)
		delete(pc.entries, key)
	}
}

// clearPrefix evicts every entry whose key starts with prefix.
func (pc *planCache) clearPrefix(prefix string) {
	pc.clearMatching(prefix, func(planCacheEntry) bool { return true })
}

// clearMatching evicts the entries under prefix that match reports.
func (pc *planCache) clearMatching(prefix string, match func(planCacheEntry) bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for key, el := range pc.entries {
		if strings.HasPrefix(key, prefix) && match(el.Value.(*planCacheItem).entry) {
			pc.lru.Remove(el)
			delete(pc.entries, key)
		}
	}
}

// list returns the entries under prefix, most recently used first.
func (pc *planCache) list(prefix string) []planCacheEntry {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	var out []planCacheEntry
	for el := pc.lru.Front(); el != nil; el = el.Next() {
		if item := el.Value.(*planCacheItem); strings.HasPrefix(item.key, prefix) {
			out = append(out, item.entry)
		}
	}
	return out
}

func (pc *planCache) len() int {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.lru.Len()
}

// planCacheDBPrefix scopes keys to a database on every branch.
func planCacheDBPrefix(db string) string {
	return strconv.Quote(db) + "\x00"
}

// planCacheCollectionPrefix scopes keys to one collection on one branch.
func planCacheCollectionPrefix(db, rootish, coll string) string {
	return planCacheDBPrefix(db) + strconv.Quote(rootish) + "\x00" + strconv.Quote(coll) + "\x00"
}

// planCacheKey identifies params' plan on c. The index fingerprint makes any
// index change miss every earlier entry.
func (c *collection) planCacheKey(idxInfos []backends.IndexInfo, params *backends.QueryParams) string {
	return c.planCachePrefix() + indexFingerprint(idxInfos) + "\x00" + queryShape(params)
}

func (c *collection) planCachePrefix() string {
	return planCacheCollectionPrefix(c.db.name, c.db.rootish, c.name)
}

// planCacheHashes returns the planCacheShapeHash and planCacheKey explain and
// $planCacheStats report for params: the shape without the trial target, and
// the shape with the index fingerprint.
func planCacheHashes(idxInfos []backends.IndexInfo, params *backends.QueryParams) (shapeHash, keyHash string) {
	return hash32(filterSortShape(params.Filter, params.Sort) + planOptionsShape(params)),
		hash32(indexFingerprint(idxInfos) + queryShape(params))
}

func hash32(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return fmt.Sprintf("%08X", h.Sum32())
}

// newPlanCacheEntry describes a trial winner for params on c.
func newPlanCacheEntry(idxInfos []backends.IndexInfo, params *backends.QueryParams, walkIndex, filterIndex string, works int) planCacheEntry {
	shapeHash, keyHash := planCacheHashes(idxInfos, params)
	return planCacheEntry{
		walkIndex:   walkIndex,
		filterIndex: filterIndex,
		works:       works,
		filterSort:  filterSortShape(params.Filter, params.Sort),
		shapeHash:   shapeHash,
		keyHash:     keyHash,
		filter:      params.Filter,
		sort:        params.Sort,
		limit:       params.LimitHint,
		created:     time.Now().UTC(),
	}
}

// cachedPlan is the plan tree $planCacheStats reports for e.
func (e planCacheEntry) cachedPlan() *types.Document {
	fetch := func(index string) *types.Document {
		return must.NotFail(types.NewDocument(
			"stage", "FETCH",
			"inputStage", must.NotFail(types.NewDocument("stage", "IXSCAN", "indexName", index)),
		))
	}
	if e.walkIndex != "" {
		plan := fetch(e.walkIndex)
		if e.limit > 0 {
			plan = must.NotFail(types.NewDocument("stage", "LIMIT", "limitAmount", e.limit, "inputStage", plan))
		}
		return plan
	}
	sortStage := must.NotFail(types.NewDocument("stage", "SORT", "sortPattern", e.sort))
	if e.limit > 0 {
		sortStage.Set("limitAmount", e.limit)
	}
	sortStage.Set("inputStage", fetch(e.filterIndex))
	return sortStage
}

// PlanCacheStats implements backends.PlanCacheCollection.
func (c *collection) PlanCacheStats(ctx context.Context) ([]backends.PlanCacheEntry, error) {
	_, exists, _, err := c.getMap(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("collection '%s.%s' does not exist", c.db.name, c.name))
	}
	var out []backends.PlanCacheEntry
	for _, e := range c.db.backend.planCache.list(c.planCachePrefix()) {
		out = append(out, backends.PlanCacheEntry{
			ShapeHash:  e.shapeHash,
			Key:        e.keyHash,
			Filter:     e.filter,
			Sort:       e.sort,
			CachedPlan: e.cachedPlan(),
			Works:      int64(e.works),
			Created:    e.created,
		})
	}
	return out, nil
}

// PlanCacheClear implements backends.PlanCacheCollection.
func (c *collection) PlanCacheClear(_ context.Context, params *backends.PlanCacheClearParams) error {
	if params.Filter == nil {
		c.db.backend.planCache.clearPrefix(c.planCachePrefix())
		return nil
	}
	shape := filterSortShape(params.Filter, params.Sort)
	c.db.backend.planCache.clearMatching(c.planCachePrefix(), func(e planCacheEntry) bool { return e.filterSort == shape })
	return nil
}

// indexFingerprint hashes every index definition and planner-visible flag.
func indexFingerprint(idxInfos []backends.IndexInfo) string {
	h := fnv.New128a()
	for _, idx := range idxInfos {
		fmt.Fprintf(h, "%q|", idx.Name)
		for _, kp := range idx.Key {
			fmt.Fprintf(h, "%q:%+v,", kp.Field, kp)
		}
		fmt.Fprintf(h, "|%t|%t|%t|%t|%t|%d|%t|", idx.Unique, idx.Sparse, idx.Hidden, idx.Lossy, idx.Multikey, idx.MultikeyFields, idx.MultikeyFieldsKnown)
		if idx.PartialFilterExpression != nil {
			var b strings.Builder
			writeFilterShape(&b, idx.PartialFilterExpression)
			fmt.Fprintf(h, "%s%v", b.String(), idx.PartialFilterExpression)
		}
		fmt.Fprintf(h, "|%s\n", collation.Parse(idx.Collation).Identity())
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
