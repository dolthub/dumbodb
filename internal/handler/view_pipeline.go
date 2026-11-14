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
	"errors"
	"fmt"
	"strings"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
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

	if cmp != nil {
		// The stored pipeline may serve other commands concurrently; the
		// collation is attached to this command's copy.
		viewPipeline = viewPipeline.DeepCopy()
	}
	stageDocs := must.NotFail(iterator.ConsumeValues(viewPipeline.Iterator()))
	result := make([]aggregations.Stage, 0, len(stageDocs))
	for _, v := range stageDocs {
		operators.BindCollation(v, cmp)
	}

	for i, v := range stageDocs {
		vd, ok := v.(*types.Document)
		if !ok {
			return nil, nil, lazyerrors.Errorf("view pipeline stage is not a document: %T", v)
		}

		var vs aggregations.Stage
		var err error

		switch vd.Command() {
		case "$lookup", "$graphLookup", "$unionWith", "$facet":
			vs, err = newForeignCollectionStage(vd, db)
		case "$match":
			vs, err = stages.NewMatchStage(vd, cmp, stages.SeesStoredDocuments(stageDocs, i))
		case "$sort":
			vs, err = stages.NewSortStage(vd, sortLimitBound(stageDocs, i), cmp)
		case "$group":
			vs, err = stages.NewGroupStage(vd, cmp)
		default:
			vs, err = stages.NewStage(vd)
		}

		if err != nil {
			return nil, nil, err
		}

		result = append(result, vs)
	}
	specs := make([]*types.Document, len(stageDocs))
	for i, v := range stageDocs {
		specs[i] = v.(*types.Document)
	}
	stages.MarkLookupsUnwound(result, specs)

	return result, stageDocs, nil
}

// validateViewPipeline returns mongod's error for a view definition whose
// pipeline cannot be parsed or writes to disk.
func validateViewPipeline(dbName, viewName string, pipeline *types.Array) error {
	if pipeline == nil {
		return nil
	}
	prefix := fmt.Sprintf("Invalid pipeline for view %s.%s :: caused by :: ", dbName, viewName)
	// mongod reports the lite parse's errors without the view prefix.
	if err := stages.LiteParsePipeline(pipeline); err != nil {
		return err
	}
	if err := stages.CheckViewPipelineStages(pipeline); err != nil {
		var cerr *handlererrors.CommandError
		if errors.As(err, &cerr) {
			return cerr.WithMessage(prefix + cerr.Err().Error())
		}
		return err
	}
	// Every stage parses, sub-pipelines included, before any is refused
	// for the view. A view may hold Atlas Search stages, which parse without
	// mongot, so they are left out of the stages built here.
	withPrefix := func(err error) error {
		var cerr *handlererrors.CommandError
		if errors.As(err, &cerr) && !strings.HasPrefix(cerr.Err().Error(), prefix) {
			return cerr.WithMessage(prefix + cerr.Err().Error())
		}
		return err
	}
	noFetch := stages.FetcherFunc(func(context.Context, string, *types.Document) ([]*types.Document, error) { return nil, nil })
	for i := range pipeline.Len() {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok || stage.Len() != 1 {
			continue
		}
		var err error
		switch name := stage.Command(); {
		case isViewSearchStage(name):
			err = checkSearchSpec(stage)
		case name == "$documents", name == "$listLocalSessions",
			name == "$listClusterCatalog", name == "$querySettings":
		case name == "$currentOp":
			err = stages.CheckCurrentOpSpec(stage)
		case name == "$lookup":
			var lookup *types.Document
			if lookup, err = stageWithoutSearch(stage); err == nil {
				_, err = stages.NewLookupStage(lookup, noFetch)
			}
		case name == "$unionWith":
			var unionWith *types.Document
			if unionWith, err = stageWithoutSearch(stage); err == nil {
				_, err = stages.NewUnionWithStage(unionWith, noFetch)
			}
		case name == "$graphLookup":
			_, err = stages.NewGraphLookupStage(stage.DeepCopy(), noFetch)
		case name == "$facet":
			var facet *types.Document
			if facet, err = stageWithoutSearch(stage); err == nil {
				_, err = stages.NewFacetStage(facet, noFetch)
			}
		default:
			if stages.FailsParsing(stage) {
				_, err = stages.NewStage(stage.DeepCopy())
			}
		}
		if err != nil {
			return withPrefix(err)
		}
	}
	for i := range pipeline.Len() {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok || stage.Len() != 1 {
			continue
		}
		if name := stage.Command(); name == "$out" || name == "$merge" {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrOptionNotSupportedOnView,
				fmt.Sprintf("%sThe aggregation stage %s in location %d of the pipeline cannot be used in the view "+
					"definition of %s.%s because it writes to disk", prefix, name, i, dbName, viewName),
				"create")
		}
	}
	if err := checkSearchPlacement(pipeline, false, false, false); err != nil {
		return withPrefix(err)
	}
	// Every stage's namespace is checked before any stage's position.
	for i := range pipeline.Len() {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok || stage.Len() != 1 {
			continue
		}
		var cerr *handlererrors.CommandError
		if errors.As(stages.ViewNamespaceError(stage, i), &cerr) {
			return cerr.WithMessage(prefix + cerr.Err().Error())
		}
	}
	for i := 1; i < pipeline.Len(); i++ {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok || stage.Len() != 1 {
			continue
		}
		if name := stage.Command(); isViewSearchStage(name) || name == "$collStats" || name == "$indexStats" {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCollStatsIsNotFirstStage,
				prefix+name+" is only valid as the first stage in a pipeline", name)
		}
	}
	return nil
}

