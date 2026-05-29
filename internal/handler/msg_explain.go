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
	"strings"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/version"
	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// countExplainExecution runs a counting pass against the collection
// for executionStats verbosity, returning (nReturned,
// totalDocsExamined, totalKeysExamined).
//
// Counting is best-effort: any backend error returns zeros so the
// caller still produces a structurally valid executionStats document.
// For find/aggregate the implementation iterates coll.Query's result
// (each yielded doc counts as one examined doc) and re-applies the
// filter at the handler level to compute nReturned. When the winning
// plan tree contains an IXSCAN node every examined doc was reached
// via the index, so totalKeysExamined = totalDocsExamined.
//
// count and distinct commands return zeros today; their dedicated
// stat surfaces (COUNT_SCAN, DISTINCT_SCAN counters) are a follow-up.
func countExplainExecution(ctx context.Context, coll backends.Collection, qp *backends.ExplainParams, winningPlan *types.Document) (nReturned, totalDocsExamined, totalKeysExamined int32) {
	if qp.Command != "find" && qp.Command != "aggregate" {
		return 0, 0, 0
	}
	usesIndex := planContainsIndexScan(winningPlan)

	qres, err := coll.Query(ctx, &backends.QueryParams{Filter: qp.Filter})
	if err != nil || qres == nil || qres.Iter == nil {
		return 0, 0, 0
	}
	defer qres.Iter.Close()

	for {
		_, doc, err := qres.Iter.Next()
		if err != nil {
			break
		}
		totalDocsExamined++
		if qp.Filter == nil || qp.Filter.Len() == 0 {
			nReturned++
			continue
		}
		match, ferr := common.FilterDocument(doc, qp.Filter)
		if ferr == nil && match {
			nReturned++
		}
	}

	if usesIndex {
		totalKeysExamined = totalDocsExamined
	}
	return
}

// buildExecutionStages clones the winningPlan tree shape into the
// executionStages document expected by the executionStats verbosity.
// At the root the stage's nReturned reflects the actual result count.
// Per-stage row flow (nReturned, advanced, ...) at intermediate
// levels is left at zero -- enough to satisfy stage/indexName parity
// without implementing full row-counting instrumentation.
func buildExecutionStages(plan *types.Document, rootStage string, nReturned int32) *types.Document {
	if plan == nil {
		return must.NotFail(types.NewDocument(
			"stage", rootStage,
			"nReturned", nReturned,
			"executionTimeMillisEstimate", int64(0),
		))
	}
	return cloneExecutionStage(plan, nReturned)
}

func cloneExecutionStage(node *types.Document, nReturned int32) *types.Document {
	d := must.NotFail(types.NewDocument())
	if s, _ := node.Get("stage"); s != nil {
		d.Set("stage", s)
	}
	d.Set("nReturned", nReturned)
	d.Set("executionTimeMillisEstimate", int64(0))
	if v, _ := node.Get("indexName"); v != nil {
		d.Set("indexName", v)
	}
	if v, _ := node.Get("keyPattern"); v != nil {
		d.Set("keyPattern", v)
	}
	if v, _ := node.Get("inputStage"); v != nil {
		if child, ok := v.(*types.Document); ok {
			d.Set("inputStage", cloneExecutionStage(child, 0))
		}
	}
	if v, _ := node.Get("inputStages"); v != nil {
		if arr, ok := v.(*types.Array); ok {
			out := types.MakeArray(arr.Len())
			for i := 0; i < arr.Len(); i++ {
				c, _ := arr.Get(i)
				if child, ok := c.(*types.Document); ok {
					out.Append(cloneExecutionStage(child, 0))
				}
			}
			d.Set("inputStages", out)
		}
	}
	return d
}

// planContainsIndexScan walks the winningPlan tree looking for any
// node whose stage is IXSCAN, COUNT_SCAN, or DISTINCT_SCAN. Returns
// true for both inputStage (single-child) and inputStages (branching)
// nodes so OR/AND multi-index plans count correctly.
func planContainsIndexScan(plan *types.Document) bool {
	if plan == nil {
		return false
	}
	if s, _ := plan.Get("stage"); s != nil {
		if str, ok := s.(string); ok {
			switch str {
			case "IXSCAN", "COUNT_SCAN", "DISTINCT_SCAN":
				return true
			}
		}
	}
	if v, _ := plan.Get("inputStage"); v != nil {
		if child, ok := v.(*types.Document); ok && planContainsIndexScan(child) {
			return true
		}
	}
	if v, _ := plan.Get("inputStages"); v != nil {
		if arr, ok := v.(*types.Array); ok {
			for i := 0; i < arr.Len(); i++ {
				c, _ := arr.Get(i)
				if child, ok := c.(*types.Document); ok && planContainsIndexScan(child) {
					return true
				}
			}
		}
	}
	return false
}

// MsgExplain implements `explain` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgExplain(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
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

		// our extensions
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

	qp := new(backends.ExplainParams)

	if params.Aggregate {
		params.Filter, params.Sort = aggregations.GetPushdownQuery(params.StagesDocs)
	}

	if !h.DisablePushdown {
		qp.Filter = params.Filter
	}

	qp.Hint = params.Hint
	qp.Skip = params.Skip
	qp.Projection = params.Projection
	qp.Command = params.CommandName
	qp.DistinctKey = params.DistinctKey

	if !h.EnableNestedPushdown && params.Filter != nil {
		qp.Filter = params.Filter.DeepCopy()

		for _, k := range qp.Filter.Keys() {
			if !strings.ContainsRune(k, '.') {
				continue
			}

			qp.Filter.Remove(k)
		}
	}

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

	// Add executionStats for "executionStats" and "allPlansExecution" verbosity.
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

		nReturned, totalDocsExamined, totalKeysExamined := countExplainExecution(connCtx, coll, qp, winningPlanDoc)

		// executionStages mirrors the winningPlan tree's shape so
		// stage / indexName / keyPattern parity holds at every level.
		// nReturned is reported at the root only -- per-stage row-flow
		// stats are a follow-up.
		executionStages := buildExecutionStages(winningPlanDoc, execStage, nReturned)
		executionStats := must.NotFail(types.NewDocument(
			"executionSuccess", true,
			"nReturned", nReturned,
			"executionTimeMillis", int64(0),
			"totalKeysExamined", totalKeysExamined,
			"totalDocsExamined", totalDocsExamined,
			"executionStages", executionStages,
		))
		response.Set("executionStats", executionStats)
	}

	// Add allPlansExecution for "allPlansExecution" verbosity.
	if params.Verbosity == "allPlansExecution" {
		response.Set("allPlansExecution", types.MakeArray(0))
	}

	response.Set("command", cmd)
	response.Set("serverInfo", serverInfo)
	response.Set("ok", float64(1))

	return documentOpMsg(response)
}
