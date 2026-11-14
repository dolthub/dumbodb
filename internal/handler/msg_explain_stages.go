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

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// stageBuilder builds a pipeline stage that explain runs to count what it
// returns.
type stageBuilder func(*types.Document) (aggregations.Stage, error)

// buildLocalStage builds a stage that reads no other collection and writes
// nothing.
func buildLocalStage(doc *types.Document) (aggregations.Stage, error) {
	switch doc.Command() {
	case "$out", "$merge", "$lookup", "$graphLookup", "$unionWith", "$facet":
		return nil, errors.New("explain does not run " + doc.Command())
	}
	return stages.NewStage(doc)
}

// foreignStageBuilder is buildLocalStage that also builds the stages reading
// other collections of db.
func foreignStageBuilder(db backends.Database) stageBuilder {
	return func(doc *types.Document) (aggregations.Stage, error) {
		switch doc.Command() {
		case "$lookup", "$graphLookup", "$unionWith", "$facet":
			return newForeignCollectionStage(doc, db)
		}
		return buildLocalStage(doc)
	}
}

// sourceStages make their own documents rather than read the collection's,
// so the collection's documents cannot be run through them.
var sourceStages = map[string]struct{}{
	"$collStats": {}, "$indexStats": {}, "$documents": {}, "$currentOp": {}, "$listSessions": {},
	"$listLocalSessions": {}, "$planCacheStats": {}, "$changeStream": {}, "$listSearchIndexes": {},
}

// explainPipelineStages adds an explained pipeline's stages to response the
// way MongoDB 8.0 reports them. docs is the pipeline, with each $count as the
// $group and $project it runs, and sbe how much of it runs in SBE. The stages
// SBE runs past those the query absorbs run inside the query plan, as GROUP
// and EQ_LOOKUP stages over it. Any later stage moves the explain into a
// stages array: the query under $cursor, then each remaining stage, at
// executionStats verbosity with the number of documents it returned, a
// $lookup also naming the foreign indexes it reads. A pipeline every stage of
// which runs in the query keeps the plain form.
func explainPipelineStages(ctx context.Context, db backends.Database, coll backends.Collection, qp *backends.ExplainParams, dbName string, docs []*types.Document, sbe sbePlan, response *types.Document, execStats bool) { //nolint:lll // for readability
	if sbe.distinct != nil {
		explainDistinctScanGroup(ctx, db, coll, qp, docs, sbe.distinct, response, execStats)
		return
	}
	absorbed := queryAbsorbedStages(docs)
	end := absorbed
	for i := absorbed; i < sbe.end && sbeQueryPlanStages[docs[i].Command()]; i++ {
		end = i + 1
	}
	rest := docs[end:]
	if end > absorbed {
		if sorted := groupingSort(docs[end-1]); sorted != nil {
			rest = append([]*types.Document{sorted}, rest...)
		}
	}
	queryPlanner, _ := must.NotFail(response.Get("queryPlanner")).(*types.Document)
	if end == absorbed && len(rest) == 0 || queryPlanner == nil {
		return
	}

	var returned []int32
	if execStats {
		returned = stageReturns(ctx, coll, qp, docs, end, foreignStageBuilder(db))
		if returned != nil && len(rest) > len(docs)-end {
			// The $sort of a grouping stage returns what the grouping did.
			returned = append([]int32{returned[0]}, returned...)
		}
	}

	fetcher := &foreignFetcher{db: db}
	if plan, _ := queryPlanner.Get("winningPlan"); end > absorbed && plan != nil {
		plan := plan.(*types.Document)
		cached, _ := plan.Get("isCached")
		plan.Remove("isCached")
		for _, stage := range docs[absorbed:end] {
			plan = sbePlanStage(ctx, db, dbName, fetcher, stage, plan)
		}
		winning := must.NotFail(types.NewDocument("queryPlan", plan))
		if cached != nil {
			winning = must.NotFail(types.NewDocument("isCached", cached, "queryPlan", plan))
		}
		queryPlanner.Set("winningPlan", winning)
		response.Set("explainVersion", "2")
		if es, _ := response.Get("executionStats"); es != nil && returned != nil {
			es.(*types.Document).Set("nReturned", returned[0])
		}
	}
	if len(rest) == 0 {
		return
	}

	out := cursorStage(response, queryPlanner)
	if returned != nil {
		setStageReturned(must.NotFail(out.Get(0)).(*types.Document), returned[0])
	}
	appendReportedStages(ctx, fetcher, out, rest, returned, execStats)
	response.Set("stages", reportSorts(out))
}

