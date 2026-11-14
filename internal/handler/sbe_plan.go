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
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// sbePlan is how much of a top-level pipeline mongod 8.0 runs in SBE. Both
// aggregate and its explain take it from classifySBE.
type sbePlan struct {
	// specs are the leading stages given to stages.TopLevelSBEPrefix.
	specs []*types.Document
	// groups is the SBE prefix stages.TopLevelSBEPrefix gives specs.
	groups int
	// end is how many leading stages run in SBE, its $lookup stages included.
	end int
	// pushable bounds the equality $lookup stages that run in SBE.
	pushable int
	// distinct is the $group mongod runs as a distinct scan instead, which
	// leaves the whole pipeline to its classic engine.
	distinct *distinctScanGroup
}

// runsInSBE reports whether mongod runs any of the pipeline in SBE.
func (p sbePlan) runsInSBE() bool {
	return p.end > 0
}

// classifySBE returns how much of specs, the stages of a pipeline reading the
// existing collection named collection, mongod runs in SBE. queryCollation
// is the pipeline's collation. viewInClassic is stages.ViewStagesInClassic of
// a pipeline run on a view, whose first stage SBE could run is then the
// first that it does not.
func classifySBE(ctx context.Context, db backends.Database, collection string, specs []*types.Document, queryCollation *types.Document, viewInClassic bool, isView func(string) bool) sbePlan { //nolint:lll // for readability
	if stages.IsExpressMatch(specs) {
		return sbePlan{}
	}
	if ds := findDistinctScanGroup(ctx, db, collection, specs, queryCollation); ds != nil {
		return sbePlan{distinct: ds}
	}
	if viewInClassic {
		pipeline := make([]any, len(specs))
		for i, spec := range specs {
			pipeline[i] = spec
		}
		if i := stages.SBEStageIndex(pipeline); i >= 0 {
			specs = specs[:i]
		}
	}
	specs = countsAsGroups(specs[:sbeCandidateLen(ctx, db, specs, queryCollation)])
	plan := sbePlan{specs: specs}
	plan.groups, plan.pushable = stages.TopLevelSBEPrefix(specs, isView)
	plan.end = plan.groups
	for i := plan.groups; i < plan.pushable; i++ {
		if specs[i].Command() == "$lookup" {
			plan.end = i + 1
		}
	}
	return plan
}

// sbeCandidateLen is how many leading stages of specs can run in SBE as far
// as the stages around a $lookup go: mongod keeps in its classic engine a
// $lookup that absorbs the $unwind of its join, one joining across
// collations, and a $match after a $lookup that reads the join's output,
// which its optimizer cannot move ahead of the $lookup.
func sbeCandidateLen(ctx context.Context, db backends.Database, specs []*types.Document, queryCollation *types.Document) int {
	outputs, outputsSeen := map[string]int{}, map[string]int{}
	for i, spec := range specs {
		if spec == nil {
			return i
		}
		switch spec.Command() {
		case "$match":
			if len(outputsSeen) == 0 {
				continue
			}
			filter, _ := must.NotFail(spec.Get("$match")).(*types.Document)
			if filter == nil {
				return i
			}
			if dep, ok := lastJoinRead(filter, outputs, outputsSeen); !ok || dep > 0 {
				return i
			}
		case "$lookup":
			lookup, _ := must.NotFail(spec.Get("$lookup")).(*types.Document)
			var next *types.Document
			if i+1 < len(specs) {
				next = specs[i+1]
			}
			if lookup == nil || unwindsJoin(lookup, next) || joinsAcrossCollation(ctx, db, lookup, queryCollation) {
				return i
			}
			as, _ := specString(lookup, "as")
			outputsSeen[as] = 1
			for p := as; ; {
				outputs[p] = 1
				dot := strings.LastIndexByte(p, '.')
				if dot < 0 {
					break
				}
				p = p[:dot]
			}
		}
	}
	return len(specs)
}

// joinsAcrossCollation reports whether the $lookup spec, under a non-simple
// queryCollation, joins on a field that a foreign index of another collation
// leads, the _id index included.
func joinsAcrossCollation(ctx context.Context, db backends.Database, spec, queryCollation *types.Document) bool {
	want := collation.Parse(queryCollation)
	if want.IsSimple() {
		return false
	}
	from, _ := specString(spec, "from")
	field, _ := specString(spec, "foreignField")
	if info, err := lookupCollectionInfo(ctx, db, from); err != nil || info == nil || info.IsView {
		return false
	}
	coll, err := db.Collection(from)
	if err != nil {
		return false
	}
	res, err := coll.ListIndexes(ctx, new(backends.ListIndexesParams))
	if err != nil {
		return false
	}
	for _, idx := range res.Indexes {
		if len(idx.Key) > 0 && idx.Key[0].Field == field && collation.Parse(idx.Collation).Identity() != want.Identity() {
			return true
		}
	}
	return false
}

// countsAsGroups returns specs with each $count replaced by the $group mongod
// runs for it.
func countsAsGroups(specs []*types.Document) []*types.Document {
	var out []*types.Document
	for i, spec := range specs {
		field, _ := spec.Get("$count")
		if f, ok := field.(string); ok && spec.Len() == 1 {
			if out == nil {
				out = slices.Clone(specs)
			}
			out[i] = desugaredCount(f)[0]
		}
	}
	if out == nil {
		return specs
	}
	return out
}

// viewChecker reports which collections of db are views.
func viewChecker(ctx context.Context, db backends.Database) func(string) bool {
	return func(name string) bool {
		info, err := lookupCollectionInfo(ctx, db, name)
		return err == nil && info != nil && info.IsView
	}
}
