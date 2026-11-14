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
	"slices"

	"github.com/FerretDB/wire"
	mongobson "go.mongodb.org/mongo-driver/bson"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// catalogStage is a stage mongod expands $listClusterCatalog into, as its
// explain shows it, with the fields it sets or removes.
type catalogStage struct {
	json     string
	modifies []string
}

var (
	catalogLeadingStages = []catalogStage{
		{json: `{"$_internalListCollections": {}}`},
		{json: `{"$lookup": {"from": {"db": "config", "coll": "collections"}, "as": "trackedCollectionInfo", "localField": "ns", "foreignField": "_id"}}`,
			modifies: []string{"trackedCollectionInfo"}},
		{json: `{"$addFields": {"firstTrackedCollectionInfo": {"$first": ["$trackedCollectionInfo"]}}}`,
			modifies: []string{"firstTrackedCollectionInfo"}},
		{json: `{"$addFields": {"sharded": {"$and": [{"$ne": ["$trackedCollectionInfo", {"$const": []}]}, ` +
			`{"$ne": ["$firstTrackedCollectionInfo.unsplittable", {"$const": true}]}]}, ` +
			`"tracked": {"$ne": ["$trackedCollectionInfo", {"$const": []}]}}}`,
			modifies: []string{"sharded", "tracked"}},
		{json: `{"$addFields": {"shardKey": {"$cond": ["$sharded", "$firstTrackedCollectionInfo.key", "$$REMOVE"]}}}`,
			modifies: []string{"shardKey"}},
	}
	catalogShardsStages = []catalogStage{
		{json: `{"$lookup": {"from": {"db": "config", "coll": "chunks"}, "as": "matchingShards", "localField": "info.uuid", ` +
			`"foreignField": "uuid", "let": {}, "pipeline": [{"$group": {"_id": null, "shards": {"$addToSet": "$shard"}}}]}}`,
			modifies: []string{"matchingShards"}},
		{json: `{"$addFields": {"shards": {"$cond": ["$tracked", {"$first": ["$matchingShards.shards"]}, ` +
			`{"$cond": ["$primary", ["$primary"], {"$const": []}]}]}}}`,
			modifies: []string{"shards"}},
	}
	catalogBalancingStages = []catalogStage{
		{json: `{"$addFields": {"balancingEnabled": {"$cond": ["$sharded", {"$and": [{"$not": [{"$ifNull": ` +
			`["$firstTrackedCollectionInfo.noBalance", {"$const": false}]}]}, {"$ifNull": ["$firstTrackedCollectionInfo.permitMigrations", ` +
			`{"$const": true}]}]}, "$$REMOVE"]}, "balancingEnabledReason": {"$cond": ["$sharded", {"enableBalancing": {"$not": ` +
			`[{"$ifNull": ["$firstTrackedCollectionInfo.noBalance", {"$const": false}]}]}, "allowMigrations": {"$ifNull": ` +
			`["$firstTrackedCollectionInfo.permitMigrations", {"$const": true}]}}, "$$REMOVE"]}, "autoMergingEnabled": {"$cond": ` +
			`["$sharded", {"$ne": ["$firstTrackedCollectionInfo.enableAutoMerge", {"$const": false}]}, "$$REMOVE"]}, "chunkSize": ` +
			`{"$cond": ["$sharded", {"$ifNull": [{"$divide": ["$firstTrackedCollectionInfo.maxChunkSizeBytes", {"$const": 1048576}]}, ` +
			`{"$const": null}]}, "$$REMOVE"]}}}`,
			modifies: []string{"balancingEnabled", "balancingEnabledReason", "autoMergingEnabled", "chunkSize"}},
	}
	catalogTrailingStages = []catalogStage{
		{json: `{"$project": {"tracked": false, "_id": true}}`, modifies: []string{"tracked"}},
		{json: `{"$project": {"trackedCollectionInfo": false, "firstTrackedCollectionInfo": false, "matchingShards": false, ` +
			`"primary": false, "_id": true}}`,
			modifies: []string{"trackedCollectionInfo", "firstTrackedCollectionInfo", "matchingShards", "primary"}},
	}
)