// appendReportedStages appends to out the explain entries of rest, the
// stages after the query, with returned[i+1] the documents rest[i] returned
// when returned is set.
func appendReportedStages(ctx context.Context, fetcher *foreignFetcher, out *types.Array, rest []*types.Document, returned []int32, execStats bool) {
	for i := 0; i < len(rest); i++ {
		stage := rest[i]
		n := i + 1
		for j, entry := range reportedStages(stage) {
			// A classic $lookup absorbs the $unwind of its join array.
			if spec, _ := must.NotFail(entry.Get(entry.Command())).(*types.Document); entry.Command() == "$lookup" && spec != nil &&
				i+1 < len(rest) && unwindsJoin(spec, rest[i+1]) {
				spec.Set("unwinding", unwindingSpec(rest[i+1]))
				i++
				n = i + 1
			}
			if returned != nil {
				setStageReturned(entry, returned[n])
			}
			if spec, _ := must.NotFail(stage.Get(stage.Command())).(*types.Document); execStats && j == 0 && stage.Command() == "$lookup" && spec != nil {
				used := types.MakeArray(0)
				for _, name := range lookupIndexesUsed(ctx, fetcher, spec) {
					used.Append(name)
				}
				entry.Set("indexesUsed", used)
			}
			out.Append(entry)
		}
	}
}

// sbeQueryPlanStages are the stages SBE runs inside the query plan.
var sbeQueryPlanStages = map[string]bool{"$group": true, "$bucket": true, "$sortByCount": true, "$lookup": true}

// sbePlanStage is the query plan stage running stage, one of
// sbeQueryPlanStages, on the documents input returns.
func sbePlanStage(ctx context.Context, db backends.Database, dbName string, fetcher *foreignFetcher, stage, input *types.Document) *types.Document {
	if stage.Command() == "$lookup" {
		spec, _ := must.NotFail(stage.Get("$lookup")).(*types.Document)
		return eqLookupStage(ctx, db, dbName, fetcher, spec, input)
	}
	return must.NotFail(types.NewDocument("stage", "GROUP", "inputStage", input))
}

// reportedStages are the stages mongod's explain shows for stage: a $bucket
// or $sortByCount as the $group and $sort it runs, any other as
// reportedStage.
func reportedStages(stage *types.Document) []*types.Document {
	if sorted := groupingSort(stage); sorted != nil {
		return []*types.Document{groupingGroup(stage), sorted}
	}
	return []*types.Document{reportedStage(stage)}
}

// groupingSort is the $sort mongod runs after the $group of a $bucket or
// $sortByCount, or nil for any other stage.
func groupingSort(stage *types.Document) *types.Document {
	switch stage.Command() {
	case "$bucket":
		return must.NotFail(types.NewDocument("$sort", must.NotFail(types.NewDocument("_id", int32(1)))))
	case "$sortByCount":
		return must.NotFail(types.NewDocument("$sort", must.NotFail(types.NewDocument("count", int32(-1)))))
	}
	return nil
}

