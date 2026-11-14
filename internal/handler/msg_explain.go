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
	"math"
	"os"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
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
	scanned int32
	// writes, when set, is what reaches a write's root stage from the stages
	// below it: a sorted single-document write sorts every match and writes
	// one.
	writes *int32
	// sorted, when set, is a find's SORT stage nReturned: the matches it keeps,
	// at most skip plus limit.
	sorted     *int32
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
	case qp.ZeroLimit:
		// A limit of 0 reads nothing.
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
	// A scan streaming index entries counts what it returned; the stage
	// above it filters them.
	if entries := examined.Entries(); entries > 0 {
		run.scanned = int32(entries)
	}
	return run
}

func (h *Handler) runExplainedCount(ctx context.Context, coll backends.Collection, params *common.ExplainParams, qp *backends.ExplainParams) explainRun {
	cp, err := common.GetCountParams(params.Command, h.L)
	if err != nil {
		return explainRun{}
	}
	common.CanonicalizeFilter(cp.Filter)
	cp.Collation = qp.Collation
	n, read, _ := h.execCountReading(ctx, coll, cp, collation.Parse(qp.Collation).Comparator())
	matched := n
	if read > 0 {
		// A FETCH returns every match it read, skipped ones included.
		matched = read
	}
	return explainRun{matched: matched, scanned: matched}
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
// would write, the way the write does, without writing them, under its hint,
// collation and sort. A single-document write stops at its first match unless
// it must sort every match first.
func runExplainedWrite(ctx context.Context, coll backends.Collection, qp *backends.ExplainParams) explainRun {
	var run explainRun
	single := qp.Command == "findAndModify" || !qp.Multi
	query := &backends.QueryParams{Filter: qp.Filter, Collated: qp.Collated, Collation: qp.Collation, Hint: qp.Hint}
	if backends.PushableSort(qp.Sort) {
		query.Sort = qp.Sort
	}
	run.matched = scanMatching(ctx, coll, query, qp, single)
	run.scanned = run.matched
	written := run.matched
	if single {
		written = min(written, 1)
	}
	run.writes = &written
	if qp.Command == "findAndModify" {
		run.returned = written
	}
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
	if backends.PushableSort(qp.Sort) {
		query.Sort = qp.Sort
	}
	var run explainRun
	run.matched = scanMatching(ctx, coll, query, qp, false)
	run.scanned = run.matched
	skip := int32(min(qp.Skip, math.MaxInt32))
	sorted := run.matched
	if limit > 0 {
		sorted = min(sorted, skip+limit)
	}
	run.sorted = &sorted
	run.returned = max(sorted-skip, 0)
	return run
}

// scanMatching runs query and returns how many of its documents match qp's
// filter, stopping at the first when single is set, or at qp's skip plus limit
// when the query's order is the requested one.
func scanMatching(ctx context.Context, coll backends.Collection, query *backends.QueryParams, qp *backends.ExplainParams, single bool) int32 {
	qres, err := coll.Query(ctx, query)
	if err != nil || qres == nil || qres.Iter == nil {
		return 0
	}
	defer qres.Iter.Close()

	limit := int32(max(qp.Limit, -qp.Limit))
	if limit > 0 {
		limit += int32(min(qp.Skip, math.MaxInt32-int64(limit)))
	}
	if single {
		limit = 1
	}
	stopAtLimit := limit > 0 && (qp.Sort.Len() == 0 || qres.Sorted)
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

// examinedDocs is the documents run examined, or for a plan that skips keys
// under its FETCH, only those it fetched past the skip.
func examinedDocs(plan *types.Document, run explainRun) int32 {
	for node := plan; node != nil; {
		child, _ := node.Get("inputStage")
		childDoc, _ := child.(*types.Document)
		if childDoc != nil && must.NotFail(node.Get("stage")) == "FETCH" && must.NotFail(childDoc.Get("stage")) == "SKIP" {
			return run.returned
		}
		node = childDoc
	}
	return int32(run.docs)
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

	// A SKIP under a FETCH drops keys before their documents are fetched, so
	// both pass up only what the stages above take.
	skipUnderFetch := stage == "FETCH" && childDoc != nil && must.NotFail(childDoc.Get("stage")) == "SKIP"
	var nReturned int32
	switch {
	case root:
		nReturned = run.returned
	case skipUnderFetch || stage == "SKIP" && !aboveMatch:
		nReturned = run.returned
	case childArr != nil:
		// An OR or SORT_MERGE returns each document its branches found once,
		// all fetched.
		nReturned = int32(run.outerDocs())
	case stage == "FETCH" || stage == "COLLSCAN" || stage == "CLUSTERED_IXSCAN":
		nReturned = run.matched
	case stage == "SORT" && aboveMatch && run.sorted != nil:
		nReturned = *run.sorted
	case childDoc == nil || !aboveMatch:
		nReturned = run.scanned
	case run.writes != nil:
		nReturned = *run.writes
	default:
		nReturned = run.returned
	}
	if stage == "FETCH" || stage == "COLLSCAN" || stage == "CLUSTERED_IXSCAN" {
		aboveMatch = false
	}
	d.Set("nReturned", nReturned)
	d.Set("executionTimeMillisEstimate", int64(0))
	switch stage {
	case "IXSCAN", "COUNT_SCAN", "DISTINCT_SCAN":
		d.Set("keysExamined", int32(run.keys))
	case "FETCH", "COLLSCAN", "CLUSTERED_IXSCAN":
		if skipUnderFetch {
			d.Set("docsExamined", run.returned)
		} else {
			d.Set("docsExamined", int32(run.outerDocs()))
		}
	case "EXPRESS_CLUSTERED_IXSCAN", "EXPRESS_UPDATE", "EXPRESS_DELETE":
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

func (h *Handler) MsgExplain(connCtx context.Context, msg *wire.OpMsg) (reply *wire.OpMsg, err error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = common.RejectUnknownFields(document, "verbosity"); err != nil {
		return nil, err
	}

	// mongod refuses explain in a transaction, which stays open.
	if conninfo.Get(connCtx).InTransaction() {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationNotSupportedInTransaction,
			"Cannot run 'explain' in a multi-document transaction.")
	}

	maxTimeMS, err := explainMaxTimeMS(document)
	if err != nil {
		return nil, err
	}
	if inner, ok := must.NotFail(document.Get(document.Command())).(*types.Document); ok && inner.Len() > 0 {
		if err = checkInnerMaxTimeMS(inner); err != nil {
			return nil, err
		}
	}
	connCtx, cancel := withMaxTimeMS(connCtx, maxTimeMS)
	defer cancel()
	defer func() {
		if err != nil && maxTimeMS != 0 && errors.Is(connCtx.Err(), context.DeadlineExceeded) {
			reply, err = nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrMaxTimeMSExpired, "operation exceeded time limit")
		}
	}()

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

	hintCollection := params.Collection
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
	var viaView, targetIsView bool
	if info, ierr := lookupCollectionInfo(connCtx, db, params.Collection); ierr == nil && info != nil && info.IsView {
		viaView, targetIsView = true, true
		baseName, _, rawStages, rerr := resolveViewChain(connCtx, db, info.Name, info.ViewOn, info.ViewPipeline, nil)
		if rerr != nil {
			return nil, rerr
		}

		hintCollection = baseName
		if coll, err = db.Collection(baseName); err != nil {
			return nil, lazyerrors.Error(err)
		}
		// A count's $count runs as a $group and a $project.
		if err = viewPipelineLength(aggregations.ExpandedLength(commandStages(params)) +
			aggregations.ExpandedLength(rawStages) - len(rawStages))(len(rawStages)); err != nil {
			return nil, err
		}

		switch {
		case onlyMatchStages(rawStages) && params.CommandName == "find":
			// The command's own filter applies after the view's.
			viewFilter, _ := aggregations.GetPushdownQuery(rawStages)
			params.Filter = andFilters(viewFilter, params.Filter)
			common.CanonicalizeFilter(params.Filter)
			params.Aggregate = false
			params.StagesDocs = nil
		case params.CommandName == "find" || params.CommandName == "count" || params.CommandName == "aggregate" ||
			params.CommandName == "distinct":
			// A view that reshapes documents runs before the command, so the
			// command is explained as an aggregate of the view's stages and
			// its own, as mongod does.
			combined := append(append([]any{}, rawStages...), commandStages(params)...)
			params.StagesDocs = mergeLeadingMatches(dropShapingBeforeCount(aggregations.MoveMatchesAhead(combined)))
			params.Aggregate = true
			params.CommandName = "aggregate"
			params.Filter, params.Sort, params.Skip, params.Limit = nil, nil, 0, 0
			viaView = false
		default:
			viewFilter, _ := aggregations.GetPushdownQuery(rawStages)
			params.Filter = andFilters(viewFilter, params.Filter)
			common.CanonicalizeFilter(params.Filter)
			params.Aggregate = false
			params.StagesDocs = nil
		}
	}

	// A hint naming an index that does not exist is an error, matching MongoDB.
	params.Hint = clusteredHint(params.Hint, recordedClusteredName(connCtx, db, params.Collection))
	if err = validateHintExists(connCtx, db, hintCollection, coll, params.Hint, document.Command()); err != nil {
		return nil, err
	}

	qp := new(backends.ExplainParams)
	var zeroLimit bool

	if params.Aggregate {
		// $skip and $limit stages move ahead as aggregate moves them. A match
		// may need to pass shaping stages to reach a $lookup it can pass, then
		// more shaping stages before that.
		optimized := stages.OptimizeSkipsAndLimits(aggregations.MoveMatchesAhead(stages.OptimizeSkipsAndLimits(params.StagesDocs, true)), true)
		moved := moveMatchesBeforeLookups(aggregations.MoveMatchesAhead(moveWindowBeforeLookups(optimized)))
		params.StagesDocs = mergeLeadingMatches(dropShapingBeforeCount(aggregations.MoveMatchesAhead(moved)))
		params.Filter, params.Sort = aggregations.GetPushdownQuery(params.StagesDocs)
		common.CanonicalizeFilter(params.Filter)
		var limited bool
		params.Skip, params.Limit, limited = pushdownWindow(params.StagesDocs)
		zeroLimit = limited && params.Limit == 0
		params.Projection = absorbedProjection(params.StagesDocs)
	}

	if !h.DisablePushdown {
		qp.Filter = params.Filter
	}

	qp.Hint = params.Hint
	qp.Skip = params.Skip
	qp.ZeroLimit = zeroLimit
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
	if opCollation == nil {
		opCollation = params.StatementCollation
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

	var pipelineDocs []*types.Document
	var sbe sbePlan
	if params.Aggregate && !viaView {
		pipelineDocs = explainedStageDocs(params.StagesDocs)
		if info, ierr := lookupCollectionInfo(connCtx, db, hintCollection); pipelineDocs != nil && ierr == nil && info != nil {
			sbe = classifySBE(connCtx, db, pipelineDocs, effColl, !targetIsView, viewChecker(connCtx, db))
		}
	}

	res, err := coll.Explain(connCtx, qp)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	response := must.NotFail(types.NewDocument(
		"explainVersion", explainVersion(params, viaView),
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
		if sbe.runsInSBE() {
			runCtx = backends.WithSBEScan(runCtx)
		}
		run := h.runExplained(runCtx, coll, params, qp, viaView)
		// A counting GROUP returns one document, the count.
		if qp.CountOnly || (viaView && params.CommandName == "count") {
			run.returned = min(run.returned, 1)
		}

		executionStages := buildExecutionStages(winningPlanDoc, execStage, run)
		executionStats := must.NotFail(types.NewDocument(
			"executionSuccess", true,
			"nReturned", run.returned,
			"executionTimeMillis", int64(0),
			"totalKeysExamined", int32(run.keys),
			"totalDocsExamined", examinedDocs(winningPlanDoc, run),
			"executionStages", executionStages,
		))
		response.Set("executionStats", executionStats)
	}

	if qp.CountOnly {
		explainCountingGroup(response, afterCount)
	}

	if params.Aggregate && !viaView && !h.explainStatsStages(connCtx, db, params.DB, params.Collection, coll, params.StagesDocs, response,
		params.Verbosity != "queryPlanner") {
		if !qp.CountOnly && pipelineDocs != nil {
			explainPipelineStages(connCtx, db, coll, qp, params.DB, pipelineDocs, sbe, response, params.Verbosity != "queryPlanner")
		}
	}

	if params.Verbosity == "allPlansExecution" {
		response.Set("allPlansExecution", types.MakeArray(0))
	}

	response.Set("command", cmd)
	response.Set("serverInfo", serverInfo)
	response.Set("ok", float64(1))

	return documentOpMsg(response)
}

// onlyMatchStages reports whether a view pipeline holds only $match stages.
func onlyMatchStages(stages []any) bool {
	for _, stage := range stages {
		d, ok := stage.(*types.Document)
		if !ok || d.Command() != "$match" {
			return false
		}
	}
	return true
}

// dropShapingBeforeCount removes a $project directly before a $count, which
// mongod runs inside the counting plan. A $set, $addFields or $unset there
// stays a stage of its own, as on mongod.
func dropShapingBeforeCount(stages []any) []any {
	out := make([]any, 0, len(stages))
	for i, stage := range stages {
		d, _ := stage.(*types.Document)
		if d != nil && d.Len() == 1 && d.Command() == "$project" && i+1 < len(stages) && isCountStage(stages[i+1]) {
			continue
		}
		out = append(out, stage)
	}
	return out
}

func isCountStage(stage any) bool {
	d, _ := stage.(*types.Document)
	return d != nil && d.Len() == 1 && d.Command() == "$count"
}

// mergeLeadingMatches joins a pipeline's leading $match stages into one, as
// mongod coalesces adjacent $match stages before pushing the result down.
func mergeLeadingMatches(stages []any) []any {
	var merged *types.Document
	i := 0
	for ; i < len(stages); i++ {
		d, ok := stages[i].(*types.Document)
		if !ok || d.Command() != "$match" {
			break
		}
		filter, ok := must.NotFail(d.Get("$match")).(*types.Document)
		if !ok {
			break
		}
		merged = andFilters(merged, filter)
	}
	if i < 2 {
		return stages
	}
	return append([]any{must.NotFail(types.NewDocument("$match", merged))}, stages[i:]...)
}

// commandStages are the aggregation stages a find, count, distinct or
// aggregate applies to a view's documents.
func commandStages(params *common.ExplainParams) []any {
	if params.CommandName == "aggregate" {
		return params.StagesDocs
	}
	var stages []any
	if params.Filter.Len() > 0 {
		stages = append(stages, must.NotFail(types.NewDocument("$match", params.Filter)))
	}
	if params.CommandName == "find" && params.Sort.Len() > 0 {
		stages = append(stages, must.NotFail(types.NewDocument("$sort", params.Sort)))
	}
	if params.Skip > 0 {
		stages = append(stages, must.NotFail(types.NewDocument("$skip", params.Skip)))
	}
	if params.Limit != 0 {
		stages = append(stages, must.NotFail(types.NewDocument("$limit", max(params.Limit, -params.Limit))))
	}
	switch params.CommandName {
	case "count":
		stages = append(stages, must.NotFail(types.NewDocument("$count", "count")))
	case "distinct":
		stages = append(stages, distinctStages(params.DistinctKey)...)
	}
	return stages
}

// distinctStages are the stages mongod runs for a distinct on a view: each
// value at key, arrays unwound, gathered into one set.
func distinctStages(key string) []any {
	const field = "_internalUnwoundArray"
	return []any{
		must.NotFail(types.NewDocument("$replaceRoot", must.NotFail(types.NewDocument(
			"newRoot", must.NotFail(types.NewDocument(field, "$"+key)),
		)))),
		must.NotFail(types.NewDocument("$unwind", must.NotFail(types.NewDocument(
			"path", "$"+field, "preserveNullAndEmptyArrays", true,
		)))),
		must.NotFail(types.NewDocument("$group", must.NotFail(types.NewDocument(
			"_id", types.Null, "distinct", must.NotFail(types.NewDocument("$addToSet", "$"+field)),
		)))),
	}
}

// andFilters returns a filter matching both a and b, either of which may be
// empty.
func andFilters(a, b *types.Document) *types.Document {
	switch {
	case a.Len() == 0:
		return b
	case b.Len() == 0:
		return a
	}
	return must.NotFail(types.NewDocument("$and", must.NotFail(types.NewArray(a, b))))
}

// explainVersion is "2" where MongoDB runs the command as a pipeline: a count
// on a view.
func explainVersion(params *common.ExplainParams, viaView bool) string {
	if viaView && params.CommandName == "count" {
		return "2"
	}
	return "1"
}

// checkInnerMaxTimeMS returns mongod's error for the maxTimeMS of an
// explained command: find, aggregate and count check only its type and sign,
// distinct reads it as it does unexplained.
func checkInnerMaxTimeMS(inner *types.Document) error {
	v, _ := inner.Get("maxTimeMS")
	switch inner.Command() {
	case "find":
		return handlerparams.TypedMaxTimeMS(v, "FindCommandRequest")
	case "aggregate", "count":
		return handlerparams.TypedMaxTimeMS(v, inner.Command())
	case "distinct":
		if v == nil {
			return nil
		}
		_, err := handlerparams.CommandMaxTimeMS(v)
		return err
	}
	return nil
}

// explainMaxTimeMS returns the maxTimeMS beside the explained command, 0 when
// unset, read as count and distinct read theirs (handlerparams.CommandMaxTimeMS).
func explainMaxTimeMS(document *types.Document) (int64, error) {
	v, _ := document.Get("maxTimeMS")
	return handlerparams.CommandMaxTimeMS(v)
}

// pushdownWindow returns the skip and limit the query plan absorbs from the
// run of $skip and $limit stages directly after its leading $match and
// $sort, combined as mongod combines them: a skip after a limit takes what
// it can from it. limited reports whether a limit applies, as it may be 0.
func pushdownWindow(stages []any) (skip, limit int64, limited bool) {
	i := 0
	for seen := map[string]bool{}; i < 2; i++ {
		d := stageDocAt(stages, i)
		if d == nil || d.Command() != "$match" && d.Command() != "$sort" || seen[d.Command()] {
			break
		}
		seen[d.Command()] = true
	}
	for ; ; i++ {
		d := stageDocAt(stages, i)
		if d == nil {
			return skip, limit, limited
		}
		switch d.Command() {
		case "$skip":
			n, _ := stageInt64(d, "$skip", common.GetSkipStageParam)
			if limited {
				n = min(n, limit)
				limit -= n
			}
			skip += n
		case "$limit":
			n, _ := stageInt64(d, "$limit", common.GetLimitStageParam)
			if !limited || n < limit {
				limit = n
			}
			limited = true
		default:
			return skip, limit, limited
		}
	}
}

// absorbedProjection returns the projection of the $project or $unset the
// query plan absorbs from stages, when it only includes or excludes fields,
// or nil.
func absorbedProjection(stages []any) *types.Document {
	docs := make([]*types.Document, 0, len(stages))
	for _, s := range stages {
		d, _ := s.(*types.Document)
		if d == nil || d.Len() != 1 {
			return nil
		}
		docs = append(docs, d)
	}
	n := queryAbsorbedStages(docs)
	if n == 0 || n != len(docs) {
		return nil
	}
	last := docs[n-1]
	switch last.Command() {
	case "$unset":
		return must.NotFail(reportedStage(last).Get("$project")).(*types.Document)
	case "$project":
		spec, _ := must.NotFail(last.Get("$project")).(*types.Document)
		if spec == nil {
			return nil
		}
		for _, k := range spec.Keys() {
			switch must.NotFail(spec.Get(k)).(type) {
			case bool, int32, int64, float64:
			default:
				return nil
			}
		}
		return spec
	}
	return nil
}