// isViewSearchStage reports whether name is an Atlas Search stage, which a
// view definition holds without mongot.
func isViewSearchStage(name string) bool {
	return stages.IsSearchStage(name) || name == "$listSearchIndexes"
}

// checkSearchSpec returns mongod's error for an Atlas Search stage whose
// specification does not parse.
func checkSearchSpec(stage *types.Document) error {
	name := stage.Command()
	spec := must.NotFail(stage.Get(name))
	if name == "$listSearchIndexes" {
		return checkListSearchIndexesSpec(spec)
	}
	if _, ok := spec.(*types.Document); ok {
		return nil
	}
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
		fmt.Sprintf("%s value must be an object. Found: %s", name, handlerparams.AliasFromType(spec)), name)
}

// checkListSearchIndexesSpec returns mongod's error for a $listSearchIndexes
// specification that does not parse.
func checkListSearchIndexesSpec(spec any) error {
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$listSearchIndexes (stage)")
	}
	doc, ok := spec.(*types.Document)
	if !ok {
		return fail(handlererrors.ErrFailedToParse,
			"The $listSearchIndexes stage specification must be an object. Found: "+handlerparams.AliasFromType(spec))
	}
	var set int
	for _, key := range doc.Keys() {
		v := must.NotFail(doc.Get(key))
		if key != "id" && key != "name" {
			return fail(handlererrors.ErrIDLUnknownField, "BSON field '$listSearchIndexes."+key+"' is an unknown field.")
		}
		switch v.(type) {
		case types.NullType:
		case string:
			set++
		default:
			return fail(handlererrors.ErrTypeMismatch, fmt.Sprintf(
				"BSON field '$listSearchIndexes.%s' is the wrong type '%s', expected type 'string'", key, handlerparams.AliasFromType(v)))
		}
	}
	if set == 2 {
		return fail(handlererrors.ErrInvalidOptions, "Cannot set both 'name' and 'id' for $listSearchIndexes.")
	}
	return nil
}

// subPipeline is a sub-pipeline of a stage, under its key in the stage's
// specification.
type subPipeline struct {
	key      string
	pipeline *types.Array
}

// subPipelines returns the sub-pipelines of a $lookup, $unionWith or $facet
// stage, in order.
func subPipelines(stage *types.Document) []subPipeline {
	spec, _ := must.NotFail(stage.Get(stage.Command())).(*types.Document)
	if spec == nil {
		return nil
	}
	var keys []string
	switch stage.Command() {
	case "$lookup", "$unionWith":
		keys = []string{"pipeline"}
	case "$facet":
		keys = spec.Keys()
	}
	var out []subPipeline
	for _, k := range keys {
		v, _ := spec.Get(k)
		if p, ok := v.(*types.Array); ok {
			out = append(out, subPipeline{key: k, pipeline: p})
		}
	}
	return out
}