// groupingGroup is the $group mongod runs for a $bucket or $sortByCount.
func groupingGroup(stage *types.Document) *types.Document {
	spec := must.NotFail(stage.Get(stage.Command()))
	countAll := must.NotFail(types.NewDocument("$sum", int32(1)))
	if stage.Command() == "$sortByCount" {
		return must.NotFail(types.NewDocument("$group", must.NotFail(types.NewDocument("_id", spec, "count", countAll))))
	}
	bucket, _ := spec.(*types.Document)
	if bucket == nil {
		return stage.DeepCopy()
	}
	groupBy, _ := bucket.Get("groupBy")
	branches := types.MakeArray(0)
	if bounds, _ := bucket.Get("boundaries"); bounds != nil {
		if arr, ok := bounds.(*types.Array); ok {
			for i := 0; i+1 < arr.Len(); i++ {
				lo, hi := must.NotFail(arr.Get(i)), must.NotFail(arr.Get(i+1))
				inBucket := must.NotFail(types.NewDocument("$and", must.NotFail(types.NewArray(
					must.NotFail(types.NewDocument("$gte", must.NotFail(types.NewArray(groupBy, lo)))),
					must.NotFail(types.NewDocument("$lt", must.NotFail(types.NewArray(groupBy, hi)))),
				))))
				branches.Append(must.NotFail(types.NewDocument("case", inBucket, "then", lo)))
			}
		}
	}
	switchSpec := must.NotFail(types.NewDocument("branches", branches))
	if def, _ := bucket.Get("default"); def != nil {
		switchSpec.Set("default", def)
	}
	group := must.NotFail(types.NewDocument("_id", must.NotFail(types.NewDocument("$switch", switchSpec))))
	output, _ := bucket.Get("output")
	if out, ok := output.(*types.Document); ok {
		for _, k := range out.Keys() {
			group.Set(k, must.NotFail(out.Get(k)))
		}
	} else {
		group.Set("count", countAll)
	}
	return must.NotFail(types.NewDocument("$group", group))
}

// explainedStageDocs returns the stages of pipeline, each $count as the
// $group and $project mongod runs for it, or nil when a stage is not a
// one-field document.
func explainedStageDocs(pipeline []any) []*types.Document {
	docs := make([]*types.Document, 0, len(pipeline))
	for _, s := range pipeline {
		stage, _ := s.(*types.Document)
		if stage == nil || stage.Len() != 1 {
			return nil
		}
		if field, ok := must.NotFail(stage.Get(stage.Command())).(string); ok && stage.Command() == "$count" {
			docs = append(docs, desugaredCount(field)...)
			continue
		}
		docs = append(docs, stage)
	}
	return docs
}

// reportedStage is stage as mongod's explain shows it: a $unset as the
// $project that excludes its fields, and a $limit or $skip as a long.
func reportedStage(stage *types.Document) *types.Document {
	switch stage.Command() {
	case "$limit":
		if n, ok := stageInt64(stage, "$limit", common.GetLimitStageParam); ok {
			return must.NotFail(types.NewDocument("$limit", n))
		}
	case "$skip":
		if n, ok := stageInt64(stage, "$skip", common.GetSkipStageParam); ok {
			return must.NotFail(types.NewDocument("$skip", n))
		}
	}
	if stage.Command() != "$unset" {
		return stage.DeepCopy()
	}
	excluded := types.MakeDocument(0)
	switch fields := must.NotFail(stage.Get("$unset")).(type) {
	case string:
		excluded.Set(fields, false)
	case *types.Array:
		for i := 0; i < fields.Len(); i++ {
			if f, ok := must.NotFail(fields.Get(i)).(string); ok {
				excluded.Set(f, false)
			}
		}
	}
	return must.NotFail(types.NewDocument("$project", excluded))
}

// desugaredCount is the $group and $project MongoDB runs for a $count of
// field.
func desugaredCount(field string) []*types.Document {
	return []*types.Document{
		must.NotFail(types.NewDocument("$group", must.NotFail(types.NewDocument(
			"_id", types.Null, field, must.NotFail(types.NewDocument("$sum", int32(1))),
		)))),
		must.NotFail(types.NewDocument("$project", must.NotFail(types.NewDocument(field, true, "_id", false)))),
	}
}

// queryAbsorbedStages is how many leading stages MongoDB's query runs: its
// filter and sort, in either order, then skip and limit, then a projection.
func queryAbsorbedStages(docs []*types.Document) int {
	phases := map[string]int{"$match": 0, "$sort": 0, "$skip": 1, "$limit": 1}
	i, phase := 0, 0
	for ; i < len(docs); i++ {
		p, ok := phases[docs[i].Command()]
		if !ok || p < phase {
			break
		}
		phase = p
	}
	// A $unset is absorbed only as the last stage, and a $project computing
	// a field only by a query without a sort, skip or limit.
	if i < len(docs) && (docs[i].Command() == "$project" && (i == 0 || phase == 0 && !hasSort(docs[:i]) || !computesFields(docs[i])) ||
		docs[i].Command() == "$unset" && i == len(docs)-1) {
		i++
	}
	return i
}