// explainListClusterCatalog explains {aggregate: 1} led by $listClusterCatalog
// as mongod does: the stages it expands into, with a leading $match moved
// back past those that do not set its fields, then the later stages, each at
// executionStats verbosity with the documents it returned. It reports false
// for any other command.
func (h *Handler) explainListClusterCatalog(ctx context.Context, document, inner *types.Document) (*wire.OpMsg, bool, error) {
	if inner.Command() != "aggregate" {
		return nil, false, nil
	}
	if _, isName := must.NotFail(inner.Get("aggregate")).(string); isName {
		return nil, false, nil
	}
	p, _ := inner.Get("pipeline")
	pipeline, _ := p.(*types.Array)
	if pipeline == nil || pipeline.Len() == 0 {
		return nil, false, nil
	}
	first, _ := must.NotFail(pipeline.Get(0)).(*types.Document)
	if first == nil || first.Len() != 1 || first.Command() != "$listClusterCatalog" {
		return nil, false, nil
	}

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, true, err
	}
	if err = checkDatabasePipelineStages(pipeline, dbName); err != nil {
		return nil, true, err
	}
	if err = checkDatabaseStagePositions(pipeline); err != nil {
		return nil, true, err
	}
	spec, err := stages.ParseListClusterCatalogSpec(first)
	if err != nil {
		return nil, true, err
	}
	var rest []any
	for i := 1; i < pipeline.Len(); i++ {
		rest = append(rest, must.NotFail(pipeline.Get(i)))
	}
	later := explainedStageDocs(rest)
	if len(rest) > 0 && later == nil {
		return nil, true, lazyerrors.New("unexpected pipeline stage")
	}
	counted := true
	for _, doc := range later {
		switch {
		case doc.Command() == "$out" || doc.Command() == "$merge":
			counted = false
		case foreignReading[doc.Command()]:
		default:
			if _, err = stages.NewStage(doc.DeepCopy()); err != nil {
				return nil, true, err
			}
		}
	}

	expansion := slices.Clone(catalogLeadingStages)
	if spec.Shards {
		expansion = append(expansion, catalogShardsStages...)
	}
	if spec.BalancingConfiguration {
		expansion = append(expansion, catalogBalancingStages...)
	}
	expansion = append(expansion, catalogTrailingStages...)

	// A leading $match runs right after the last stage setting a field it
	// reads.
	matchAt := -1
	if len(later) > 0 && later[0].Command() == "$match" {
		filter, _ := must.NotFail(later[0].Get("$match")).(*types.Document)
		matchAt = len(expansion)
		if fields, ok := common.FilterRootFields(filter); ok && filter != nil {
			for matchAt > 1 && !slices.ContainsFunc(expansion[matchAt-1].modifies, func(f string) bool { return slices.Contains(fields, f) }) {
				matchAt--
			}
		}
	}

	verbosity, _ := document.Get("verbosity")
	execStats := verbosity != "queryPlanner" && counted
	var pulled int32
	var counts []int32
	if execStats {
		entries, err := h.listClusterCatalogEntries(ctx, spec, dbName)
		if err != nil {
			return nil, true, err
		}
		db, err := h.b.Database(dbName)
		if err != nil {
			return nil, true, lazyerrors.Error(err)
		}
		if pulled, counts, err = sourceStageCounts(ctx, entries, later, foreignStageBuilder(db)); err != nil {
			return nil, true, err
		}
	}

	out := types.MakeArray(len(expansion) + len(later))
	n := pulled
	appendExpansion := func(stage catalogStage) error {
		var doc mongobson.Raw
		if err := mongobson.UnmarshalExtJSON([]byte(stage.json), false, &doc); err != nil {
			return lazyerrors.Error(err)
		}
		entry, err := bson.DecodeRawDocument(doc)
		if err != nil {
			return lazyerrors.Error(err)
		}
		if execStats {
			if entry.Command() == "$lookup" {
				entry.Set("totalDocsExamined", int64(0))
				entry.Set("totalKeysExamined", int64(0))
				entry.Set("collectionScans", int64(0))
				entry.Set("indexesUsed", types.MakeArray(0))
			}
			setStageReturned(entry, n)
		}
		out.Append(entry)
		return nil
	}
	laterEntries := types.MakeArray(len(later))
	if execStats {
		counts = append([]int32{pulled}, counts...)
	}
	appendReportedStages(ctx, &foreignFetcher{db: must.NotFail(h.b.Database(dbName))}, laterEntries, later, counts, execStats)
	for i, stage := range expansion {
		if i == matchAt {
			out.Append(must.NotFail(laterEntries.Get(0)))
			if execStats {
				n = counts[1]
			}
		}
		if err = appendExpansion(stage); err != nil {
			return nil, true, err
		}
	}
	for i := range laterEntries.Len() {
		if i == 0 && matchAt >= 0 && matchAt < len(expansion) {
			continue
		}
		out.Append(must.NotFail(laterEntries.Get(i)))
	}

	cmd := inner.DeepCopy()
	cmd.Set("$db", dbName)
	reply, err := documentOpMsg(must.NotFail(types.NewDocument(
		"explainVersion", "1",
		"stages", reportSorts(out),
		"command", cmd,
		"ok", float64(1),
	)))
	return reply, true, err
}

