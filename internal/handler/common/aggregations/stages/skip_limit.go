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

package stages

import (
	"math"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// skipLimitEntry is a stage spec and its index in the input pipeline, or -1
// for a $limit a $sort absorbed.
type skipLimitEntry struct {
	spec  *types.Document
	index int
}

// OptimizeSkipsAndLimits returns pipeline with its $skip and $limit stages
// moved ahead of the stages they cannot affect, as mongod optimizes it, so the
// documents they drop are never evaluated. readsCollection says the pipeline
// reads a collection, whose query layer takes the leading $skip and $limit
// stages from past every stage that keeps the document count. Only valid $skip
// and $limit stages move, so the other stages are still parsed in order.
func OptimizeSkipsAndLimits(pipeline []any, readsCollection bool) []any {
	specs := make([]*types.Document, len(pipeline))
	for i, v := range pipeline {
		specs[i], _ = v.(*types.Document)
	}
	plan := planSkipsAndLimits(specs, readsCollection)
	if plan == nil {
		return pipeline
	}
	out := make([]any, len(plan))
	for i, e := range plan {
		if e.index < 0 {
			out[i] = e.spec
		} else {
			out[i] = pipeline[e.index]
		}
	}
	return out
}

// OptimizeSkipsAndLimitsArray is OptimizeSkipsAndLimits for a pipeline array.
func OptimizeSkipsAndLimitsArray(pipeline *types.Array, readsCollection bool) *types.Array {
	if pipeline == nil {
		return nil
	}
	specs := make([]*types.Document, pipeline.Len())
	for i := range specs {
		specs[i], _ = must.NotFail(pipeline.Get(i)).(*types.Document)
	}
	plan := planSkipsAndLimits(specs, readsCollection)
	if plan == nil {
		return pipeline
	}
	out := types.MakeArray(len(plan))
	for _, e := range plan {
		if e.index < 0 {
			out.Append(e.spec)
		} else {
			out.Append(must.NotFail(pipeline.Get(e.index)))
		}
	}
	return out
}

// OptimizeSkipsAndLimitsInStages is OptimizeSkipsAndLimits for built stages
// and their specs.
func OptimizeSkipsAndLimitsInStages(built []aggregations.Stage, specs []*types.Document, readsCollection bool) ([]aggregations.Stage, []*types.Document) { //nolint:lll // for readability
	plan := planSkipsAndLimits(specs, readsCollection)
	if plan == nil {
		return built, specs
	}
	outStages := make([]aggregations.Stage, len(plan))
	outSpecs := make([]*types.Document, len(plan))
	for i, e := range plan {
		outSpecs[i] = e.spec
		if e.index < 0 {
			outStages[i] = must.NotFail(newLimit(e.spec))
		} else {
			outStages[i] = built[e.index]
		}
	}
	return outStages, outSpecs
}

// OptimizeSubPipeline returns the stages of a sub-pipeline to run on the
// documents of from, a collection or view whose resolved stages are view, or
// on generated documents when from is empty. When the optimization would move
// a stage into the view's own stages, which run separately, the sub-pipeline
// is optimized on its own as if it did not read a collection.
func OptimizeSubPipeline(sub, view *types.Array, from string) *types.Array {
	if from == "" || view == nil || view.Len() == 0 {
		return OptimizeSkipsAndLimitsArray(sub, from != "")
	}
	specs := make([]*types.Document, 0, view.Len()+sub.Len())
	for _, p := range []*types.Array{view, sub} {
		for i := range p.Len() {
			d, _ := must.NotFail(p.Get(i)).(*types.Document)
			specs = append(specs, d)
		}
	}
	plan := planSkipsAndLimits(specs, true)
	if plan == nil {
		return sub
	}
	for i := range view.Len() {
		if plan[i].index != i {
			return OptimizeSkipsAndLimitsArray(sub, false)
		}
	}
	out := types.MakeArray(len(plan) - view.Len())
	for _, e := range plan[view.Len():] {
		if e.index < 0 {
			out.Append(e.spec)
		} else {
			out.Append(must.NotFail(sub.Get(e.index - view.Len())))
		}
	}
	return out
}

// planSkipsAndLimits returns the optimized order of specs, or nil when it is
// unchanged.
func planSkipsAndLimits(specs []*types.Document, readsCollection bool) []skipLimitEntry {
	plan := make([]skipLimitEntry, len(specs))
	for i, s := range specs {
		plan[i] = skipLimitEntry{spec: s, index: i}
	}
	changed := false
	for i := 0; i < len(plan); i++ {
		if command(plan[i].spec) == "$sort" {
			var absorbed bool
			if plan, absorbed = absorbLimitsIntoSort(plan, i); absorbed {
				changed = true
			}
		}
	}
	if readsCollection {
		start := 0
		for start < len(plan) && (command(plan[start].spec) == "$match" || command(plan[start].spec) == "$sort") {
			start++
		}
		if hoistSkipsAndLimits(plan[start:]) {
			changed = true
		}
	}
	if bubbleSkipsBeforeTransforms(plan) {
		changed = true
	}
	if !changed {
		return nil
	}
	return plan
}

// absorbLimitsIntoSort moves into a $limit right after the $sort at i the
// $limit stages mongod's $sort takes from past the stages that keep the
// document count, counting the documents skipped before each.
func absorbLimitsIntoSort(plan []skipLimitEntry, i int) ([]skipLimitEntry, bool) {
	var skipped int64
	bound := int64(-1)
	absorbed := map[int]bool{}
	for j := i + 1; j < len(plan); j++ {
		spec := plan[j].spec
		if n, ok := skipValue(spec); ok {
			if n > math.MaxInt64-skipped {
				break
			}
			skipped += n
			continue
		}
		if n, ok := limitValue(spec); ok {
			if n > math.MaxInt64-skipped {
				break
			}
			if bound < 0 || n+skipped < bound {
				bound = n + skipped
			}
			absorbed[j] = true
			continue
		}
		if !keepsDocumentCount(spec) {
			break
		}
	}
	if len(absorbed) == 0 || (len(absorbed) == 1 && absorbed[i+1]) {
		return plan, false
	}
	out := make([]skipLimitEntry, 0, len(plan)+1-len(absorbed))
	out = append(out, plan[:i+1]...)
	out = append(out, skipLimitEntry{spec: must.NotFail(types.NewDocument("$limit", bound)), index: -1})
	for j := i + 1; j < len(plan); j++ {
		if !absorbed[j] {
			out = append(out, plan[j])
		}
	}
	return out, true
}

// hoistSkipsAndLimits moves the $skip and $limit stages among the stages
// leading run that keep the document count ahead of them, keeping their
// order, as mongod's query layer takes them.
func hoistSkipsAndLimits(run []skipLimitEntry) bool {
	end := 0
	for end < len(run) && (isSkipOrLimit(run[end].spec) || keepsDocumentCount(run[end].spec)) {
		end++
	}
	hoisted := make([]skipLimitEntry, 0, end)
	var rest []skipLimitEntry
	for _, e := range run[:end] {
		if isSkipOrLimit(e.spec) {
			hoisted = append(hoisted, e)
		} else {
			rest = append(rest, e)
		}
	}
	changed := false
	for i, e := range append(hoisted, rest...) {
		changed = changed || run[i].index != e.index || run[i].spec != e.spec
		run[i] = e
	}
	return changed
}

// bubbleSkipsBeforeTransforms moves each $skip ahead of the stages just before
// it that transform each document.
func bubbleSkipsBeforeTransforms(plan []skipLimitEntry) bool {
	changed := false
	for i := range plan {
		if _, ok := skipValue(plan[i].spec); !ok {
			continue
		}
		for j := i; j > 0 && transformsEachDocument(plan[j-1].spec); j-- {
			plan[j-1], plan[j] = plan[j], plan[j-1]
			changed = true
		}
	}
	return changed
}

// transformsEachDocument reports whether spec maps each document to one
// document, so it commutes with $skip.
func transformsEachDocument(spec *types.Document) bool {
	switch command(spec) {
	case "$project", "$addFields", "$set", "$unset", "$replaceRoot", "$replaceWith":
		return true
	case "$match":
		filter, ok := must.NotFail(spec.Get("$match")).(*types.Document)
		return ok && filter.Len() == 0
	}
	return false
}

// keepsDocumentCount reports whether spec outputs one document per input
// document, so mongod moves a $skip or $limit ahead of it when the query layer
// or a $sort takes them.
func keepsDocumentCount(spec *types.Document) bool {
	switch command(spec) {
	case "$lookup", "$graphLookup":
		return true
	}
	return transformsEachDocument(spec)
}

func isSkipOrLimit(spec *types.Document) bool {
	_, isSkip := skipValue(spec)
	_, isLimit := limitValue(spec)
	return isSkip || isLimit
}

func skipValue(spec *types.Document) (int64, bool) {
	if command(spec) != "$skip" {
		return 0, false
	}
	s, err := newSkip(spec)
	if err != nil {
		return 0, false
	}
	return s.(*skip).value, true
}

func limitValue(spec *types.Document) (int64, bool) {
	if command(spec) != "$limit" {
		return 0, false
	}
	l, err := newLimit(spec)
	if err != nil {
		return 0, false
	}
	return l.(*limit).limit, true
}

func command(stage any) string {
	if d, ok := stage.(*types.Document); ok && d != nil && d.Len() == 1 {
		return d.Command()
	}
	return ""
}
