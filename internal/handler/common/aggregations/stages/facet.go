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

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

type rawFacetPipeline struct {
	name        string
	pipelineArr *types.Array
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

	// fetcher reads other collections for $lookup, $graphLookup and
	// $unionWith in the sub-pipelines; nil where none is available.
	fetcher CollectionFetcher
}

// notAllowedInFacet are the stages mongod refuses in a $facet sub-pipeline.
var notAllowedInFacet = map[string]bool{
	"$collStats":         true,
	"$documents":         true,
	"$facet":             true,
	"$geoNear":           true,
	"$indexStats":        true,
	"$merge":             true,
	"$out":               true,
	"$planCacheStats":    true,
	"$queryStats":        true,
	"$changeStream":      true,
	"$listSessions":      true,
	"$currentOp":         true,
	"$listLocalSessions": true,
	"$listCatalog":       true,
	"$querySettings":     true,

	"$changeStreamSplitLargeEvent": true,
}

// CheckFacetQueryStats returns mongod's error for a $queryStats in a $facet of
// {aggregate: 1} on admin, where its namespace is right but its place is not.
// The branches are parsed in order, so an earlier branch's error comes first.
func CheckFacetQueryStats(stage *types.Document) error {
	spec, ok := must.NotFail(stage.Get("$facet")).(*types.Document)
	if !ok {
		return nil
	}
	for _, k := range spec.Keys() {
		pipeline, ok := must.NotFail(spec.Get(k)).(*types.Array)
		if !ok {
			return nil
		}
		q := -1
		for i := range pipeline.Len() {
			if sub, ok := must.NotFail(pipeline.Get(i)).(*types.Document); ok && sub.Command() == "$queryStats" {
				q = i
				break
			}
		}
		before := pipeline
		if q >= 0 {
			before = types.MakeArray(q)
			for i := range q {
				before.Append(must.NotFail(pipeline.Get(i)))
			}
		}
		for i := range before.Len() {
			sub := must.NotFail(before.Get(i))
			branch := must.NotFail(types.NewDocument(k, must.NotFail(types.NewArray(sub))))
			if _, err := newFacet(must.NotFail(types.NewDocument("$facet", branch))); err != nil {
				return err
			}
			if d, ok := sub.(*types.Document); ok {
				if err := parseStageOffline(d); err != nil {
					return err
				}
			}
		}
		if q < 0 {
			continue
		}
		if err := CheckQueryStatsSpec(must.NotFail(pipeline.Get(q)).(*types.Document)); err != nil {
			return err
		}
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrStageNotAllowedInFacet,
			"$queryStats is not allowed to be used within a $facet stage", "$facet (stage)")
	}
	return nil
}

// parseStageOffline parses a stage without reading any collection, for its
// parse errors only.
func parseStageOffline(stage *types.Document) error {
	noFetch := FetcherFunc(func(context.Context, string, *types.Document) ([]*types.Document, error) { return nil, nil })
	var err error
	switch stage.Command() {
	case "$lookup":
		_, err = NewLookupStage(stage.DeepCopy(), noFetch)
	case "$graphLookup":
		_, err = NewGraphLookupStage(stage.DeepCopy(), noFetch)
	case "$unionWith":
		_, err = NewUnionWithStage(stage.DeepCopy(), noFetch)
	default:
		if FailsParsing(stage) {
			_, err = NewStage(stage.DeepCopy())
		}
	}
	return err
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
		if err := aggregations.CheckPipelineLength(pipelineArr.Len()); err != nil {
			return nil, err
		}

		// Validate that each element is a document (basic validation at parse time).
		for i := 0; i < pipelineArr.Len(); i++ {
			elem := must.NotFail(pipelineArr.Get(i))
			stage, ok := elem.(*types.Document)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrStageInvalid,
					"A pipeline stage specification object must contain exactly one field.",
					"$facet (stage)",
				)
			}
			switch stage.Command() {
			case "$queryStats":
				// Its specification and namespace are checked before its
				// place; {aggregate: 1} on admin checks its place itself.
				_, err := newQueryStats(stage)
				return nil, err
			case "$changeStreamSplitLargeEvent":
				if err := CheckChangeStreamSplitLargeEventSpec(stage); err != nil {
					return nil, err
				}
			case "$querySettings":
				if err := CheckQuerySettingsSpec(stage); err != nil {
					return nil, err
				}
			default:
				if err := CheckParseFailingStages(must.NotFail(types.NewArray(stage))); err != nil {
					return nil, err
				}
			}
			if cmd := stage.Command(); notAllowedInFacet[cmd] {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrStageNotAllowedInFacet,
					cmd+" is not allowed to be used within a $facet stage",
					"$facet (stage)",
				)
			}
		}

		pipelines = append(pipelines, rawFacetPipeline{name: field, pipelineArr: pipelineArr})
	}

	return &facet{pipelines: pipelines}, nil
}

