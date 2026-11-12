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

	sub.sbePrefix = SBEPrefixLen(sbeSpecs(sub.pipeline))
	sub.sbeSubPipeline = true

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
	ctx = WithJoinStages(ctx, JoinStages{})
	var fromDocs []*types.Document
	if u.sub.from != "" {
		var err error
		if fromDocs, err = fetcher.Fetch(ctx, u.sub.from, nil); err != nil {
			return nil, lazyerrors.Error(err)
		}
	}
	unioned, subCloser, err := u.sub.pipelineIter(ctx, fetcher, fromDocs, types.MakeDocument(0), nil)
	if err != nil {
		return nil, err
	}
	res := &concatIterator{iters: []types.DocumentsIterator{iter, unioned}, closer: subCloser}
	closer.Add(res)
	return res, nil
}

// concatIterator yields the documents of each iterator in turn.
type concatIterator struct {
	iters  []types.DocumentsIterator
	closer *iterator.MultiCloser
}

func (it *concatIterator) Next() (struct{}, *types.Document, error) {
	for len(it.iters) > 0 {
		_, doc, err := it.iters[0].Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			it.iters = it.iters[1:]
			continue
		}
		return struct{}{}, doc, err
	}
	return struct{}{}, nil, iterator.ErrIteratorDone
}

func (it *concatIterator) Close() {
	for _, iter := range it.iters {
		iter.Close()
	}
	it.iters = nil
	it.closer.Close()
}

var _ aggregations.Stage = (*unionWith)(nil)
