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
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// explainDistinctScanGroup explains a pipeline whose $group mongod runs as a
// distinct scan: a DISTINCT_SCAN query plan under $cursor, then
// $groupByDistinctScan and the stages after the $group.
func explainDistinctScanGroup(ctx context.Context, db backends.Database, coll backends.Collection, qp *backends.ExplainParams, docs []*types.Document, ds *distinctScanGroup, response *types.Document, execStats bool) { //nolint:lll // for readability
	queryPlanner, _ := must.NotFail(response.Get("queryPlanner")).(*types.Document)
	if queryPlanner == nil {
		return
	}
	stageNames := []string{"DISTINCT_SCAN"}
	if ds.fetch {
		stageNames = append(stageNames, "FETCH")
	}
	if ds.projection != "" {
		stageNames = append(stageNames, ds.projection)
	}
	plan := func(counts func(name string, stage *types.Document)) *types.Document {
		var node *types.Document
		for _, name := range stageNames {
			stage := must.NotFail(types.NewDocument("stage", name))
			if name == "DISTINCT_SCAN" {
				stage.Set("indexName", ds.index.Name)
				stage.Set("keyPattern", indexKeyPattern(ds.index))
			}
			if counts != nil {
				counts(name, stage)
			}
			if node != nil {
				stage.Set("inputStage", node)
			}
			node = stage
		}
		return node
	}
	winning := plan(nil)
	winning.Set("isCached", false)
	queryPlanner.Set("winningPlan", winning)

	rest := docs[ds.group+1:]
	var returned []int32
	if execStats {
		groups, keys := distinctScanCounts(ctx, coll, qp, ds)
		var docsExamined int32
		if ds.fetch {
			docsExamined = groups
		}
		stagesDoc := plan(func(name string, stage *types.Document) {
			stage.Set("nReturned", groups)
			stage.Set("executionTimeMillisEstimate", int64(0))
			switch name {
			case "DISTINCT_SCAN":
				stage.Set("keysExamined", keys)
			case "FETCH":
				stage.Set("docsExamined", docsExamined)
			}
		})
		response.Set("executionStats", must.NotFail(types.NewDocument(
			"executionSuccess", true,
			"nReturned", groups,
			"executionTimeMillis", int64(0),
			"totalKeysExamined", keys,
			"totalDocsExamined", docsExamined,
			"executionStages", stagesDoc,
		)))
		returned = stageReturns(ctx, coll, qp, docs, ds.group+1, foreignStageBuilder(db))
		if returned == nil && len(rest) == 0 {
			returned = []int32{groups}
		}
	}

	out := cursorStage(response, queryPlanner)
	entry := must.NotFail(types.NewDocument("$groupByDistinctScan", must.NotFail(types.NewDocument("newRoot", ds.newRoot))))
	if returned != nil {
		setStageReturned(entry, returned[0])
	}
	out.Append(entry)
	appendReportedStages(ctx, &foreignFetcher{db: db}, out, rest, returned, execStats)
	response.Set("stages", reportSorts(out))
}

// indexKeyPattern is idx's key pattern document.
func indexKeyPattern(idx backends.IndexInfo) *types.Document {
	pattern := types.MakeDocument(len(idx.Key))
	for _, k := range idx.Key {
		if k.Descending {
			pattern.Set(k.Field, int32(-1))
		} else {
			pattern.Set(k.Field, int32(1))
		}
	}
	return pattern
}

// distinctScanCounts returns the distinct keys ds's scan returns and the
// index keys it examines: each it returns, then, past each run of keys
// outside the query's bounds, the first such key it lands on.
func distinctScanCounts(ctx context.Context, coll backends.Collection, qp *backends.ExplainParams, ds *distinctScanGroup) (int32, int32) {
	qres, err := coll.Query(ctx, &backends.QueryParams{Collation: qp.Collation, Collated: qp.Collated})
	if err != nil || qres == nil || qres.Iter == nil {
		return 0, 0
	}
	closer := iterator.NewMultiCloser(qres.Iter)
	defer closer.Close()

	order := types.MakeDocument(len(ds.index.Key))
	for _, k := range ds.index.Key {
		dir := int32(1)
		if k.Descending != ds.backward {
			dir = -1
		}
		order.Set(k.Field, dir)
	}
	sortStage, err := stages.NewStage(must.NotFail(types.NewDocument("$sort", order)))
	if err != nil {
		return 0, 0
	}
	iter, err := sortStage.Process(ctx, iterator.WithContext(ctx, qres.Iter), closer)
	if err != nil {
		return 0, 0
	}
	path, err := types.NewPathFromString(ds.key)
	if err != nil {
		return 0, 0
	}

	// The scan seeks to its first bounds, so the keys before them go
	// unexamined unless the bounds start at MinKey.
	minKeyDoc := types.MakeDocument(1)
	_ = minKeyDoc.SetByPath(path, types.MinKey)
	minKeyInBounds, _ := common.FilterDocument(minKeyDoc, ds.keyFilter)
	var groups, keys int32
	var last any
	inGap, leading := false, true
	for {
		_, doc, err := iter.Next()
		if err != nil {
			if !errors.Is(err, iterator.ErrIteratorDone) {
				return 0, 0
			}
			return groups, keys
		}
		if pfe := ds.index.PartialFilterExpression; pfe != nil {
			if in, ferr := common.FilterDocument(doc, pfe); ferr != nil || !in {
				continue
			}
		}
		key, kerr := doc.GetByPath(path)
		if kerr != nil {
			key = types.Null
		}
		if last != nil && types.CompareOrder(key, last, types.Ascending) == types.Equal {
			continue
		}
		if ok, ferr := common.FilterDocument(doc, qp.Filter); ferr == nil && ok {
			groups++
			keys++
			last, inGap, leading = key, false, false
			continue
		}
		if leading && !minKeyInBounds {
			if inBounds, _ := common.FilterDocument(doc, ds.keyFilter); !inBounds {
				continue
			}
		}
		if !inGap {
			keys++
		}
		inGap, leading = true, false
	}
}
