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

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// buildViewPipelineStages converts the stage documents of a view's defining
// pipeline into executable stages. $lookup/$graphLookup need a collection
// fetcher; the rest go through stages.NewStage.
func buildViewPipelineStages(db backends.Database, viewPipeline *types.Array) ([]aggregations.Stage, error) {
	if viewPipeline == nil || viewPipeline.Len() == 0 {
		return nil, nil
	}

	stageDocs := must.NotFail(iterator.ConsumeValues(viewPipeline.Iterator()))
	result := make([]aggregations.Stage, 0, len(stageDocs))

	for _, v := range stageDocs {
		vd, ok := v.(*types.Document)
		if !ok {
			return nil, lazyerrors.Errorf("view pipeline stage is not a document: %T", v)
		}

		var vs aggregations.Stage
		var err error

		switch vd.Command() {
		case "$lookup", "$graphLookup":
			fetcher := func(ctx context.Context, collName string) ([]*types.Document, error) {
				fromColl, collErr := db.Collection(collName)
				if collErr != nil {
					return nil, collErr
				}

				qRes, qErr := fromColl.Query(ctx, new(backends.QueryParams))
				if qErr != nil {
					return nil, qErr
				}

				defer qRes.Iter.Close()

				return iterator.ConsumeValues(qRes.Iter)
			}

			if vd.Command() == "$graphLookup" {
				vs, err = stages.NewGraphLookupStage(vd, fetcher)
			} else {
				vs, err = stages.NewLookupStage(vd, fetcher)
			}
		default:
			vs, err = stages.NewStage(vd)
		}

		if err != nil {
			return nil, err
		}

		result = append(result, vs)
	}

	return result, nil
}

// viewSourceIterator returns an iterator over the view's source collection with
// the view's defining pipeline applied. Callers layer their own filter, sort,
// projection, skip and limit on top of the returned iterator, matching how
// MongoDB resolves a read against a view to an aggregation over its source.
func viewSourceIterator(ctx context.Context, db backends.Database, viewOn string, viewPipeline *types.Array, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	srcColl, err := db.Collection(viewOn)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	viewStages, err := buildViewPipelineStages(db, viewPipeline)
	if err != nil {
		return nil, err
	}

	return processStagesDocuments(ctx, closer, &stagesDocumentsParams{srcColl, new(backends.QueryParams), viewStages})
}
