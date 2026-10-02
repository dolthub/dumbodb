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
	"github.com/dolthub/dumbodb/internal/collation"
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
// analysis needs in their original form. A non-nil cmp is the view's collation;
// $match and $sort compare strings under it.
func buildViewPipelineStages(db backends.Database, viewPipeline *types.Array, cmp *collation.Comparator) ([]aggregations.Stage, []any, error) {
	if viewPipeline == nil || viewPipeline.Len() == 0 {
		return nil, nil, nil
	}

	stageDocs := must.NotFail(iterator.ConsumeValues(viewPipeline.Iterator()))
	result := make([]aggregations.Stage, 0, len(stageDocs))

	for i, v := range stageDocs {
		vd, ok := v.(*types.Document)
		if !ok {
			return nil, nil, lazyerrors.Errorf("view pipeline stage is not a document: %T", v)
		}

		var vs aggregations.Stage
		var err error

		switch vd.Command() {
		case "$lookup", "$graphLookup":
			fetcher := foreignCollectionFetcher(db)

			if vd.Command() == "$graphLookup" {
				vs, err = stages.NewGraphLookupStage(vd, fetcher)
			} else {
				vs, err = stages.NewLookupStage(vd, fetcher)
			}
		case "$match":
			vs, err = stages.NewMatchStage(vd, cmp)
		case "$sort":
			vs, err = stages.NewSortStage(vd, sortLimitBound(stageDocs, i), cmp)
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

type foreignViewDepthKey struct{}

// foreignCollectionFetcher returns the documents of a $lookup/$graphLookup
// "from" namespace. A view is resolved to its output under its own collation.
// Nesting through foreign views is bounded by the view depth limit, so a view
// that looks up from itself fails instead of recursing forever.
func foreignCollectionFetcher(db backends.Database) stages.CollectionFetcher {
	return func(ctx context.Context, collName string) ([]*types.Document, error) {
		info, err := lookupCollectionInfo(ctx, db, collName)
		if err != nil {
			return nil, err
		}

		if info != nil && info.IsView {
			depth, _ := ctx.Value(foreignViewDepthKey{}).(int)
			if depth >= maxViewDepth {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrViewDepthLimitExceeded,
					fmt.Sprintf("View depth limit exceeded; maximum depth is %d", maxViewDepth),
					"pipeline",
				)
			}
			ctx = context.WithValue(ctx, foreignViewDepthKey{}, depth+1)

			closer := iterator.NewMultiCloser()
			defer closer.Close()

			cmp := collation.Parse(info.Collation).Comparator()
			iter, err := viewSourceIterator(ctx, db, info.Name, info.ViewOn, info.ViewPipeline, cmp, closer, false, false)
			if err != nil {
				return nil, err
			}
			defer iter.Close()

			return iterator.ConsumeValues(iter)
		}

		fromColl, err := db.Collection(collName)
		if err != nil {
			return nil, err
		}

		qRes, err := fromColl.Query(ctx, new(backends.QueryParams))
		if err != nil {
			return nil, err
		}
		defer qRes.Iter.Close()

		return iterator.ConsumeValues(qRes.Iter)
	}
}

// viewReadCollation returns the collation a read on a view runs under: the
// view's own. MongoDB rejects an operation collation that differs from it.
func viewReadCollation(opCollation, viewCollation *types.Document) (*types.Document, error) {
	if opCollation != nil && !sameCollation(opCollation, viewCollation) {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOptionNotSupportedOnView,
			"Cannot override a view's default collation",
		)
	}
	return viewCollation, nil
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
func resolveViewChain(ctx context.Context, db backends.Database, viewName, viewOn string, viewPipeline *types.Array, cmp *collation.Comparator) (string, []aggregations.Stage, []any, error) {
	viewStages, rawStages, err := buildViewPipelineStages(db, viewPipeline, cmp)
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

		innerStages, innerRaw, err := buildViewPipelineStages(db, srcInfo.ViewPipeline, cmp)
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

// validateViewDependenciesAcyclic rejects a view definition that depends on
// itself through viewOn or any $lookup, $graphLookup, or $unionWith reference,
// including references inside nested pipelines and $facet, matching MongoDB's
// GraphContainsCycle check at create and collMod time.
func validateViewDependenciesAcyclic(ctx context.Context, db backends.Database, viewName, viewOn string, pipeline *types.Array, command string) error {
	visited := map[string]struct{}{}
	pending := append([]string{viewOn}, pipelineForeignNamespaces(pipeline)...)

	for len(pending) > 0 {
		name := pending[len(pending)-1]
		pending = pending[:len(pending)-1]

		if name == viewName {
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrGraphContainsCycle,
				fmt.Sprintf("View cycle detected: %s", viewName),
				command,
			)
		}
		if _, seen := visited[name]; seen {
			continue
		}
		visited[name] = struct{}{}

		info, err := lookupCollectionInfo(ctx, db, name)
		if err != nil {
			return err
		}
		if info != nil && info.IsView {
			pending = append(pending, info.ViewOn)
			pending = append(pending, pipelineForeignNamespaces(info.ViewPipeline)...)
		}
	}
	return nil
}

// pipelineForeignNamespaces returns the collection or view names a pipeline
// reads besides its source.
func pipelineForeignNamespaces(pipeline *types.Array) []string {
	if pipeline == nil {
		return nil
	}

	var names []string
	for i := 0; i < pipeline.Len(); i++ {
		stage, _ := must.NotFail(pipeline.Get(i)).(*types.Document)
		if stage == nil || stage.Len() == 0 {
			continue
		}
		spec := must.NotFail(stage.Get(stage.Command()))

		switch stage.Command() {
		case "$lookup", "$graphLookup":
			specDoc, _ := spec.(*types.Document)
			if specDoc == nil {
				continue
			}
			if from, _ := specDoc.Get("from"); from != nil {
				if fromName, ok := from.(string); ok {
					names = append(names, fromName)
				}
			}
			if nested, _ := specDoc.Get("pipeline"); nested != nil {
				nestedArr, _ := nested.(*types.Array)
				names = append(names, pipelineForeignNamespaces(nestedArr)...)
			}
		case "$unionWith":
			switch u := spec.(type) {
			case string:
				names = append(names, u)
			case *types.Document:
				if coll, _ := u.Get("coll"); coll != nil {
					if collName, ok := coll.(string); ok {
						names = append(names, collName)
					}
				}
				if nested, _ := u.Get("pipeline"); nested != nil {
					nestedArr, _ := nested.(*types.Array)
					names = append(names, pipelineForeignNamespaces(nestedArr)...)
				}
			}
		case "$facet":
			facets, _ := spec.(*types.Document)
			if facets == nil {
				continue
			}
			for _, key := range facets.Keys() {
				sub, _ := must.NotFail(facets.Get(key)).(*types.Array)
				names = append(names, pipelineForeignNamespaces(sub)...)
			}
		}
	}
	return names
}

// viewSourceIterator returns an iterator over the view's base collection with
// the view's (fully resolved, possibly nested) defining pipeline applied.
// Callers layer their own filter, sort, projection, skip and limit on top of
// the returned iterator, matching how MongoDB resolves a read against a view to
// an aggregation over its source.
func viewSourceIterator(ctx context.Context, db backends.Database, viewName, viewOn string, viewPipeline *types.Array, cmp *collation.Comparator, closer *iterator.MultiCloser, disablePushdown, enableNestedPushdown bool) (types.DocumentsIterator, error) { //nolint:lll // for readability
	baseCollection, viewStages, rawStages, err := resolveViewChain(ctx, db, viewName, viewOn, viewPipeline, cmp)
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
	qp := &backends.QueryParams{Collated: cmp != nil}
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