func hasSort(docs []*types.Document) bool {
	for _, d := range docs {
		if d.Command() == "$sort" {
			return true
		}
	}
	return false
}

// computesFields reports whether a $project sets a field to an expression
// rather than only including or excluding fields.
func computesFields(project *types.Document) bool {
	spec, _ := must.NotFail(project.Get("$project")).(*types.Document)
	if spec == nil {
		return true
	}
	for _, v := range spec.Values() {
		switch v.(type) {
		case bool, int32, int64, float64, types.Decimal128:
		default:
			return true
		}
	}
	return false
}

// dependencyProjection is the projection mongod's query applies for the
// stages of docs it does not absorb, when they read only some fields, and
// whether it shows only under a blocking sort, a skip or a limit, as beneath
// SBE's stages. It is nil when the query absorbs a projection or the stages
// read the whole document.
func dependencyProjection(docs []*types.Document, sbe sbePlan) (*types.Document, bool) {
	absorbed := queryAbsorbedStages(docs)
	if absorbed == len(docs) || absorbed > 0 && docs[absorbed-1].Command() == "$project" {
		return nil, false
	}
	rest := make([]any, 0, len(docs)-absorbed)
	for _, d := range docs[absorbed:] {
		rest = append(rest, d)
	}
	roots, ok := pipelineRootFields(rest)
	if !ok || len(roots) == 0 {
		return nil, false
	}
	projection := types.MakeDocument(len(roots) + 1)
	for _, root := range roots {
		projection.Set(root, true)
	}
	if !projection.Has("_id") {
		projection.Set("_id", false)
	}
	return projection, sbe.end > absorbed
}

// stageReturns runs the pipeline on the documents qp's query reads and
// returns how many documents the stages from absorbed on pulled from the
// query, then how many each of them returned, or nil when a stage cannot run
// here.
func stageReturns(ctx context.Context, coll backends.Collection, qp *backends.ExplainParams, docs []*types.Document, absorbed int, build stageBuilder) []int32 {
	for _, doc := range docs {
		if _, ok := sourceStages[doc.Command()]; ok {
			return nil
		}
	}
	built := make([]aggregations.Stage, len(docs))
	for i, doc := range docs {
		stage, err := build(doc)
		if err != nil {
			return nil
		}
		built[i] = stage
	}

	query := &backends.QueryParams{Filter: qp.Filter, Collated: qp.Collated, Collation: qp.Collation, Hint: qp.Hint}
	qres, err := coll.Query(ctx, query)
	if err != nil || qres == nil || qres.Iter == nil {
		return nil
	}
	closer := iterator.NewMultiCloser(qres.Iter)
	defer closer.Close()

	var iter types.DocumentsIterator = iterator.WithContext(ctx, qres.Iter)
	counters := make([]*countingIterator, 0, len(docs)-absorbed+1)
	for i, stage := range built {
		if i == absorbed {
			// What the stages pull from the query, which a later $limit
			// can cut short.
			counter := &countingIterator{DocumentsIterator: iter}
			counters = append(counters, counter)
			iter = counter
		}
		if iter, err = stage.Process(ctx, iter, closer); err != nil {
			return nil
		}
		if i >= absorbed {
			counter := &countingIterator{DocumentsIterator: iter}
			counters = append(counters, counter)
			iter = counter
		}
	}
	if len(counters) == 0 {
		// Every stage runs in the query plan.
		counter := &countingIterator{DocumentsIterator: iter}
		counters = append(counters, counter)
		iter = counter
	}
	for {
		if _, _, err = iter.Next(); err != nil {
			break
		}
	}
	if !errors.Is(err, iterator.ErrIteratorDone) {
		return nil
	}
	returned := make([]int32, len(counters))
	for i, c := range counters {
		returned[i] = c.n
	}
	// A per-document stage the query plan runs, such as an SBE $lookup, pulls
	// as lazily as one after it.
	from := min(queryAbsorbedStages(docs), absorbed)
	if pulled, ok := pulledBeforeLimit(ctx, coll, query, docs[from:], built[from:]); ok {
		returned[0] = pulled
	}
	return returned
}

