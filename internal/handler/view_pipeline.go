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
	"fmt"
	"strings"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// maxViewDepth is MongoDB's maximum view-resolution nesting depth. A read that
// resolves through more than this many views errors with ViewDepthLimitExceeded.
const maxViewDepth = 20

// buildViewPipelineStages compiles a view's pipeline into executable stages
// ($lookup/$graphLookup get a collection fetcher; the rest go through
// stages.NewStage) and also returns the raw stage documents, which the pushdown
// analysis needs in their original form.
func buildViewPipelineStages(db backends.Database, viewPipeline *types.Array) ([]aggregations.Stage, []any, error) {
	if viewPipeline == nil || viewPipeline.Len() == 0 {
		return nil, nil, nil
	}

	stageDocs := must.NotFail(iterator.ConsumeValues(viewPipeline.Iterator()))
	result := make([]aggregations.Stage, 0, len(stageDocs))

	for _, v := range stageDocs {
		vd, ok := v.(*types.Document)
		if !ok {
			return nil, nil, lazyerrors.Errorf("view pipeline stage is not a document: %T", v)
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
			return nil, nil, err
		}

		result = append(result, vs)
	}

	return result, stageDocs, nil
}

// lookupCollectionInfo returns the CollectionInfo for name, or nil if no such
// namespace exists.
func lookupCollectionInfo(ctx context.Context, db backends.Database, name string) (*backends.CollectionInfo, error) {
	res, err := db.ListCollections(ctx, &backends.ListCollectionsParams{Name: name})
	if err != nil {
		return nil, err
	}
	if res == nil || len(res.Collections) == 0 {
		return nil, nil
	}
	return &res.Collections[0], nil
}

// resolveViewChain flattens a view -- whose source may itself be a view -- into
// its base collection plus the resolved pipeline, returned as both compiled
// stages and raw stage documents in resolved order (inner views prepended).
// viewName seeds cycle detection; enforces GraphContainsCycle and the maximum
// nesting depth (ViewDepthLimitExceeded), matching MongoDB.
func resolveViewChain(ctx context.Context, db backends.Database, viewName, viewOn string, viewPipeline *types.Array) (string, []aggregations.Stage, []any, error) {
	viewStages, rawStages, err := buildViewPipelineStages(db, viewPipeline)
	if err != nil {
		return "", nil, nil, err
	}

	seen := map[string]struct{}{viewName: {}}
	source := viewOn
	depth := 1

	for {
		srcInfo, err := lookupCollectionInfo(ctx, db, source)
		if err != nil {
			return "", nil, nil, err
		}
		if srcInfo == nil || !srcInfo.IsView {
			return source, viewStages, rawStages, nil
		}

		if _, cycle := seen[source]; cycle {
			return "", nil, nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrGraphContainsCycle,
				fmt.Sprintf("View cycle detected: %s", source),
				"pipeline",
			)
		}
		depth++
		if depth > maxViewDepth {
			return "", nil, nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrViewDepthLimitExceeded,
				fmt.Sprintf("View depth limit exceeded; maximum depth is %d", maxViewDepth),
				"pipeline",
			)
		}
		seen[source] = struct{}{}

		innerStages, innerRaw, err := buildViewPipelineStages(db, srcInfo.ViewPipeline)
		if err != nil {
			return "", nil, nil, err
		}
		viewStages = append(innerStages, viewStages...)
		rawStages = append(innerRaw, rawStages...)
		source = srcInfo.ViewOn
	}
}

// validateViewChainAcyclic checks that a new view named viewName with source
// viewOn would not form a cycle or exceed the maximum nesting depth, matching
// MongoDB's create-time validation (GraphContainsCycle / ViewDepthLimitExceeded).
// The new view need not yet exist in the catalog: viewName seeds the walk so a
// source chain that loops back to it is detected. It does not build stages.
func validateViewChainAcyclic(ctx context.Context, db backends.Database, viewName, viewOn string) error {
	seen := map[string]struct{}{viewName: {}}
	source := viewOn
	depth := 1

	for {
		if _, cycle := seen[source]; cycle {
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrGraphContainsCycle,
				fmt.Sprintf("View cycle detected: %s", source),
				"create",
			)
		}

		srcInfo, err := lookupCollectionInfo(ctx, db, source)
		if err != nil {
			return err
		}
		if srcInfo == nil || !srcInfo.IsView {
			return nil
		}

		depth++
		if depth > maxViewDepth {
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrViewDepthLimitExceeded,
				fmt.Sprintf("View depth limit exceeded; maximum depth is %d", maxViewDepth),
				"create",
			)
		}
		seen[source] = struct{}{}
		source = srcInfo.ViewOn
	}
}

// viewSourceIterator returns an iterator over the view's base collection with
// the view's (fully resolved, possibly nested) defining pipeline applied.
// Callers layer their own filter, sort, projection, skip and limit on top of
// the returned iterator, matching how MongoDB resolves a read against a view to
// an aggregation over its source.
func viewSourceIterator(ctx context.Context, db backends.Database, viewName, viewOn string, viewPipeline *types.Array, closer *iterator.MultiCloser, disablePushdown, enableNestedPushdown bool) (types.DocumentsIterator, error) { //nolint:lll // for readability
	baseCollection, viewStages, rawStages, err := resolveViewChain(ctx, db, viewName, viewOn, viewPipeline)
	if err != nil {
		return nil, err
	}

	srcColl, err := db.Collection(baseCollection)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Push the resolved view pipeline's leading $match to the base collection so
	// a base-collection index can be used (IXSCAN) instead of scanning every
	// document and replaying the $match in memory. GetPushdownQuery only pushes a
	// genuine leading $match/$sort against base fields; the flags mirror the
	// direct-aggregate path exactly. The user's own find filter is layered on the
	// returned iterator by the caller and is NOT pushed here (view stages may
	// rename or compute fields, making that unsound).
	qp := new(backends.QueryParams)
	filter, _ := aggregations.GetPushdownQuery(rawStages)

	if !disablePushdown {
		qp.Filter = filter
	}

	if !enableNestedPushdown && filter != nil {
		qp.Filter = filter.DeepCopy()

		for _, k := range qp.Filter.Keys() {
			if !strings.ContainsRune(k, '.') {
				continue
			}

			qp.Filter.Remove(k)
		}
	}

	return processStagesDocuments(ctx, closer, &stagesDocumentsParams{srcColl, qp, viewStages})
}
