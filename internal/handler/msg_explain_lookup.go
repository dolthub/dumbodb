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

package handler

import (
	"context"
	"slices"
	"strings"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// hashJoinMaxDocs is the largest foreign collection MongoDB joins without an
// index by hashing it (internalQueryCollectionMaxNoOfDocumentsToChooseHashJoin).
const hashJoinMaxDocs = 10000

// explainLookups adds an explained pipeline's $lookup stages to response the
// way MongoDB 8.0 reports them. Equality $lookups that directly follow the
// stages pushed into the query run in its plan, as EQ_LOOKUP stages over the
// query plan. Any other stage moves the explain into a stages array: the query
// under $cursor, then each remaining stage, a $lookup naming at
// executionStats verbosity the foreign indexes it reads.
func explainLookups(ctx context.Context, db backends.Database, dbName string, pipeline []any, response *types.Document, execStats, collated bool) {
	fetcher := &foreignFetcher{db: db}
	var rest []*types.Document
	var eqLookups []*types.Document
	hasLookup := false
	for i, s := range pipeline {
		stage, _ := s.(*types.Document)
		if stage == nil || stage.Len() == 0 {
			return
		}
		var next *types.Document
		if i+1 < len(pipeline) {
			next, _ = pipeline[i+1].(*types.Document)
		}
		name := stage.Command()
		if name == "$lookup" {
			hasLookup = true
		}
		if rest == nil {
			switch name {
			case "$match", "$sort", "$limit", "$skip":
				continue
			case "$lookup":
				if spec, _ := must.NotFail(stage.Get(name)).(*types.Document); spec != nil && !collated &&
					!unwindsJoin(spec, next) && runsInQueryPlan(ctx, db, spec) {
					eqLookups = append(eqLookups, spec)
					continue
				}
			}
		}
		rest = append(rest, stage)
	}
	if !hasLookup {
		return
	}

	queryPlanner, _ := must.NotFail(response.Get("queryPlanner")).(*types.Document)
	if len(eqLookups) > 0 {
		plan, _ := must.NotFail(queryPlanner.Get("winningPlan")).(*types.Document)
		for _, spec := range eqLookups {
			plan = eqLookupStage(ctx, db, dbName, fetcher, spec, plan)
		}
		queryPlanner.Set("winningPlan", must.NotFail(types.NewDocument("queryPlan", plan)))
		// A plan joining in the query runs in SBE.
		response.Set("explainVersion", "2")
	}
	if len(rest) == 0 {
		return
	}

	out := cursorStage(response, queryPlanner)
	for i := 0; i < len(rest); i++ {
		stage := rest[i]
		entry := stage.DeepCopy()
		// A classic $lookup absorbs the $unwind of its join array.
		if spec, _ := must.NotFail(entry.Get(entry.Command())).(*types.Document); entry.Command() == "$lookup" && spec != nil &&
			i+1 < len(rest) && unwindsJoin(spec, rest[i+1]) {
			spec.Set("unwinding", unwindingSpec(rest[i+1]))
			i++
		}
		if spec, _ := must.NotFail(stage.Get(stage.Command())).(*types.Document); execStats && stage.Command() == "$lookup" && spec != nil {
			used := types.MakeArray(0)
			for _, name := range lookupIndexesUsed(ctx, fetcher, spec) {
				used.Append(name)
			}
			entry.Set("indexesUsed", used)
		}
		out.Append(entry)
	}
	response.Set("stages", out)
}

// runsInQueryPlan reports whether MongoDB runs the $lookup spec inside the
// query plan: an equality join of a collection, with no sub-pipeline.
func runsInQueryPlan(ctx context.Context, db backends.Database, spec *types.Document) bool {
	for _, k := range []string{"pipeline", "let"} {
		if spec.Has(k) {
			return false
		}
	}
	for _, k := range []string{"from", "localField", "foreignField", "as"} {
		if _, ok := specString(spec, k); !ok {
			return false
		}
	}
	from, _ := specString(spec, "from")
	info, err := lookupCollectionInfo(ctx, db, from)
	return err == nil && (info == nil || !info.IsView)
}

func specString(spec *types.Document, key string) (string, bool) {
	v, _ := spec.Get(key)
	s, ok := v.(string)
	return s, ok
}

// eqLookupStage is the EQ_LOOKUP stage joining spec's foreign collection to
// the documents input returns.
func eqLookupStage(ctx context.Context, db backends.Database, dbName string, fetcher *foreignFetcher, spec, input *types.Document) *types.Document {
	from, _ := specString(spec, "from")
	localField, _ := specString(spec, "localField")
	foreignField, _ := specString(spec, "foreignField")
	as, _ := specString(spec, "as")
	stage := must.NotFail(types.NewDocument(
		"stage", "EQ_LOOKUP",
		"foreignCollection", dbName+"."+from,
		"localField", localField,
		"foreignField", foreignField,
		"asField", as,
		"scanDirection", "forward",
	))
	if info, err := lookupCollectionInfo(ctx, db, from); err == nil && info == nil {
		stage.Set("strategy", "NonExistentForeignCollection")
	} else if idx, ok := fetcher.indexOn(ctx, from, stages.LookupJoinField(spec)); ok {
		stage.Set("strategy", "IndexedLoopJoin")
		stage.Set("indexName", idx.Name)
		stage.Set("indexKeyPattern", joinKeyPattern(idx))
	} else {
		stage.Set("strategy", unindexedJoinStrategy(ctx, db, from))
	}
	stage.Set("inputStage", input)
	return stage
}

func unindexedJoinStrategy(ctx context.Context, db backends.Database, from string) string {
	if coll, err := db.Collection(from); err == nil {
		if res, err := coll.Count(ctx, new(backends.CountParams)); err == nil && res.Count <= hashJoinMaxDocs {
			return "HashJoin"
		}
	}
	return "NestedLoopJoin"
}

func joinKeyPattern(idx backends.IndexInfo) *types.Document {
	pattern := types.MakeDocument(len(idx.Key))
	for _, k := range idx.Key {
		order := int32(1)
		if k.Descending {
			order = -1
		}
		pattern.Set(k.Field, order)
	}
	return pattern
}

// unwindsJoin reports whether next, the stage after the $lookup spec,
// unwinds the array the join writes, which MongoDB folds into a classic
// $lookup.
func unwindsJoin(spec, next *types.Document) bool {
	if next == nil || next.Command() != "$unwind" {
		return false
	}
	as, _ := specString(spec, "as")
	path, _ := must.NotFail(next.Get("$unwind")).(string)
	if d, ok := must.NotFail(next.Get("$unwind")).(*types.Document); ok {
		path, _ = specString(d, "path")
	}
	return path == "$"+as
}

// unwindingSpec is the unwinding field of a $lookup that absorbs unwind.
func unwindingSpec(unwind *types.Document) *types.Document {
	out := must.NotFail(types.NewDocument("preserveNullAndEmptyArrays", false))
	if d, ok := must.NotFail(unwind.Get("$unwind")).(*types.Document); ok {
		if v, _ := d.Get("preserveNullAndEmptyArrays"); v != nil {
			out.Set("preserveNullAndEmptyArrays", v)
		}
		if v, _ := d.Get("includeArrayIndex"); v != nil {
			out.Set("includeArrayIndex", v)
		}
	}
	return out
}

// moveMatchesBeforeLookups moves each $match that follows a $lookup ahead of
// it when the match does not read the join's output, as mongod's optimizer
// does, so the match can reach the query and an $unwind can follow its join.
// A $match moves past $lookups only, never past another $match, so within a
// run of $lookup and $match stages each match lands just after the last
// $lookup whose output it reads, and no earlier than the match before it.
// One pass: each match looks up its fields against the run's join outputs.
func moveMatchesBeforeLookups(pipeline []any) []any {
	out := make([]any, 0, len(pipeline))
	var run []any
	flush := func() { out = append(out, run...); run = run[:0] }
	var (
		lookups     []*types.Document // the run's $lookups, in order
		slots       [][]*types.Document
		lastSlot    int
		outputs     map[string]int // a join output path, or a prefix of one, to its latest $lookup
		outputsSeen map[string]int // a join output path to its latest $lookup
	)
	reset := func() {
		lookups, slots, lastSlot = nil, [][]*types.Document{nil}, 0
		outputs, outputsSeen = map[string]int{}, map[string]int{}
	}
	emitRun := func() {
		for i, l := range lookups {
			for _, m := range slots[i] {
				run = append(run, m)
			}
			run = append(run, l)
		}
		for _, m := range slots[len(lookups)] {
			run = append(run, m)
		}
		flush()
		reset()
	}
	reset()
	for _, stage := range pipeline {
		d, _ := stage.(*types.Document)
		switch {
		case d != nil && d.Len() > 0 && d.Command() == "$lookup":
			spec, _ := must.NotFail(d.Get("$lookup")).(*types.Document)
			as, ok := specString(spec, "as")
			n := len(lookups) + 1
			if ok {
				outputsSeen[as] = n
				for p := as; ; {
					outputs[p] = n
					i := strings.LastIndexByte(p, '.')
					if i < 0 {
						break
					}
					p = p[:i]
				}
			}
			lookups = append(lookups, d)
			slots = append(slots, nil)
		case d != nil && d.Len() > 0 && d.Command() == "$match" && len(lookups) > 0:
			slot := len(lookups)
			if filter, ok := must.NotFail(d.Get("$match")).(*types.Document); ok {
				if dep, ok := lastJoinRead(filter, outputs, outputsSeen); ok {
					slot = dep
				}
			}
			slot = max(slot, lastSlot)
			lastSlot = slot
			slots[slot] = append(slots[slot], d)
		default:
			emitRun()
			out = append(out, stage)
		}
	}
	emitRun()
	return out
}

// lastJoinRead returns the latest $lookup (1-based, 0 for none) whose output
// filter may read, or false when filter holds an operator it cannot follow.
func lastJoinRead(filter *types.Document, outputs, outputsSeen map[string]int) (int, bool) {
	latest := 0
	for _, k := range filter.Keys() {
		switch {
		case k == "$and" || k == "$or" || k == "$nor":
			arr, ok := must.NotFail(filter.Get(k)).(*types.Array)
			if !ok {
				return 0, false
			}
			for i := 0; i < arr.Len(); i++ {
				sub, ok := must.NotFail(arr.Get(i)).(*types.Document)
				if !ok {
					return 0, false
				}
				n, ok := lastJoinRead(sub, outputs, outputsSeen)
				if !ok {
					return 0, false
				}
				latest = max(latest, n)
			}
		case strings.HasPrefix(k, "$"):
			return 0, false
		default:
			// The field is a join output, under one, or above one.
			latest = max(latest, outputs[k])
			for p := k; ; {
				latest = max(latest, outputsSeen[p])
				i := strings.LastIndexByte(p, '.')
				if i < 0 {
					break
				}
				p = p[:i]
			}
		}
	}
	return latest, true
}

// lookupIndexesUsed returns the foreign indexes the $lookup spec reads,
// including those of $lookups in its sub-pipeline.
func lookupIndexesUsed(ctx context.Context, fetcher *foreignFetcher, spec *types.Document) []string {
	var used []string
	from, _ := specString(spec, "from")
	// A join of a view reads its base collection's indexes.
	if info, err := lookupCollectionInfo(ctx, fetcher.db, from); err == nil && info != nil && info.IsView {
		if base, _, _, err := resolveViewChain(ctx, fetcher.db, info.Name, info.ViewOn, info.ViewPipeline, nil); err == nil {
			from = base
		}
	}
	if idx, ok := fetcher.indexOn(ctx, from, stages.LookupJoinField(spec)); ok {
		used = append(used, idx.Name)
	}
	v, _ := spec.Get("pipeline")
	pipeline, _ := v.(*types.Array)
	for i := 0; pipeline != nil && i < pipeline.Len(); i++ {
		stage, _ := must.NotFail(pipeline.Get(i)).(*types.Document)
		if stage == nil || stage.Len() == 0 || stage.Command() != "$lookup" {
			continue
		}
		if inner, _ := must.NotFail(stage.Get("$lookup")).(*types.Document); inner != nil {
			for _, name := range lookupIndexesUsed(ctx, fetcher, inner) {
				if !slices.Contains(used, name) {
					used = append(used, name)
				}
			}
		}
	}
	return used
}
