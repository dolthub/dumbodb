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
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// updatePipelineStages build the stages an update pipeline allows. mongod
// does not fold constant subexpressions there.
var updatePipelineStages = map[string]newStageFunc{
	"$addFields":   func(stage *types.Document) (aggregations.Stage, error) { return newAddFieldsStage(stage, false) },
	"$set":         func(stage *types.Document) (aggregations.Stage, error) { return newSetStage(stage, false) },
	"$project":     func(stage *types.Document) (aggregations.Stage, error) { return newProjectStage(stage, false) },
	"$unset":       newUnset,
	"$replaceRoot": func(stage *types.Document) (aggregations.Stage, error) { return newReplaceRootStage(stage, false) },
	"$replaceWith": func(stage *types.Document) (aggregations.Stage, error) { return newReplaceWithStage(stage, false) },
}

func init() {
	common.RegisterUpdatePipelineParser(newUpdatePipeline)
}

// newUpdatePipeline parses the stages of a pipeline-style update as the
// aggregation stages they are.
func newUpdatePipeline(pipeline *types.Array) ([]aggregations.Stage, error) {
	stages := make([]aggregations.Stage, pipeline.Len())
	for i := range stages {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
				"Each element of the 'pipeline' array must be an object", "update")
		}
		newStage := NewStage
		if stage.Len() == 1 {
			name := stage.Command()
			_, known := Stages[name]
			allowed, isAllowed := updatePipelineStages[name]
			if known && !isAllowed {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidOptions,
					name+" is not allowed to be used within an update", "update")
			}
			if isAllowed {
				newStage = allowed
			}
		}
		var err error
		if stages[i], err = newStage(stage); err != nil {
			return nil, err
		}
	}
	return stages, nil
}
