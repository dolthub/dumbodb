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
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
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
	if err := LiteParsePipeline(pipeline); err != nil {
		return nil, &common.MalformedPipelineError{Err: err}
	}
	stages := make([]aggregations.Stage, pipeline.Len())
	for i := range stages {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
				"Each element of the 'pipeline' array must be an object", "update")
		}
		if err := checkUpdatePipelineParse(stage, i); err != nil {
			return nil, err
		}
		newStage := NewStage
		if stage.Len() == 1 {
			name := stage.Command()
			known := IsKnownStage(name)
			allowed, isAllowed := updatePipelineStages[name]
			if known && !isAllowed {
				// mongod parses the stage before it refuses it here.
				if _, alwaysFails := parseFailingStages[name]; !alwaysFails && FailsParsing(stage) {
					if _, err := stageConstructors[name](stage.DeepCopy()); err != nil {
						return nil, err
					}
				}
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

// checkUpdatePipelineParse returns the error mongod reports while it parses an
// update pipeline stage, before it checks that the stage is allowed there.
func checkUpdatePipelineParse(stage *types.Document, position int) error {
	if stage.Len() != 1 {
		return nil
	}
	switch name := stage.Command(); name {
	case "$changeStream", "$currentOp", "$listSessions", "$queryStats", "$shardedDataDistribution",
		"$documents", "$listLocalSessions", "$changeStreamSplitLargeEvent":
		_, err := NewStage(stage)
		return err
	case "$setVariableFromSubPipeline":
		return checkSetVariableFromSubPipelineSpec(stage)
	case "$search", "$vectorSearch":
		if position > 0 {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCollStatsIsNotFirstStage,
				name+" is only valid as the first stage in a pipeline", name)
		}
	case "$searchMeta":
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSearchNotAllowedInContext,
			"Running search command in non-allowed context (update pipeline)", "update")
	}
	return nil
}

// checkSetVariableFromSubPipelineSpec returns the error mongod's IDL parser
// gives for a $setVariableFromSubPipeline specification.
func checkSetVariableFromSubPipelineSpec(stage *types.Document) error {
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$setVariableFromSubPipeline (stage)")
	}
	spec, ok := must.NotFail(stage.Get("$setVariableFromSubPipeline")).(*types.Document)
	if !ok {
		return nil
	}
	for _, key := range spec.Keys() {
		v := must.NotFail(spec.Get(key))
		want := map[string]string{"setVariable": "string", "pipeline": "array"}[key]
		switch {
		case want == "":
			return fail(handlererrors.ErrIDLUnknownField, "BSON field '$setVariableFromSubPipeline."+key+"' is an unknown field.")
		case handlerparams.AliasFromType(v) != want:
			return fail(handlererrors.ErrTypeMismatch, fmt.Sprintf("BSON field '$setVariableFromSubPipeline.%s' is the wrong type '%s', expected type '%s'",
				key, handlerparams.AliasFromType(v), want))
		}
	}
	for _, key := range []string{"pipeline", "setVariable"} {
		if !spec.Has(key) {
			return fail(handlererrors.ErrMissingField, "BSON field '$setVariableFromSubPipeline."+key+"' is missing but a required field")
		}
	}
	return nil
}
