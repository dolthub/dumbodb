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
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/common"
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
		sub.from = spec
	case *types.Document:
		for _, key := range spec.Keys() {
			value := must.NotFail(spec.Get(key))
			switch key {
			case "coll":
				coll, ok := value.(string)
				if !ok {
					return nil, unionWithWrongType(key, value, "string")
				}
				sub.from = coll
			case "pipeline":
				pipeline, ok := value.(*types.Array)
				if !ok {
					return nil, unionWithWrongType(key, value, "array")
				}
				sub.pipeline = pipeline
			default:
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIDLUnknownField,
					fmt.Sprintf("BSON field '$unionWith.%s' is an unknown field.", key), "$unionWith (stage)")
			}
		}
		if sub.from == "" && !startsWithDocuments(spec) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
				"$unionWith stage without explicit collection must have a pipeline with $documents as first stage",
				"$unionWith (stage)")
		}
	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"the $unionWith stage specification must be an object or string, but found "+handlerparams.AliasFromType(spec),
			"$unionWith (stage)")
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

	return &unionWith{sub: sub, fetcher: fetcher}, nil
}

func unionWithWrongType(field string, v any, want string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
		fmt.Sprintf("BSON field '$unionWith.%s' is the wrong type '%s', expected type '%s'",
			field, handlerparams.AliasFromType(v), want), "$unionWith (stage)")
}

// Process implements aggregations.Stage.
func (u *unionWith) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	out, err := common.ConsumeDocuments(iter, "$unionWith")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	fetcher := withFetchCache(u.fetcher)
	var fromDocs []*types.Document
	if u.sub.from != "" {
		if fromDocs, err = fetcher.Fetch(ctx, u.sub.from, nil); err != nil {
			return nil, lazyerrors.Error(err)
		}
	}
	unioned, err := u.sub.runPipeline(ctx, fetcher, fromDocs, types.MakeDocument(0))
	if err != nil {
		return nil, err
	}
	// Fetched documents may be shared; later stages may modify these.
	for _, d := range unioned {
		out = append(out, d.DeepCopy())
	}

	result := iterator.Values(iterator.ForSlice(out))
	closer.Add(result)
	return result, nil
}

var _ aggregations.Stage = (*unionWith)(nil)