// readOnlyStages never change the documents they read.
var readOnlyStages = map[string]bool{
	"$match": true, "$count": true, "$group": true, "$sortByCount": true, "$bucket": true,
	"$bucketAuto": true, "$sort": true, "$limit": true, "$skip": true,
}

// readsOnly reports whether every stage of a $facet branch only reads its
// input documents, so the branch can share them.
func readsOnly(pipeline *types.Array) bool {
	for i := range pipeline.Len() {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok || stage.Len() != 1 || !readOnlyStages[stage.Command()] {
			return false
		}
	}
	return true
}

// parsePipelineArray parses an array of stage documents into a list of Stage instances.
// This is a standalone function that calls NewStage; it must only be called from
// Process() methods (not from newXxx constructors) to avoid initialization cycles.
func parsePipelineArray(arr *types.Array, fetcher CollectionFetcher) ([]aggregations.Stage, error) {
	var stageList []aggregations.Stage
	var specs []*types.Document
	subStages := must.NotFail(iterator.ConsumeValues(arr.Iterator()))

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

		s, err := newSubPipelineStage(stageDoc, fetcher, SeesStoredDocuments(subStages, i))
		if err != nil {
			return nil, err
		}

		stageList = append(stageList, s)
		specs = append(specs, stageDoc)
	}
	MarkLookupsUnwound(stageList, specs)

	return stageList, nil
}

// newSubPipelineStage builds one stage of a $facet, $lookup or $unionWith
// sub-pipeline, giving fetcher to the stages that read other collections.
func newSubPipelineStage(stage *types.Document, fetcher CollectionFetcher, seesStoredDocuments bool) (aggregations.Stage, error) {
	cmd := stage.Command()
	if cmd == "$match" {
		return NewMatchStage(stage, nil, seesStoredDocuments)
	}
	if fetcher != nil {
		switch cmd {
		case "$lookup":
			return NewLookupStage(stage, fetcher)
		case "$graphLookup":
			return NewGraphLookupStage(stage, fetcher)
		case "$unionWith":
			return NewUnionWithStage(stage, fetcher)
		case "$facet":
			return NewFacetStage(stage, fetcher)
		}
	}
	return NewStage(stage)
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

	result, err := common.ConsumeDocuments(iter, "$facet")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return result, nil
}

func (f *facet) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	// Collect all input documents  -- each sub-pipeline gets the same set.
	inputDocs, err := common.ConsumeDocuments(iter, "$facet")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}
	// mongod's sub-pipelines see no RecordId.
	for _, doc := range inputDocs {
		doc.SetRecordKey(nil)
	}

	// Build the output document.
	outputDoc := new(types.Document)

	for i, p := range f.pipelines {
		// Parse stages at process time to avoid initialization cycle.
		stageList, parseErr := parsePipelineArray(p.pipelineArr, f.fetcher)
		if parseErr != nil {
			return nil, parseErr
		}

		// Stages may change documents in place; each branch but the last
		// works on its own copy so the others see the input unchanged.
		branchDocs := inputDocs
		if i < len(f.pipelines)-1 && !readsOnly(p.pipelineArr) {
			branchDocs = make([]*types.Document, len(inputDocs))
			for j, doc := range inputDocs {
				branchDocs[j] = doc.DeepCopy()
			}
		}

		results, err := runSubPipeline(ctx, branchDocs, stageList)
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

// NewFacetStage is $facet whose sub-pipelines can read other collections
// through fetcher.
func NewFacetStage(stage *types.Document, fetcher CollectionFetcher) (aggregations.Stage, error) {
	s, err := newFacet(stage)
	if err != nil {
		return nil, err
	}
	f := s.(*facet)
	f.fetcher = fetcher
	for _, p := range f.pipelines {
		if err := checkSubPipelineParses(p.pipelineArr, fetcher); err != nil {
			return nil, err
		}
	}
	return f, nil
}
