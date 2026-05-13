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
	"context"
	"errors"
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

type rawFacetPipeline struct {
	name         string
	pipelineArr  *types.Array
}

// facet represents $facet stage.
//
//	{ $facet: { <field1>: [ <stage1>, ... ], <field2>: [ <stage1>, ... ], ... } }
//
// $facet allows multiple sub-pipelines to be run on the same set of input documents.
// The output is a single document where each field contains the results of one sub-pipeline.
//
// Stage parsing is deferred to Process time to avoid circular initialization with NewStage.
type facet struct {
	pipelines []rawFacetPipeline
}

// newFacet creates a new $facet stage.
//
// Note: stage parsing (calling NewStage) is deferred to Process() to avoid the
// initialization cycle: Stages -> newFacet -> parsePipeline -> NewStage -> Stages.
func newFacet(stage *types.Document) (aggregations.Stage, error) {
	v, err := stage.Get("$facet")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	spec, ok := v.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"Arguments to $facet must be arrays",
			"$facet (stage)",
		)
	}

	if spec.Len() == 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$facet requires at least one sub-pipeline",
			"$facet (stage)",
		)
	}

	iter := spec.Iterator()
	defer iter.Close()

	var pipelines []rawFacetPipeline

	for {
		field, pv, iterErr := iter.Next()
		if errors.Is(iterErr, iterator.ErrIteratorDone) {
			break
		}

		if iterErr != nil {
			return nil, lazyerrors.Error(iterErr)
		}

		pipelineArr, ok := pv.(*types.Array)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("Arguments to $facet must be arrays, field '%s' is not", field),
				"$facet (stage)",
			)
		}

		// Validate that each element is a document (basic validation at parse time).
		for i := 0; i < pipelineArr.Len(); i++ {
			elem := must.NotFail(pipelineArr.Get(i))
			if _, ok := elem.(*types.Document); !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrStageInvalid,
					"A pipeline stage specification object must contain exactly one field.",
					"$facet (stage)",
				)
			}
		}

		pipelines = append(pipelines, rawFacetPipeline{name: field, pipelineArr: pipelineArr})
	}

	return &facet{pipelines: pipelines}, nil
}

// parsePipelineArray parses an array of stage documents into a list of Stage instances.
// This is a standalone function that calls NewStage; it must only be called from
// Process() methods (not from newXxx constructors) to avoid initialization cycles.
func parsePipelineArray(arr *types.Array) ([]aggregations.Stage, error) {
	var stageList []aggregations.Stage

	for i := 0; i < arr.Len(); i++ {
		v := must.NotFail(arr.Get(i))

		stageDoc, ok := v.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrStageInvalid,
				"A pipeline stage specification object must contain exactly one field.",
				"$facet (stage)",
			)
		}

		s, err := NewStage(stageDoc)
		if err != nil {
			return nil, err
		}

		stageList = append(stageList, s)
	}

	return stageList, nil
}

// runSubPipeline runs a set of stages on the given input documents and returns the result.
func runSubPipeline(ctx context.Context, inputDocs []*types.Document, stages []aggregations.Stage) ([]*types.Document, error) {
	closer := iterator.NewMultiCloser()
	defer closer.Close()

	var iter types.DocumentsIterator = iterator.Values(iterator.ForSlice(inputDocs))
	closer.Add(iter)

	var err error

	for _, s := range stages {
		iter, err = s.Process(ctx, iter, closer)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
	}

	result, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return result, nil
}

func (f *facet) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	// Collect all input documents  -- each sub-pipeline gets the same set.
	inputDocs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Build the output document.
	outputDoc := new(types.Document)

	for _, p := range f.pipelines {
		// Parse stages at process time to avoid initialization cycle.
		stageList, parseErr := parsePipelineArray(p.pipelineArr)
		if parseErr != nil {
			return nil, parseErr
		}

		results, err := runSubPipeline(ctx, inputDocs, stageList)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		arr := types.MakeArray(len(results))
		for _, doc := range results {
			arr.Append(doc)
		}

		outputDoc.Set(p.name, arr)
	}

	result := iterator.Values(iterator.ForSlice([]*types.Document{outputDoc}))
	closer.Add(result)

	return result, nil
}

var (
	_ aggregations.Stage = (*facet)(nil)
)