// stageWithoutSearch returns a copy of a $lookup, $unionWith or $facet stage
// whose sub-pipelines, at any depth, leave out their Atlas Search stages, or
// mongod's error for a search stage that does not parse.
func stageWithoutSearch(stage *types.Document) (*types.Document, error) {
	stage = stage.DeepCopy()
	subs := subPipelines(stage)
	if len(subs) == 0 {
		return stage, nil
	}
	spec := must.NotFail(stage.Get(stage.Command())).(*types.Document)
	for _, sp := range subs {
		p := sp.pipeline
		kept := types.MakeArray(p.Len())
		for i := range p.Len() {
			sub := must.NotFail(p.Get(i))
			d, ok := sub.(*types.Document)
			if ok && d.Len() == 1 {
				if isViewSearchStage(d.Command()) {
					if err := checkSearchSpec(d); err != nil {
						return nil, err
					}
					continue
				}
				var err error
				if sub, err = stageWithoutSearch(d); err != nil {
					return nil, err
				}
			}
			kept.Append(sub)
		}
		spec.Set(sp.key, kept)
	}
	return stage, nil
}

// checkSearchPlacement returns mongod's error for an Atlas Search stage a
// parsed view pipeline holds where it may not run: after the first stage,
// in a $facet, or a $vectorSearch in a $lookup. In a $facet, a stage whose
// sub-pipeline holds a search stage is refused too. A search stage after the
// first stage of the view's own pipeline is refused by the caller.
func checkSearchPlacement(pipeline *types.Array, nested, inFacet, inLookup bool) error {
	for i := range pipeline.Len() {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok || stage.Len() != 1 {
			continue
		}
		name := stage.Command()
		if isViewSearchStage(name) {
			switch {
			case inFacet:
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrStageNotAllowedInFacet,
					name+" is not allowed to be used within a $facet stage", "$facet (stage)")
			case name == "$vectorSearch" && inLookup:
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrLookupSubPipelineWrites,
					"$vectorSearch is not allowed within a $lookup's sub-pipeline", "$lookup (stage)")
			case i > 0 && nested:
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCollStatsIsNotFirstStage,
					name+" is only valid as the first stage in a pipeline", name)
			}
			continue
		}
		// A stage's own sub-pipelines are checked before the stage is
		// checked against the pipeline holding it.
		subs := subPipelines(stage)
		for _, sp := range subs {
			if err := checkSearchPlacement(sp.pipeline, true, inFacet || name == "$facet", name == "$lookup"); err != nil {
				return err
			}
		}
		for _, sp := range subs {
			if inFacet && holdsSearch(sp.pipeline, isViewSearchStage) {
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrStageNotAllowedInFacet,
					name+" is not allowed to be used within a $facet stage", "$facet (stage)")
			}
			if inLookup && holdsSearch(sp.pipeline, isVectorSearch) {
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrLookupSubPipelineWrites,
					name+" is not allowed within a $lookup's sub-pipeline", "$lookup (stage)")
			}
		}
	}
	return nil
}

func isVectorSearch(name string) bool { return name == "$vectorSearch" }

// holdsSearch reports whether pipeline holds, at any depth, a stage whose
// name is matched by isSearch.
func holdsSearch(pipeline *types.Array, isSearch func(string) bool) bool {
	for i := range pipeline.Len() {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok || stage.Len() != 1 {
			continue
		}
		if isSearch(stage.Command()) {
			return true
		}
		for _, sp := range subPipelines(stage) {
			if holdsSearch(sp.pipeline, isSearch) {
				return true
			}
		}
	}
	return false
}

type foreignViewDepthKey struct{}

// dbNameKey holds the database a command runs on, which a sub-pipeline's
// $collStats reports.
type dbNameKey struct{}

func withDBName(ctx context.Context, dbName string) context.Context {
	return context.WithValue(ctx, dbNameKey{}, dbName)
}

// SourceExists implements stages.ExistenceChecker.
func (f *foreignFetcher) SourceExists(ctx context.Context, name string) bool {
	info, err := lookupCollectionInfo(ctx, f.db, name)
	if err != nil || info == nil {
		return false
	}
	if !info.IsView {
		return true
	}
	base, _, _, err := resolveViewChain(ctx, f.db, info.Name, info.ViewOn, info.ViewPipeline, nil)
	if err != nil {
		return false
	}
	baseInfo, err := lookupCollectionInfo(ctx, f.db, base)
	return err == nil && baseInfo != nil
}

// CheckViewCollation implements stages.ViewCollationChecker.
func (f *foreignFetcher) CheckViewCollation(ctx context.Context, name string, cmp *collation.Comparator) error {
	info, err := lookupCollectionInfo(ctx, f.db, name)
	if err != nil || info == nil || !info.IsView || collation.Parse(info.Collation).Comparator() == cmp {
		return nil
	}
	dbName, _ := ctx.Value(dbNameKey{}).(string)
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrOptionNotSupportedOnView,
		"Cannot override a view's default collation"+dbName+"."+name, "aggregate")
}