// foreignReading are the stages that read another collection, which need
// the database to build.
var foreignReading = map[string]bool{"$lookup": true, "$graphLookup": true, "$unionWith": true, "$facet": true}

// sourceStageCounts runs docs, the documents a source stage made, through
// the stages specs and returns how many of docs the stages pulled and how
// many each returned. Stages that map each document to their own outputs,
// up to a $limit, pull no more than the limit needs.
func sourceStageCounts(ctx context.Context, docs, specs []*types.Document, build stageBuilder) (int32, []int32, error) {
	built := make([]aggregations.Stage, len(specs))
	for i, spec := range specs {
		var err error
		if built[i], err = build(spec); err != nil {
			return 0, nil, err
		}
	}
	counts := make([]int32, len(specs))
	run := func(stage aggregations.Stage, in []*types.Document) ([]*types.Document, error) {
		closer := iterator.NewMultiCloser()
		defer closer.Close()
		iter, err := stage.Process(ctx, iterator.Values(iterator.ForSlice(in)), closer)
		if err != nil {
			return nil, err
		}
		var res []*types.Document
		for {
			_, doc, err := iter.Next()
			if errors.Is(err, iterator.ErrIteratorDone) {
				return res, nil
			}
			if err != nil {
				return nil, err
			}
			res = append(res, doc)
		}
	}

	l := 0
	for l < len(specs) && perDocumentStages[specs[l].Command()] && specs[l].Command() != "$limit" {
		l++
	}
	pulled := int32(len(docs))
	from := 0
	if l < len(specs) && specs[l].Command() == "$limit" {
		limit, _ := stageInt64(specs[l], "$limit", common.GetLimitStageParam)
		var kept []*types.Document
		pulled = 0
		for _, doc := range docs {
			if int64(len(kept)) >= limit {
				break
			}
			pulled++
			outs := []*types.Document{doc}
			for i := 0; i < l; i++ {
				var err error
				if outs, err = run(built[i], outs); err != nil {
					return 0, nil, err
				}
				counts[i] += int32(len(outs))
			}
			kept = append(kept, outs[:min(int64(len(outs)), limit-int64(len(kept)))]...)
		}
		counts[l] = int32(len(kept))
		docs, from = kept, l+1
	}
	for i := from; i < len(specs); i++ {
		var err error
		if docs, err = run(built[i], docs); err != nil {
			return 0, nil, err
		}
		counts[i] = int32(len(docs))
	}
	return pulled, counts, nil
}