// perDocumentStages map each input document to outputs of its own, so a
// $limit after them stops the query once enough have come out.
var perDocumentStages = map[string]bool{
	"$lookup": true, "$unwind": true, "$set": true, "$addFields": true, "$project": true, "$unset": true,
	"$match": true, "$replaceRoot": true, "$replaceWith": true,
}

// pulledBeforeLimit returns how many documents the query yields before a
// $limit reached through only per-document stages has its documents, as
// mongod's lazy pipeline pulls them, or false when the stages do not end in
// such a $limit.
func pulledBeforeLimit(ctx context.Context, coll backends.Collection, query *backends.QueryParams, docs []*types.Document, built []aggregations.Stage) (int32, bool) {
	l := 0
	for ; l < len(docs) && docs[l].Command() != "$limit"; l++ {
		if !perDocumentStages[docs[l].Command()] {
			return 0, false
		}
	}
	if l == len(docs) {
		return 0, false
	}
	limit, _ := stageInt64(docs[l], "$limit", common.GetLimitStageParam)
	qres, err := coll.Query(ctx, query)
	if err != nil || qres == nil || qres.Iter == nil {
		return 0, false
	}
	defer qres.Iter.Close()
	var pulled int32
	var out int64
	for out < limit {
		_, doc, err := qres.Iter.Next()
		if err != nil {
			break
		}
		pulled++
		closer := iterator.NewMultiCloser()
		var iter types.DocumentsIterator = iterator.Values(iterator.ForSlice([]*types.Document{doc}))
		for _, stage := range built[:l] {
			if iter, err = stage.Process(ctx, iter, closer); err != nil {
				closer.Close()
				return 0, false
			}
		}
		for out < limit {
			if _, _, err = iter.Next(); err != nil {
				break
			}
			out++
		}
		closer.Close()
	}
	return pulled, true
}

// cursorStage moves response's query explain under a $cursor stage, the first
// of the returned stages, with the number of documents it returned at
// executionStats verbosity.
func cursorStage(response, queryPlanner *types.Document) *types.Array {
	cursor := must.NotFail(types.NewDocument("queryPlanner", queryPlanner))
	entry := must.NotFail(types.NewDocument("$cursor", cursor))
	if es, _ := response.Get("executionStats"); es != nil {
		cursor.Set("executionStats", es)
		response.Remove("executionStats")
		if n, err := es.(*types.Document).Get("nReturned"); err == nil {
			setStageReturned(entry, toInt32(n))
		}
	}
	response.Remove("queryPlanner")
	return must.NotFail(types.NewArray(entry))
}

// setStageReturned sets a pipeline stage's executionStats counters.
func setStageReturned(entry *types.Document, n int32) {
	entry.Set("nReturned", int64(n))
	entry.Set("executionTimeMillisEstimate", int64(0))
}

func toInt32(v any) int32 {
	switch n := v.(type) {
	case int32:
		return n
	case int64:
		return int32(n)
	}
	return 0
}

// explainStatsStages explains a pipeline led by $collStats or $indexStats as
// mongod does, reporting false for any other: only its stages, which read no
// query, each at executionStats verbosity with the documents it returned.
func (h *Handler) explainStatsStages(ctx context.Context, db backends.Database, dbName, cName string, coll backends.Collection, pipeline []any, response *types.Document, execStats bool) bool {
	docs := make([]*types.Document, 0, len(pipeline))
	for _, s := range pipeline {
		doc, _ := s.(*types.Document)
		if doc == nil || doc.Len() != 1 {
			return false
		}
		docs = append(docs, doc)
	}
	if len(docs) == 0 || docs[0].Command() != "$collStats" && docs[0].Command() != "$indexStats" {
		return false
	}
	var returned []int32
	if execStats {
		var err error
		if returned, err = h.statsStageReturns(ctx, db, dbName, cName, coll, docs); err != nil {
			return false
		}
	}
	out := types.MakeArray(len(docs))
	for i, doc := range docs {
		entry := reportedStage(doc)
		if returned != nil {
			setStageReturned(entry, returned[i])
		}
		out.Append(entry)
	}
	response.Remove("queryPlanner")
	response.Remove("executionStats")
	response.Set("stages", out)
	return true
}

