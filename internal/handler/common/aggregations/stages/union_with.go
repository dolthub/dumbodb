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
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// unionWith represents $unionWith: the input documents, then those of coll
// (or of a leading $documents) through pipeline.
type unionWith struct {
	sub     *lookup
	fetcher CollectionFetcher
}

// NewUnionWithStage parses { $unionWith: <coll> } or
// { $unionWith: { coll: <coll>, pipeline: [...] } }.
func NewUnionWithStage(stage *types.Document, fetcher CollectionFetcher) (aggregations.Stage, error) {
	spec := must.NotFail(stage.Get("$unionWith"))
	sub := &lookup{pipeline: types.MakeArray(0), fetcher: fetcher}

	switch spec := spec.(type) {
	case string:
		if err := checkSubPipelineSource(spec, types.MakeDocument(0), "$unionWith"); err != nil {
			return nil, err
		}
		sub.from = spec
	case *types.Document:
		var fromGiven bool
		for _, key := range spec.Keys() {
			value := must.NotFail(spec.Get(key))
			switch key {
			case "coll":
				coll, ok := value.(string)
				if !ok {
					return nil, unionWithWrongType(key, value, "string")
				}
				sub.from = coll
				fromGiven = true
			case "pipeline":
				pipeline, ok := value.(*types.Array)
				if !ok {
					return nil, unionWithWrongType(key, value, "array")
				}
				if err := aggregations.CheckPipelineLength(pipeline.Len()); err != nil {
					return nil, err
				}
				sub.pipeline = pipeline
			default:
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIDLUnknownField,
					fmt.Sprintf("BSON field '$unionWith.%s' is an unknown field.", key), "$unionWith (stage)")
			}
		}
		if fromGiven {
			if err := checkSubPipelineSource(sub.from, spec, "$unionWith"); err != nil {
				return nil, err
			}
		} else if !startsWithDocuments(spec) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
				"$unionWith stage without explicit collection must have a pipeline with $documents as first stage",
				"$unionWith (stage)")
		}
	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"the $unionWith stage specification must be an object or string, but found "+handlerparams.AliasFromType(spec),
			"$unionWith (stage)")
	}

	if err := CheckParseFailingStages(sub.pipeline); err != nil {
		return nil, err
	}
	if err := checkFirstOnlyStages(sub.pipeline); err != nil {
		return nil, err
	}
	if err := checkSubPipelineParses(sub.pipeline, fetcher); err != nil {
		return nil, err
	}
	for i := range sub.pipeline.Len() {
		d, ok := must.NotFail(sub.pipeline.Get(i)).(*types.Document)
		if !ok || d.Len() == 0 {
			continue
		}
		if name := d.Command(); name == "$out" || name == "$merge" {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrOutInSubPipeline,
				name+" is not allowed within a $unionWith's sub-pipeline", "$unionWith (stage)")
		}
	}

	sub.sbeSubPipeline = true
	sub.cmp = operators.CollationOf(stage)

	return &unionWith{sub: sub, fetcher: fetcher}, nil
}

func unionWithWrongType(field string, v any, want string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
		fmt.Sprintf("BSON field '$unionWith.%s' is the wrong type '%s', expected type '%s'",
			field, handlerparams.AliasFromType(v), want), "$unionWith (stage)")
}

// Process implements aggregations.Stage.
func (u *unionWith) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	fetcher := withFetchCache(u.fetcher)
	if err := checkViewCollation(ctx, fetcher, u.sub.from, u.sub.cmp); err != nil {
		iter.Close()
		return nil, err
	}
	ctx = WithJoinStages(ctx, JoinStages{})

	isView := func(name string) bool {
		_, ok := fetcher.ViewPipeline(ctx, name)
		return ok
	}

	// mongod runs a $unionWith sub-pipeline in SBE, after the stages of the
	// view it reads from, unless it is nested in a $lookup sub-pipeline.
	subSpecs := sbeSpecs(u.sub.pipeline)
	var viewSpecs []*types.Document
	if view, ok := fetcher.ViewPipeline(ctx, u.sub.from); ok && u.sub.from != "" {
		viewSpecs = sbeSpecs(view)
	}
	viewInSBE := false
	// A missing collection gives mongod an empty plan and a leading _id
	// equality its express plan; neither runs in SBE.
	combinedSpecs := append(append([]*types.Document{}, viewSpecs...), subSpecs...)
	noSBE := IsExpressMatch(combinedSpecs) || (u.sub.from != "" && !fetcher.SourceExists(ctx, u.sub.from))
	if u.sub.inClassic || noSBE {
		u.sub.sbePrefix, u.sub.sbeSubPipeline = 0, false
	} else {
		combined := SBEPrefixLen(append(append([]*types.Document{}, viewSpecs...), subSpecs...), isView)
		u.sub.sbePrefix = max(0, combined-len(viewSpecs))
		viewInSBE = len(viewSpecs) > 0 && combined > 0
		u.sub.viewIsView = isView
	}

	// The sub-pipeline starts once the input is exhausted, as in mongod, so
	// an error reading the input comes before any from the sub-pipeline.
	start := func() (types.DocumentsIterator, *iterator.MultiCloser, error) {
		var fromDocs []*types.Document
		if u.sub.from != "" {
			var err error
			if fromDocs, err = fetcher.Fetch(ctx, u.sub.from, nil); err != nil {
				if viewInSBE {
					return nil, nil, sbeError(err)
				}
				return nil, nil, lazyerrors.Error(err)
			}
		}
		return u.sub.pipelineIter(ctx, fetcher, fromDocs, types.MakeDocument(0), nil)
	}
	res := &unionIterator{input: iter, start: start}
	closer.Add(res)
	return res, nil
}

// unionIterator returns its input's documents, then those of a sub-pipeline
// it starts when the input is exhausted.
type unionIterator struct {
	input    types.DocumentsIterator
	start    func() (types.DocumentsIterator, *iterator.MultiCloser, error)
	unioned  types.DocumentsIterator
	closer   *iterator.MultiCloser
	finished bool
}

func (it *unionIterator) Next() (struct{}, *types.Document, error) {
	var unused struct{}
	if it.finished {
		return unused, nil, iterator.ErrIteratorDone
	}
	if it.unioned == nil {
		_, doc, err := it.input.Next()
		if !errors.Is(err, iterator.ErrIteratorDone) {
			return unused, doc, err
		}
		if it.unioned, it.closer, err = it.start(); err != nil {
			it.finished = true
			return unused, nil, err
		}
	}
	_, doc, err := it.unioned.Next()
	if errors.Is(err, iterator.ErrIteratorDone) {
		it.finished = true
	}
	return unused, doc, err
}

func (it *unionIterator) Close() {
	it.input.Close()
	if it.unioned != nil {
		it.unioned.Close()
	}
	if it.closer != nil {
		it.closer.Close()
	}
}

var _ aggregations.Stage = (*unionWith)(nil)