// ViewPipeline implements stages.ViewResolver: the stages of a view and of
// the views it reads from.
func (f *foreignFetcher) ViewPipeline(ctx context.Context, name string) (*types.Array, bool) {
	info, err := lookupCollectionInfo(ctx, f.db, name)
	if err != nil || info == nil || !info.IsView {
		return nil, false
	}
	_, _, raw, err := resolveViewChain(ctx, f.db, info.Name, info.ViewOn, info.ViewPipeline, nil)
	if err != nil {
		return types.MakeArray(0), true
	}
	arr := types.MakeArray(len(raw))
	for _, st := range raw {
		arr.Append(st)
	}
	return arr, true
}

// CollStats implements stages.StatsFetcher.
func (f *foreignFetcher) CollStats(ctx context.Context, collName string, pipeline []aggregations.Stage, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	c, err := f.db.Collection(collName)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}
	dbName, _ := ctx.Value(dbNameKey{}).(string)
	return processStagesStats(ctx, closer, &stagesStatsParams{
		c: c, db: f.db, dbName: dbName, cName: collName, statistics: stages.GetStatistics(pipeline), stages: pipeline,
		subPipeline: true,
	})
}

// newForeignCollectionStage builds a $lookup, $graphLookup, $unionWith or
// $facet stage that reads other collections of db.
func newForeignCollectionStage(stage *types.Document, db backends.Database) (aggregations.Stage, error) {
	fetcher := foreignCollectionFetcher(db)
	switch stage.Command() {
	case "$graphLookup":
		return stages.NewGraphLookupStage(stage, fetcher)
	case "$unionWith":
		return stages.NewUnionWithStage(stage, fetcher)
	case "$facet":
		return stages.NewFacetStage(stage, fetcher)
	default:
		return stages.NewLookupStage(stage, fetcher)
	}
}

// foreignCollectionFetcher reads the "from" namespace of $lookup/$graphLookup
// from db.
func foreignCollectionFetcher(db backends.Database) stages.CollectionFetcher {
	return &foreignFetcher{db: db}
}

type foreignFetcher struct {
	db backends.Database
}

// Fetch returns the documents of collName that match filter, letting the
// backend use an index for it. A view is resolved to its output under its own
// collation. Nesting through foreign views is bounded by the view depth limit,
// so a view that looks up from itself fails instead of recursing forever.
func (f *foreignFetcher) Fetch(ctx context.Context, collName string, filter *types.Document) ([]*types.Document, error) {
	db := f.db
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
		// A $unionWith over a view was checked as the stage was parsed
		// (checkJoinedViewLength); a $lookup also is as it executes.
		js := stages.JoinStagesOf(ctx)
		ctx = stages.WithJoinStages(ctx, stages.JoinStages{})
		check := func(n int) error { return wrapAggregateExecutorError(aggregations.CheckPipelineLength(n + js.N)) }
		if js.ViewLimit {
			check = func(n int) error { return wrapAggregateExecutorError(viewPipelineLength(js.N)(n)) }
		}
		iter, err := viewSourceIterator(ctx, db, info.Name, info.ViewOn, info.ViewPipeline, cmp, closer, false, check)
		if err != nil {
			return nil, err
		}
		defer iter.Close()
		if filter != nil {
			iter = common.FilterIterator(iter, closer, filter)
		}

		return iterator.ConsumeValues(iter)
	}

	fromColl, err := db.Collection(collName)
	if err != nil {
		return nil, err
	}

	qRes, err := fromColl.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		return nil, err
	}
	defer qRes.Iter.Close()
	if filter == nil {
		return iterator.ConsumeValues(qRes.Iter)
	}

	closer := iterator.NewMultiCloser()
	defer closer.Close()
	return iterator.ConsumeValues(common.FilterIterator(qRes.Iter, closer, filter))
}

// IndexedField reports whether collName has a single-field index on field that
// the backend can use for an equality or $in filter without a collation.
func (f *foreignFetcher) IndexedField(ctx context.Context, collName, field string) bool {
	_, ok := f.indexOn(ctx, collName, field)
	return ok
}

