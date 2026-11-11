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
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// countingPipeline reports whether pipeline opens with an optional $match and
// then counts its documents, by a $count or a $group of every document into
// one {$sum: 1}, as countDocuments does; MongoDB runs that much in its query
// plan. It returns the stages after the count, which hold no $lookup; a
// $count is followed by the $project MongoDB rewrites it into.
func countingPipeline(pipeline []any) ([]any, bool) {
	i := 0
	if first, _ := firstStage(pipeline); first != nil && first.Command() == "$match" {
		i = 1
	}
	if i >= len(pipeline) {
		return nil, false
	}
	stage, _ := pipeline[i].(*types.Document)
	if stage == nil || stage.Len() != 1 {
		return nil, false
	}
	var rest []any
	switch stage.Command() {
	case "$count":
		field, ok := must.NotFail(stage.Get("$count")).(string)
		if !ok {
			return nil, false
		}
		rest = append(rest, must.NotFail(types.NewDocument("$project", must.NotFail(types.NewDocument(field, true, "_id", false)))))
	case "$group":
		if _, ok := tryCountAggregateShortcut(pipeline[:i+1]); !ok {
			return nil, false
		}
	default:
		return nil, false
	}
	for _, s := range pipeline[i+1:] {
		if d, _ := s.(*types.Document); d == nil || d.Len() == 0 || d.Command() == "$lookup" {
			return nil, false
		}
	}
	return append(rest, pipeline[i+1:]...), true
}

func firstStage(pipeline []any) (*types.Document, bool) {
	if len(pipeline) == 0 {
		return nil, false
	}
	d, ok := pipeline[0].(*types.Document)
	return d, ok && d.Len() > 0
}

// explainCountingGroup reshapes the explain of a counting pipeline the way
// MongoDB 8.0 reports a plan its SBE engine runs: explainVersion 2, the
// winning plan under queryPlan, and any stages after the count following the
// query under $cursor.
func explainCountingGroup(response *types.Document, rest []any) {
	response.Set("explainVersion", "2")
	queryPlanner, _ := must.NotFail(response.Get("queryPlanner")).(*types.Document)
	if plan, _ := must.NotFail(queryPlanner.Get("winningPlan")).(*types.Document); plan != nil {
		cached, _ := plan.Get("isCached")
		plan.Remove("isCached")
		queryPlanner.Set("winningPlan", must.NotFail(types.NewDocument("isCached", cached, "queryPlan", plan)))
	}
	if len(rest) == 0 {
		return
	}
	stages := cursorStage(response, queryPlanner)
	execStats := must.NotFail(stages.Get(0)).(*types.Document).Has("nReturned")
	for _, s := range rest {
		entry := s.(*types.Document).DeepCopy()
		if execStats {
			// Each stage after the count reshapes its one document.
			setStageReturned(entry, 1)
		}
		stages.Append(entry)
	}
	response.Set("stages", stages)
}
