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
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// sourceStages make their own documents rather than read the collection's,
// so the collection's documents cannot be run through them.
var sourceStages = map[string]struct{}{
	"$collStats": {}, "$indexStats": {}, "$documents": {}, "$currentOp": {}, "$listSessions": {},
	"$listLocalSessions": {}, "$planCacheStats": {}, "$changeStream": {}, "$listSearchIndexes": {},
}

// sbeStages are the stages MongoDB 8.0 runs in SBE when they directly follow
// those the query absorbs, reporting them inside the query plan rather than
// as stages.
var sbeStages = map[string]bool{
	"$group": true, "$bucket": true, "$sortByCount": true, "$count": true, "$lookup": true,
}

// explainClassicStages moves the explain of a pipeline MongoDB runs in the
// classic engine into its stages form: the query, with the leading stages it
// absorbs, under $cursor, then each remaining stage, at executionStats
// verbosity with the number of documents it returned. A pipeline every stage
// of which the query absorbs keeps the plain form.
func explainClassicStages(ctx context.Context, coll backends.Collection, qp *backends.ExplainParams, pipeline []any, response *types.Document, execStats bool) {
	docs := make([]*types.Document, 0, len(pipeline))
	for _, s := range pipeline {
		stage, _ := s.(*types.Document)
		if stage == nil || stage.Len() != 1 || stage.Command() == "$lookup" {
			return
		}
		if field, ok := must.NotFail(stage.Get(stage.Command())).(string); ok && stage.Command() == "$count" && len(docs) > 0 {
			docs = append(docs, desugaredCount(field)...)
			continue
		}
		docs = append(docs, stage)
	}
	absorbed := queryAbsorbedStages(docs)
	rest := docs[absorbed:]
	if len(rest) == 0 || sbeStages[rest[0].Command()] {
		return
	}

	var returned []int32
	if execStats {
		returned = stageReturns(ctx, coll, qp, docs, absorbed)
	}

	queryPlanner, _ := must.NotFail(response.Get("queryPlanner")).(*types.Document)
	out := cursorStage(response, queryPlanner)
	for i, stage := range rest {
		entry := reportedStage(stage)
		if returned != nil {
			setStageReturned(entry, returned[i])
		}
		out.Append(entry)
	}
	response.Set("stages", out)
}

// reportedStage is stage as mongod's explain shows it: a $unset as the
// $project that excludes its fields.
func reportedStage(stage *types.Document) *types.Document {
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
	// A $unset is absorbed only as the last stage.
	if i < len(docs) && (docs[i].Command() == "$project" || (docs[i].Command() == "$unset" && i == len(docs)-1)) {
		i++
	}
	return i
}

// stageReturns runs the pipeline on the documents qp's query reads and
// returns how many documents each stage from absorbed on returned, or nil
// when a stage cannot run here.
func stageReturns(ctx context.Context, coll backends.Collection, qp *backends.ExplainParams, docs []*types.Document, absorbed int) []int32 {
	for _, doc := range docs {
		if _, ok := sourceStages[doc.Command()]; ok {
			return nil
		}
	}
	built := make([]aggregations.Stage, len(docs))
	for i, doc := range docs {
		stage, err := stages.NewStage(doc)
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
	counters := make([]*countingIterator, 0, len(docs)-absorbed)
	for i, stage := range built {
		if iter, err = stage.Process(ctx, iter, closer); err != nil {
			return nil
		}
		if i >= absorbed {
			counter := &countingIterator{DocumentsIterator: iter}
			counters = append(counters, counter)
			iter = counter
		}
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
	return returned
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