// indexOn returns the index IndexedField finds.
func (f *foreignFetcher) indexOn(ctx context.Context, collName, field string) (backends.IndexInfo, bool) {
	info, err := lookupCollectionInfo(ctx, f.db, collName)
	if err != nil || info == nil || info.IsView {
		return backends.IndexInfo{}, false
	}
	coll, err := f.db.Collection(collName)
	if err != nil {
		return backends.IndexInfo{}, false
	}
	res, err := coll.ListIndexes(ctx, new(backends.ListIndexesParams))
	if err != nil {
		return backends.IndexInfo{}, false
	}
	// A single-field index on the join field, else a compound one it leads.
	var led *backends.IndexInfo
	for i, idx := range res.Indexes {
		if len(idx.Key) == 0 || idx.Key[0].Field != field || idx.Lossy ||
			idx.PartialFilterExpression != nil || !collation.Parse(idx.Collation).IsSimple() {
			continue
		}
		if len(idx.Key) == 1 {
			return idx, true
		}
		if led == nil {
			led = &res.Indexes[i]
		}
	}
	if led != nil {
		return *led, true
	}
	return backends.IndexInfo{}, false
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

// checkJoinedViewLength returns mongod's error for a $unionWith, or a $lookup
// with only a pipeline, over a view whose stages and the stage's sub-pipeline
// are together over the limit, which mongod refuses as it parses the stage.
func checkJoinedViewLength(ctx context.Context, db backends.Database, stage *types.Document) error {
	spec, _ := stage.Get(stage.Command())
	var from string
	var pipeline *types.Array
	switch stage.Command() {
	case "$unionWith":
		switch spec := spec.(type) {
		case string:
			from = spec
		case *types.Document:
			from, pipeline = joinSpecFields(spec, "coll")
		}
	case "$lookup":
		specDoc, ok := spec.(*types.Document)
		if !ok || specDoc.Has("localField") {
			return nil
		}
		from, pipeline = joinSpecFields(specDoc, "from")
	}
	if from == "" {
		return nil
	}
	info, err := lookupCollectionInfo(ctx, db, from)
	if err != nil || info == nil || !info.IsView {
		return nil
	}
	_, _, rawStages, err := resolveViewChain(ctx, db, info.Name, info.ViewOn, info.ViewPipeline, nil)
	if err != nil {
		return nil
	}
	pipelineLen := 0
	if pipeline != nil {
		pipelineLen = pipeline.Len()
	}
	return aggregations.CheckPipelineLength(len(rawStages) + pipelineLen)
}

// joinSpecFields returns a join spec's namespace, under fromField, and
// pipeline, each zero when absent or of the wrong type.
func joinSpecFields(spec *types.Document, fromField string) (string, *types.Array) {
	v, _ := spec.Get(fromField)
	from, _ := v.(string)
	v, _ = spec.Get("pipeline")
	pipeline, _ := v.(*types.Array)
	return from, pipeline
}

// viewPipelineLength returns a check of a resolved view's stages for a
// command that adds extra stages of its own: mongod refuses a view over the
// limit with ErrPipelineTooLong, and one the command takes over it with
// ErrViewPipelineTooLong.
func viewPipelineLength(extra int) func(viewStages int) error {
	return func(viewStages int) error {
		if err := aggregations.CheckPipelineLength(viewStages); err != nil {
			return err
		}
		if viewStages+extra > aggregations.MaxPipelineLength {
			return handlererrors.NewCommandErrorMsg(handlererrors.ErrViewPipelineTooLong,
				"Pipeline length must be no longer than 1000 stages")
		}
		return nil
	}
}

// findStageCount is how many stages mongod runs a find on a view as, after
// the view's own: a $match, $sort, $skip, $limit and $project as set.
func findStageCount(params *common.FindParams) int {
	return boolInt(params.Filter.Len() > 0) + boolInt(params.Sort.Len() > 0) + boolInt(params.Skip > 0) +
		boolInt(params.Limit != 0) + boolInt(params.Projection.Len() > 0)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
func viewSourceIterator(ctx context.Context, db backends.Database, viewName, viewOn string, viewPipeline *types.Array, cmp *collation.Comparator, closer *iterator.MultiCloser, disablePushdown bool, checkLength func(viewStages int) error) (types.DocumentsIterator, error) { //nolint:lll // for readability
	baseCollection, viewStages, rawStages, err := resolveViewChain(ctx, db, viewName, viewOn, viewPipeline, cmp)
	if err != nil {
		return nil, err
	}
	if err = checkLength(len(rawStages)); err != nil {
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
	common.CanonicalizeFilter(filter)

	if !disablePushdown {
		qp.Filter = filter
	}

	return processStagesDocuments(ctx, closer, &stagesDocumentsParams{c: srcColl, qp: qp, stages: viewStages})
}
