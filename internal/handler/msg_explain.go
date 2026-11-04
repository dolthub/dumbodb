// Copyright 2021 FerretDB Inc.
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
	"os"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
	"github.com/dolthub/dumbodb/internal/version"
)

// explainRun is what running an explained command did: how many documents
// its plan's stages returned, and the index keys and documents it read.
type explainRun struct {
	// returned is the root stage's nReturned: the documents the command
	// returns, or zero for a write or count.
	returned int32
	// matched is what the stage that fetches or scans documents returned:
	// those matching the filter.
	matched int32
	// scanned is what the leaf index scan returned.
	scanned    int32
	keys, docs int64
	// branches are the counts of the index scans under an OR or SORT_MERGE
	// stage.
	branches []backends.ExplainBranchCounts
}

// outerDocs are the documents run read outside the fetches under an OR.
func (run explainRun) outerDocs() int64 {
	docs := run.docs
	for _, b := range run.branches {
		docs -= b.DocsExamined
	}
	return docs
}

// runExplained runs the explained command's read the way the command itself
// runs it, counting the index keys and documents it reads. A backend error
// leaves the counts reached so far, so the caller still produces a
// structurally valid executionStats document.
func (h *Handler) runExplained(ctx context.Context, coll backends.Collection, params *common.ExplainParams, qp *backends.ExplainParams, viaView bool) explainRun {
	ctx, examined := backends.WithExaminedCounts(ctx)
	var run explainRun
	switch {
	case qp.Command == "count" && !viaView:
		run = h.runExplainedCount(ctx, coll, params, qp)
	case qp.Command == "distinct" && !viaView:
		run = h.runExplainedDistinct(ctx, coll, params, qp)
	case qp.Command == "update" || qp.Command == "delete" || qp.Command == "findAndModify":
		run = runExplainedWrite(ctx, coll, qp)
	default:
		run = runExplainedFind(ctx, coll, qp)
	}
	run.keys, run.docs, run.branches = examined.Keys(), examined.Docs(), examined.Branches()
	return run
}

func (h *Handler) runExplainedCount(ctx context.Context, coll backends.Collection, params *common.ExplainParams, qp *backends.ExplainParams) explainRun {
	cp, err := common.GetCountParams(params.Command, h.L)
	if err != nil {
		return explainRun{}
	}
	common.CanonicalizeFilter(cp.Filter)
	cp.Collation = qp.Collation
	n, _ := h.execCount(ctx, coll, cp, collation.Parse(qp.Collation).Comparator())
	return explainRun{matched: n, scanned: n}
}

func (h *Handler) runExplainedDistinct(ctx context.Context, coll backends.Collection, params *common.ExplainParams, qp *backends.ExplainParams) explainRun {
	dp, err := common.GetDistinctParams(params.Command, h.L)
	if err != nil {
		return explainRun{}
	}
	dp.Collation = qp.Collation
	values, err := h.execDistinct(ctx, coll, dp, collation.Parse(qp.Collation).Comparator())
	if err != nil {
		return explainRun{}
	}
	n := int32(values.Len())
	return explainRun{returned: n, matched: n, scanned: n}
}

// runExplainedWrite reads the documents an update, delete or findAndModify
// would write, the way the write does, without writing them. A single-document
// write stops at its first match.
func runExplainedWrite(ctx context.Context, coll backends.Collection, qp *backends.ExplainParams) explainRun {
	var run explainRun
	single := qp.Command == "findAndModify" || !qp.Multi
	run.matched = scanMatching(ctx, coll, &backends.QueryParams{Filter: qp.Filter, Collated: qp.Collated}, qp, single)
	if qp.Command == "findAndModify" {
		run.returned = run.matched
	}
	run.scanned = run.matched
	return run
}

// runExplainedFind pulls documents the way the query pipeline does, reading
// only qp's fields when set: when no blocking sort is needed (no sort, or the
// backend walked an index in sort order) a limit stops the pass, otherwise
// every candidate is examined and the limit only caps what is returned.
func runExplainedFind(ctx context.Context, coll backends.Collection, qp *backends.ExplainParams) explainRun {
	limit := int32(max(qp.Limit, -qp.Limit))
	query := &backends.QueryParams{Filter: qp.Filter, Collated: qp.Collated, Collation: qp.Collation, Hint: qp.Hint, Fields: qp.Fields}
	if limit > 0 {
		query.LimitHint = qp.Skip + int64(limit)
	}
	if qp.Sort.Len() == 1 {
		query.Sort = qp.Sort
	}
	var run explainRun
	run.matched = scanMatching(ctx, coll, query, qp, false)
	run.returned = run.matched
	if limit > 0 && run.returned > limit {
		run.returned = limit
	}
	run.scanned = run.matched
	return run
}