// statsStageReturns runs docs, a pipeline led by $collStats or $indexStats,
// once and returns how many documents each stage returned.
func (h *Handler) statsStageReturns(ctx context.Context, db backends.Database, dbName, cName string, coll backends.Collection, docs []*types.Document) ([]int32, error) {
	built := make([]aggregations.Stage, len(docs))
	for i, doc := range docs {
		var err error
		switch doc.Command() {
		case "$indexStats":
			built[i], err = stages.NewIndexStatsStage(doc, ctx, coll, h.TCPHost, func(indexes []backends.IndexInfo) []backends.IndexInfo {
				return clientVisibleIndexes(ctx, db, cName, indexes)
			})
		default:
			built[i], err = foreignStageBuilder(db)(doc)
		}
		if err != nil {
			return nil, err
		}
	}
	closer := iterator.NewMultiCloser()
	defer closer.Close()
	var iter types.DocumentsIterator
	var err error
	if docs[0].Command() == "$collStats" {
		iter, err = processStagesStats(ctx, closer, &stagesStatsParams{
			coll, db, dbName, cName, stages.GetStatistics(built[:1]), built[:1], h.TCPHost, false,
		})
	} else {
		iter, err = built[0].Process(ctx, iterator.Values(iterator.ForSlice([]*types.Document{})), closer)
	}
	if err != nil {
		return nil, err
	}
	counters := make([]*countingIterator, len(built))
	counters[0] = &countingIterator{DocumentsIterator: iterator.WithContext(ctx, iter)}
	iter = counters[0]
	for i, stage := range built[1:] {
		if iter, err = stage.Process(ctx, iter, closer); err != nil {
			return nil, err
		}
		counters[i+1] = &countingIterator{DocumentsIterator: iterator.WithContext(ctx, iter)}
		iter = counters[i+1]
	}
	for {
		if _, _, err = iter.Next(); err != nil {
			break
		}
	}
	if !errors.Is(err, iterator.ErrIteratorDone) {
		return nil, err
	}
	returned := make([]int32, len(counters))
	for i, c := range counters {
		returned[i] = c.n
	}
	return returned, nil
}

// reportSorts rewrites each $sort entry of an explain's stages as mongod
// reports it: its pattern under sortKey, with the $limit directly after it
// folded in as limit, and, at executionStats verbosity, its sort counters.
func reportSorts(out *types.Array) *types.Array {
	res := types.MakeArray(out.Len())
	for i := 0; i < out.Len(); i++ {
		entry := must.NotFail(out.Get(i)).(*types.Document)
		if entry.Command() != "$sort" {
			res.Append(entry)
			continue
		}
		sort := must.NotFail(types.NewDocument("sortKey", must.NotFail(entry.Get("$sort"))))
		returned, hasStats := entry.Get("nReturned")
		if i+1 < out.Len() {
			if next := must.NotFail(out.Get(i + 1)).(*types.Document); next.Command() == "$limit" {
				limit, _ := common.GetLimitStageParam(must.NotFail(next.Get("$limit")))
				sort.Set("limit", limit)
				returned, hasStats = next.Get("nReturned")
				i++
			}
		}
		reported := must.NotFail(types.NewDocument("$sort", sort))
		if hasStats == nil {
			reported.Set("totalDataSizeSortedBytesEstimate", int64(0))
			reported.Set("usedDisk", false)
			reported.Set("spills", int64(0))
			reported.Set("spilledDataStorageSize", int64(0))
			reported.Set("nReturned", returned)
			reported.Set("executionTimeMillisEstimate", int64(0))
		}
		res.Append(reported)
	}
	return res
}
