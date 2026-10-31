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

var updatePipelineStages = map[string]struct{}{
	"$addFields":   {},
	"$set":         {},
	"$project":     {},
	"$unset":       {},
	"$replaceRoot": {},
	"$replaceWith": {},
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
		if stage.Len() == 1 {
			name := stage.Command()
			_, known := Stages[name]
			if _, allowed := updatePipelineStages[name]; known && !allowed {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidOptions,
					name+" is not allowed to be used within an update", "update")
			}
		}
		var err error
		if stages[i], err = NewStage(stage); err != nil {
			return nil, err
		}
	}
	return stages, nil
}