// scanMatching runs query and returns how many of its documents match qp's
// filter, stopping at the first when single is set, or at qp's limit when the
// query's order is the requested one.
func scanMatching(ctx context.Context, coll backends.Collection, query *backends.QueryParams, qp *backends.ExplainParams, single bool) int32 {
	qres, err := coll.Query(ctx, query)
	if err != nil || qres == nil || qres.Iter == nil {
		return 0
	}
	defer qres.Iter.Close()

	limit := int32(max(qp.Limit, -qp.Limit))
	if single {
		limit = 1
	}
	stopAtLimit := limit > 0 && (single || qp.Sort.Len() == 0 || qres.Sorted)
	cmp := collation.Parse(qp.Collation).Comparator()

	var matched int32
	for !stopAtLimit || matched < limit {
		_, doc, err := qres.Iter.Next()
		if err != nil {
			break
		}
		if ok, ferr := common.FilterDocumentColl(doc, qp.Filter, cmp); ferr == nil && ok {
			matched++
		}
	}
	return matched
}

// buildExecutionStages clones the winningPlan tree into the executionStages
// document, with the counts run measured. Below a stage with more than one
// input, counts are left at zero.
func buildExecutionStages(plan *types.Document, rootStage string, run explainRun) *types.Document {
	if plan == nil {
		return must.NotFail(types.NewDocument(
			"stage", rootStage,
			"nReturned", run.returned,
			"executionTimeMillisEstimate", int64(0),
		))
	}
	return cloneExecutionStage(plan, run, true, true)
}

// cloneExecutionStage copies node with the counts run measured. root marks
// the plan's top stage; aboveMatch marks a stage above the one that fetches or
// scans documents, which returns what the command returns.
func cloneExecutionStage(node *types.Document, run explainRun, root, aboveMatch bool) *types.Document {
	d := must.NotFail(types.NewDocument())
	stage, _ := node.Get("stage")
	if stage != nil {
		d.Set("stage", stage)
	}
	child, _ := node.Get("inputStage")
	childDoc, _ := child.(*types.Document)
	children, _ := node.Get("inputStages")
	childArr, _ := children.(*types.Array)

	var nReturned int32
	switch {
	case root:
		nReturned = run.returned
	case childArr != nil:
		// An OR or SORT_MERGE returns each document its branches found once,
		// all fetched.
		nReturned = int32(run.outerDocs())
	case stage == "FETCH" || stage == "COLLSCAN":
		nReturned = run.matched
	case childDoc == nil || !aboveMatch:
		nReturned = run.scanned
	default:
		nReturned = run.returned
	}
	if stage == "FETCH" || stage == "COLLSCAN" {
		aboveMatch = false
	}
	d.Set("nReturned", nReturned)
	d.Set("executionTimeMillisEstimate", int64(0))
	switch stage {
	case "IXSCAN", "COUNT_SCAN", "DISTINCT_SCAN":
		d.Set("keysExamined", int32(run.keys))
	case "FETCH", "COLLSCAN":
		d.Set("docsExamined", int32(run.outerDocs()))
	case "EXPRESS_IXSCAN", "EXPRESS_UPDATE", "EXPRESS_DELETE":
		d.Set("keysExamined", int32(run.keys))
		d.Set("docsExamined", int32(run.docs))
	}
	if v, _ := node.Get("indexName"); v != nil {
		d.Set("indexName", v)
	}
	if v, _ := node.Get("keyPattern"); v != nil {
		d.Set("keyPattern", v)
	}
	if childDoc != nil {
		d.Set("inputStage", cloneExecutionStage(childDoc, run, false, aboveMatch))
	}
	if childArr != nil {
		out := types.MakeArray(childArr.Len())
		for i := 0; i < childArr.Len(); i++ {
			var branch explainRun
			if i < len(run.branches) {
				b := run.branches[i]
				branch = explainRun{scanned: int32(b.NReturned), keys: b.KeysExamined, docs: b.DocsExamined, matched: int32(b.Matched)}
			}
			c, _ := childArr.Get(i)
			if cd, ok := c.(*types.Document); ok {
				out.Append(cloneExecutionStage(cd, branch, false, false))
			}
		}
		d.Set("inputStages", out)
	}
	return d
}

// explainableCommands are the commands, by canonical name, that MongoDB 8.0
// can explain.
var explainableCommands = map[string]bool{
	"aggregate":     true,
	"count":         true,
	"delete":        true,
	"distinct":      true,
	"find":          true,
	"findAndModify": true,
	"update":        true,
}

// checkExplainable returns MongoDB's error for explaining the named command:
// CommandNotFound for an unknown command, IllegalOperation for one that cannot
// be explained. Authorization and execution both check it, so explain can
// only open a collection for a command whose privileges were checked.
func (h *Handler) checkExplainable(name string) error {
	cmd := h.commands[name]
	if cmd == nil {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrCommandNotFound,
			"Explain failed due to unknown command: "+name,
		)
	}
	if !explainableCommands[cmd.name] {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrIllegalOperation, "Cannot explain cmd: "+name)
	}
	return nil
}

func (h *Handler) MsgExplain(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = common.RejectUnknownFields(document, "verbosity"); err != nil {
		return nil, err
	}

	if inner, ok := must.NotFail(document.Get(document.Command())).(*types.Document); ok && inner.Len() > 0 {
		if err = h.checkExplainable(inner.Command()); err != nil {
			return nil, err
		}
	}

	params, err := common.GetExplainParams(document, h.L)
	if err != nil {
		return nil, err
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	serverInfo := must.NotFail(types.NewDocument(
		"host", hostname,
		"port", int32(27017),
		"version", version.Get().MongoDBVersion,
		"gitVersion", version.Get().Commit,
		"dumbodb", version.Get().Version,
	))

	cmd := params.Command
	cmd.Set("$db", params.DB)

	db, err := h.b.Database(params.DB)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid namespace specified '%s.%s'", params.DB, params.Collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, document.Command())
		}

		return nil, lazyerrors.Error(err)
	}

	coll, err := db.Collection(params.Collection)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
			msg := fmt.Sprintf("Invalid collection name: %s", params.Collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, document.Command())
		}

		return nil, lazyerrors.Error(err)
	}

	// If the explain target is a view, resolve it to its base collection and
	// derive the pushdown from the view's resolved pipeline (the same leading
	// $match viewSourceIterator pushes), so the plan reflects a base-collection
	// index used through the view instead of scanning the view namespace.
	var viaView bool
	if info, ierr := lookupCollectionInfo(connCtx, db, params.Collection); ierr == nil && info != nil && info.IsView {
		viaView = true
		baseName, _, rawStages, rerr := resolveViewChain(connCtx, db, info.Name, info.ViewOn, info.ViewPipeline, nil)
		if rerr != nil {
			return nil, rerr
		}

		if coll, err = db.Collection(baseName); err != nil {
			return nil, lazyerrors.Error(err)
		}

		params.Filter, _ = aggregations.GetPushdownQuery(rawStages)
		common.CanonicalizeFilter(params.Filter)
		params.Aggregate = false
		params.StagesDocs = nil
	}

	// A hint naming an index that does not exist is an error, matching MongoDB.
	if err = validateHintExists(connCtx, coll, params.Hint, document.Command()); err != nil {
		return nil, err
	}

	qp := new(backends.ExplainParams)

	if params.Aggregate {
		params.Filter, params.Sort = aggregations.GetPushdownQuery(params.StagesDocs)
		common.CanonicalizeFilter(params.Filter)
		params.Limit = pushdownLimit(params.StagesDocs)
	}

	if !h.DisablePushdown {
		qp.Filter = params.Filter
	}

	qp.Hint = params.Hint
	qp.Skip = params.Skip
	qp.Projection = params.Projection
	qp.Command = params.CommandName
	qp.DistinctKey = params.DistinctKey
	qp.Multi = params.Multi
	qp.Remove = params.Remove
	switch {
	case h.DisablePushdown:
	case params.CommandName == "find":
		qp.Fields, _ = findRootFields(&common.FindParams{Filter: params.Filter, Sort: params.Sort, Projection: params.Projection})
	case params.Aggregate && !viaView:
		qp.Fields, _ = pipelineRootFields(params.StagesDocs)
	}
	var afterCount []any
	if params.Aggregate && !viaView && !h.DisablePushdown {
		afterCount, qp.CountOnly = countingPipeline(params.StagesDocs)
	}

	var opCollation *types.Document
	if params.Command != nil {
		if cv, _ := params.Command.Get("collation"); cv != nil {
			opCollation, _ = cv.(*types.Document)
		}
	}
	effColl := h.effectiveCollation(connCtx, db, params.Collection, opCollation)
	qp.Collation = effColl
	qp.Collated = !collation.Parse(effColl).IsSimple()

	if params.Sort, err = common.ValidateSortDocument(params.Sort); err != nil {
		var pathErr *types.PathError
		if errors.As(err, &pathErr) && pathErr.Code() == types.ErrPathElementEmpty {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrPathContainsEmptyElement,
				"Empty field names in path are not allowed",
				document.Command(),
			)
		}

		return nil, err
	}

	var cList *backends.ListCollectionsResult

	collectionParam := backends.ListCollectionsParams{Name: params.Collection}
	if cList, err = db.ListCollections(connCtx, &collectionParam); err != nil {
		return nil, err
	}

	var cInfo backends.CollectionInfo

	if len(cList.Collections) > 0 {
		cInfo = cList.Collections[0]
	}

	// For Explain we always surface the requested sort to the backend so
	// the rendered plan tree can include a SORT stage. The historical
	// $natural-only gate was about pushdown to the Query path; pushdown
	// is not relevant for explain output.
	if !h.DisablePushdown {
		switch {
		case params.Sort.Len() == 0 && cInfo.Capped():
			qp.Sort = must.NotFail(types.NewDocument("$natural", int64(1)))
		case params.Sort.Len() > 0:
			qp.Sort = params.Sort
		}
	}

	// For Explain we always surface the requested limit to the backend
	// so the plan tree can include a LIMIT stage. The pushdown gate that
	// applied at the Query path (no filter / no sort / no skip) is
	// irrelevant to explain output.
	if !h.DisablePushdown {
		qp.Limit = params.Limit
	}

	res, err := coll.Explain(connCtx, qp)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	response := must.NotFail(types.NewDocument(
		"explainVersion", "1",
		"queryPlanner", res.QueryPlanner,
	))

	if params.Verbosity == "executionStats" || params.Verbosity == "allPlansExecution" {
		// Reflect the winning plan's stage in executionStages so the two
		// halves of the explain response agree on whether an index was used.
		execStage := "COLLSCAN"
		var winningPlanDoc *types.Document
		if wp, _ := res.QueryPlanner.Get("winningPlan"); wp != nil {
			if d, ok := wp.(*types.Document); ok {
				winningPlanDoc = d
				if s, _ := d.Get("stage"); s != nil {
					if str, ok := s.(string); ok && str != "" {
						execStage = str
					}
				}
			}
		}

		runCtx := connCtx
		if qp.CountOnly {
			runCtx = backends.WithSeekEachRange(connCtx)
		}
		run := h.runExplained(runCtx, coll, params, qp, viaView)
		if qp.CountOnly {
			run.returned = min(run.returned, 1)
		}

		executionStages := buildExecutionStages(winningPlanDoc, execStage, run)
		executionStats := must.NotFail(types.NewDocument(
			"executionSuccess", true,
			"nReturned", run.returned,
			"executionTimeMillis", int64(0),
			"totalKeysExamined", int32(run.keys),
			"totalDocsExamined", int32(run.docs),
			"executionStages", executionStages,
		))
		response.Set("executionStats", executionStats)
	}

	if qp.CountOnly {
		explainCountingGroup(response, afterCount)
	}

	if params.Aggregate && !viaView {
		explainLookups(connCtx, db, params.DB, params.StagesDocs, response, params.Verbosity != "queryPlanner")
	}

	if params.Verbosity == "allPlansExecution" {
		response.Set("allPlansExecution", types.MakeArray(0))
	}

	response.Set("command", cmd)
	response.Set("serverInfo", serverInfo)
	response.Set("ok", float64(1))

	return documentOpMsg(response)
}
