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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/FerretDB/wire"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/clientconn/cursor"
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

// MsgAggregate implements `aggregate` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgAggregate(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = common.RejectUnknownFields(document,
		"pipeline", "cursor", "allowDiskUse", "bypassDocumentValidation",
		"collation", "hint", "let", "explain",
	); err != nil {
		return nil, err
	}

	common.Ignored(document, h.L, "lsid")

	if err = common.Unimplemented(document, "explain", "let"); err != nil {
		return nil, err
	}

	var collCmp *collation.Comparator
	if cv, _ := document.Get("collation"); cv != nil {
		if cd, ok := cv.(*types.Document); ok {
			collCmp = collation.Parse(cd).Comparator()
		}
	}

	common.Ignored(
		document, h.L,
		"allowDiskUse", "bypassDocumentValidation", "readConcern", "hint", "comment", "writeConcern",
	)

	var dbName string

	if dbName, err = common.GetRequiredParam[string](document, "$db"); err != nil {
		return nil, err
	}
	connCtx = withDBName(connCtx, dbName)

	collectionParam, err := document.Get(document.Command())
	if err != nil {
		return nil, err
	}

	var ok bool
	var cName string
	var dbLevel bool

	switch v := collectionParam.(type) {
	case string:
		cName, ok = v, true
	case int32:
		dbLevel, ok = v == 1, v == 1
	case int64:
		dbLevel, ok = v == 1, v == 1
	case float64:
		dbLevel, ok = v == 1, v == 1
	}
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"Invalid command format: the 'aggregate' field must specify a collection name or 1",
			document.Command(),
		)
	}

	if dbLevel {
		return h.aggregateDatabase(connCtx, document, dbName)
	}

	// Validate rootish before backend access so invalid forms (HEAD, reflog, range)
	// return OperationFailed (96) rather than silently succeeding or returning
	// InvalidNamespace (73) from MongoDB's own namespace check.
	if _, _, _, err := branchFromDBName(dbName); err != nil {
		return nil, err
	}

	db, err := h.b.Database(dbName)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid namespace specified '%s.%s'", dbName, cName)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, document.Command())
		}

		return nil, lazyerrors.Error(err)
	}

	c, err := db.Collection(cName)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
			msg := fmt.Sprintf("Invalid collection name: %s", cName)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, document.Command())
		}

		return nil, lazyerrors.Error(err)
	}

	if target, lookupErr := lookupCollectionInfo(connCtx, db, cName); lookupErr == nil && target != nil && target.IsView {
		opCollation, _ := document.Get("collation")
		opCollationDoc, _ := opCollation.(*types.Document)
		viewCollation, vErr := viewReadCollation(opCollationDoc, target.Collation)
		if vErr != nil {
			return nil, vErr
		}
		collCmp = collation.Parse(viewCollation).Comparator()
	}

	owner := conninfo.Get(connCtx).SessionPrincipal()

	v, _ := document.Get("maxTimeMS")
	if v == nil {
		v = int64(0)
	}

	maxTimeMS, err := handlerparams.FindMaxTimeMS(v, "aggregate")
	if err != nil {
		return nil, err
	}

	pipeline, err := common.GetRequiredParam[*types.Array](document, "pipeline")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"'pipeline' option must be specified as an array",
			document.Command(),
		)
	}
	if err = aggregations.CheckPipelineLength(pipeline.Len()); err != nil {
		return nil, err
	}
	pipeline = stages.MoveSkipsBeforeTransforms(operators.BindNowInPipeline(pipeline, operators.CommandNow()))

	aggregationStages := aggregations.MoveMatchesAhead(must.NotFail(iterator.ConsumeValues(pipeline.Iterator())))
	if err = aggregations.CheckExpandedPipelineLength(aggregations.ExpandedLength(aggregationStages)); err != nil {
		return nil, err
	}
	stagesDocuments := make([]aggregations.Stage, 0, len(aggregationStages))
	collStatsDocuments := make([]aggregations.Stage, 0, len(aggregationStages))
	stagesSpecs := make([]*types.Document, 0, len(aggregationStages))

	for i, v := range aggregationStages {
		var d *types.Document

		if d, ok = v.(*types.Document); !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"Each element of the 'pipeline' array must be an object",
				document.Command(),
			)
		}

		if err = documentsNotFirst(d, i); err != nil {
			return nil, err
		}
		if name := d.Command(); i > 0 && (name == "$listCatalog" || name == "$querySettings") {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCollStatsIsNotFirstStage,
				name+" is only valid as the first stage in a pipeline", document.Command())
		}

		var s aggregations.Stage

		switch d.Command() {
		case "$lookup", "$graphLookup", "$unionWith", "$facet":
			if err = checkJoinedViewLength(connCtx, db, d); err != nil {
				return nil, err
			}
			s, err = newForeignCollectionStage(d, db)

		case "$out":
			// $out requires database write access to replace a collection.
			outWriter := makeOutWriter(h.b, dbName)
			s, err = stages.NewOutStage(d, dbName, outWriter)

		case "$merge":
			// $merge requires database read/write access to merge into a collection.
			mergeWriter := makeMergeWriter(h.b, dbName)
			if s, err = stages.NewMergeStage(d, dbName, mergeWriter); err == nil {
				err = checkMergeOnIndex(connCtx, h.b, s)
			}

		case "$indexStats":
			// $indexStats requires collection access to retrieve index metadata.
			s, err = stages.NewIndexStatsStage(d, connCtx, c, h.TCPHost, func(indexes []backends.IndexInfo) []backends.IndexInfo {
				return clientVisibleIndexes(connCtx, db, cName, indexes)
			})

		case "$planCacheStats":
			s, err = stages.NewPlanCacheStatsStage(connCtx, c, dbName+"."+cName, h.TCPHost)

		case "$match":
			// A stage doc with more than one field is invalid; defer to NewStage
			// for that error rather than the single-field NewMatchStage.
			if d.Len() == 1 {
				s, err = stages.NewMatchStage(d, collCmp, stages.SeesStoredDocuments(aggregationStages, i))
			} else {
				s, err = stages.NewStage(d)
			}

		case "$group":
			s, err = stages.NewGroupStage(d, collCmp)

		case "$sort":
			// Coalesce a following $limit (or $skip + $limit) into a top-K bound
			// so $sort holds only the needed documents instead of the whole input.
			s, err = stages.NewSortStage(d, sortLimitBound(aggregationStages, i), collCmp)

		default:
			s, err = stages.NewStage(d)
		}

		if err != nil {
			return nil, err
		}

		switch d.Command() {
		case "$collStats":
			if i > 0 {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrCollStatsIsNotFirstStage,
					"$collStats is only valid as the first stage in a pipeline",
					document.Command(),
				)
			}

			collStatsDocuments = append(collStatsDocuments, s)
		default:
			stagesDocuments = append(stagesDocuments, s)
			stagesSpecs = append(stagesSpecs, d)
			collStatsDocuments = append(collStatsDocuments, s) // It's possible to apply any stage after $collStats stage
		}
	}

	stages.MarkLookupsUnwound(stagesDocuments, stagesSpecs)

	if err = common.ValidatePipelineMetadata(aggregationStages); err != nil {
		return nil, err
	}

	// validate cursor after validating pipeline stages to keep compatibility
	v, _ = document.Get("cursor")
	if v == nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"The 'cursor' option is required, except for aggregate with the explain argument",
			document.Command(),
		)
	}

	cursorDoc, ok := v.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"cursor field must be missing or an object",
			document.Command(),
		)
	}

	v, _ = cursorDoc.Get("batchSize")
	if v == nil {
		v = int32(101)
	}

	batchSize, err := handlerparams.GetValidatedNumberParamWithMinValue("cursor", "batchSize", v, 0)
	if err != nil {
		return nil, err
	}

	ctx := connCtx
	if common.MentionsRecordIDMeta(pipeline) {
		ctx = backends.WithRecordKeys(ctx)
	}
	cancel := func() {}

	if maxTimeMS != 0 {
		findDone := make(chan struct{})
		defer close(findDone)

		ctx, cancel = context.WithCancel(ctx)

		go func() {
			t := time.NewTimer(time.Duration(maxTimeMS) * time.Millisecond)
			defer t.Stop()

			select {
			case <-t.C:
				cancel()
			case <-findDone:
			}
		}()
	}

	closer := iterator.NewMultiCloser(iterator.CloserFunc(cancel))

	var iter iterator.Interface[struct{}, *types.Document]

	if len(collStatsDocuments) == len(stagesDocuments) {
		filter, sort := aggregations.GetPushdownQuery(aggregationStages)
		common.CanonicalizeFilter(filter)

		// only documents stages or no stages - fetch documents from the DB and apply stages to them
		qp := new(backends.QueryParams)

		// Under a collation the backend must return a superset: its byte-exact
		// narrowing would drop string matches that only the collator equates,
		// before the $match stage re-checks them.
		qp.Collated = collCmp != nil

		if !h.DisablePushdown {
			qp.Filter = filter
		}

		if sort, err = common.ValidateSortDocument(sort); err != nil {
			closer.Close()

			var pathErr *types.PathError
			if errors.As(err, &pathErr) && pathErr.Code() == types.ErrPathElementEmpty {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrPathContainsEmptyElement,
					"FieldPath field names may not be empty strings.",
					document.Command(),
				)
			}

			return nil, err
		}

		var cList *backends.ListCollectionsResult

		collectionParam := backends.ListCollectionsParams{Name: cName}
		if cList, err = db.ListCollections(ctx, &collectionParam); err != nil {
			closer.Close()
			return nil, handleMaxTimeMSError(err, maxTimeMS, "aggregate")
		}

		var cInfo backends.CollectionInfo

		if len(cList.Collections) > 0 {
			cInfo = cList.Collections[0]
		}

		if hint, _ := document.Get("hint"); hint != nil {
			hint = clusteredHint(hint, cInfo.ClusteredIndexName)
			document.Set("hint", hint)
			err = checkHintType(hint)
			// Led by $match, mongod plans the read as a query, which refuses a
			// compound $natural hint and overlooks a bad $natural value.
			ledByMatch := false
			if pipeline.Len() > 0 {
				first, _ := must.NotFail(pipeline.Get(0)).(*types.Document)
				ledByMatch = first != nil && first.Command() == "$match"
			}
			if err == nil && ledByMatch {
				err = checkCompoundNaturalHint(hint, "aggregate")
			}
			if err == nil && !cInfo.IsView && !(ledByMatch && backends.HintIsNatural(hint)) {
				err = validateHintExists(ctx, db, cName, c, hint, "aggregate")
			}
			if err == nil && cInfo.IsView {
				err = validateViewHint(ctx, db, &cInfo, hint, "aggregate", ledByMatch)
			}
			if err != nil {
				closer.Close()
				return nil, err
			}
		}

		// Fast path: drivers (notably the official Go driver) implement
		// CountDocuments by issuing an aggregate pipeline of the form
		// [{$match: <filter>}, optional $skip/$limit, {$group: {_id: <c>, n: {$sum: 1}}}].
		// When the target is a real collection (not a view), the backend can
		// satisfy this from a covering index (filtered) or O(1) tree count
		// (unfiltered) and skip the full scan + group accumulator entirely.
		// Filtered counts where the backend cannot answer from an index fall
		// through to the regular pipeline path.
		// The count shortcut answers via a byte-exact indexed/tree count that
		// cannot honor a collation, so skip it when one is set and let the
		// regular pipeline path re-check $match through the collator (mirrors
		// the count command). workspace-4jf.
		hint, _ := document.Get("hint")
		if !cInfo.IsView && collCmp == nil && (hint == nil || backends.HintIsNatural(hint)) {
			if info, ok := tryCountAggregateShortcut(aggregationStages); ok {
				if info.filter != nil {
					if err := common.ValidateFilter(info.filter, nil); err != nil {
						closer.Close()
						return nil, err
					}
				}
				countRes, cerr := c.Count(ctx, &backends.CountParams{Filter: info.filter})
				if cerr != nil {
					closer.Close()
					return nil, handleMaxTimeMSError(cerr, maxTimeMS, "aggregate")
				}

				// Skip to the regular pipeline if the backend declined a
				// filtered count it couldn't answer from an index.
				if info.filter == nil || countRes.Filtered {
					n := countRes.Count
					if info.skip > 0 {
						n -= info.skip
						if n < 0 {
							n = 0
						}
					}
					if info.limit >= 0 && n > info.limit {
						n = info.limit
					}

					closer.Close()

					// Match the type $sum returns: int32 when the value fits, else int64.
					var nVal any
					if n <= math.MaxInt32 && n >= math.MinInt32 {
						nVal = int32(n)
					} else {
						nVal = n
					}

					// $group emits no group when no document reaches it.
					firstBatch := types.MakeArray(1)
					if n > 0 {
						firstBatch.Append(must.NotFail(types.NewDocument("_id", info.idValue, info.outField, nVal)))
					}

					return documentOpMsg(
						must.NotFail(types.NewDocument(
							"cursor", must.NotFail(types.NewDocument(
								"firstBatch", firstBatch,
								"id", int64(0),
								"ns", dbName+"."+cName,
							)),
							"ok", float64(1),
						)),
					)
				}
			}
		}

		if !cInfo.IsView && !h.DisablePushdown {
			if fields, ok := pipelineRootFields(aggregationStages); ok {
				qp.Fields = fields
			}
		}

		// The stages the pushed filter and limit are drawn from: the user's,
		// or for a view, the view's then the user's, as they run on the base.
		pushdownStages := aggregationStages
		collectionExists := len(cList.Collections) > 0
		isView := cInfo.IsView
		if isView {
			view := cList.Collections[0]
			baseCollection, viewStages, rawViewStages, vErr := resolveViewChain(ctx, db, view.Name, view.ViewOn, view.ViewPipeline, collCmp)
			if vErr != nil {
				closer.Close()
				return nil, handleMaxTimeMSError(vErr, maxTimeMS, "aggregate")
			}
			if err = viewPipelineLength(aggregations.ExpandedLength(aggregationStages) +
				aggregations.ExpandedLength(rawViewStages) - len(rawViewStages))(len(rawViewStages)); err != nil {
				closer.Close()
				return nil, err
			}
			srcInfo, srcErr := lookupCollectionInfo(ctx, db, baseCollection)
			if srcErr != nil {
				closer.Close()
				return nil, handleMaxTimeMSError(srcErr, maxTimeMS, "aggregate")
			}
			cInfo = backends.CollectionInfo{}
			collectionExists = srcInfo != nil
			if srcInfo != nil {
				cInfo = *srcInfo
			}
			c, err = db.Collection(baseCollection)
			if err != nil {
				closer.Close()
				return nil, lazyerrors.Error(err)
			}
			stagesDocuments = append(viewStages, stagesDocuments...)
			pushdownStages = aggregations.MoveMatchesAhead(append(append([]any{}, rawViewStages...), aggregationStages...))
			if !h.DisablePushdown {
				qp.Filter, _ = aggregations.GetPushdownQuery(pushdownStages)
				common.CanonicalizeFilter(qp.Filter)
			}
			specs := make([]*types.Document, 0, len(rawViewStages)+len(stagesSpecs))
			for _, v := range rawViewStages {
				d, _ := v.(*types.Document)
				specs = append(specs, d)
			}
			stagesSpecs = append(specs, stagesSpecs...)
			stages.MarkLookupsUnwound(stagesDocuments, stagesSpecs)
		}

		collectionIsView := func(name string) bool {
			info, err := lookupCollectionInfo(ctx, db, name)
			return err == nil && info != nil && info.IsView
		}
		// mongod answers a missing collection with an empty plan, and a
		// leading equality on _id with its express plan; neither runs in SBE.
		if collectionExists && !stages.IsExpressMatch(stagesSpecs) {
			if !isView {
				stages.MarkSBELookups(stagesDocuments, stagesSpecs, collectionIsView)
			}
			stages.WrapSBEStages(stagesDocuments, stages.SBEPrefixLen(stagesSpecs, collectionIsView))
		}

		switch {
		case h.DisablePushdown:
			// Pushdown disabled
		case sort.Len() == 0 && cInfo.Capped():
			// Pushdown default recordID sorting for capped collections
			qp.Sort = must.NotFail(types.NewDocument("$natural", int64(1)))
		case sort.Len() == 1 && sort.Keys()[0] == "$natural":
			qp.Sort = sort
		case sort.Len() == 1 && !isView && collCmp == nil:
			// The backend may walk an index in sort order, making the $sort
			// stage a no-op.
			qp.Sort = sort
		}

		var sortStage aggregations.Stage
		if qp.Sort != nil && qp.Sort.Keys()[0] != "$natural" {
			sortStage = stagesDocuments[pushdownSortStageIndex(aggregationStages)]
		}
		qp.LimitHint = pushdownLimit(pushdownStages)

		var hintOrder, hintSortFilter *types.Document
		if hint, _ := document.Get("hint"); hint != nil {
			qp.Hint = hint
			if qp.Sort == nil {
				if hintOrder, hintSortFilter, _, err = hintedIndexOrder(ctx, c, hint, qp.Filter, qp.Collation, 0); err != nil {
					closer.Close()
					return nil, handleMaxTimeMSError(err, maxTimeMS, "aggregate")
				}
			}
			if hintOrder != nil {
				// The backend reads ascending, so a limit would stop it early,
				// and the sort reads the index's keys, which the stages'
				// fields may not include.
				qp.LimitHint = 0
				if qp.Fields != nil {
					for _, k := range hintOrder.Keys() {
						root, _, _ := strings.Cut(k, ".")
						if !slices.Contains(qp.Fields, root) {
							qp.Fields = append(qp.Fields, root)
						}
					}
				}
			}
		}

		if runsInSBE(aggregationStages) {
			ctx = backends.WithSBEScan(ctx)
		}
		iter, err = processStagesDocuments(ctx, closer, &stagesDocumentsParams{
			c: c, qp: qp, stages: stagesDocuments, sortStage: sortStage, hintOrder: hintOrder, hintSortFilter: hintSortFilter, cmp: collCmp,
		})
	} else {
		statistics := stages.GetStatistics(collStatsDocuments)

		iter, err = processStagesStats(ctx, closer, &stagesStatsParams{
			c, db, dbName, cName, statistics, collStatsDocuments, h.TCPHost, false,
		})
	}

	if err != nil {
		closer.Close()
		return nil, handleMaxTimeMSError(err, maxTimeMS, "aggregate")
	}

	closer.Add(iter)

	cursor := h.cursors.NewCursor(ctx, iterator.WithClose(iter, closer.Close), &cursor.NewParams{
		DB:         dbName,
		Collection: cName,
		Owner:      owner,
		Type:       cursor.Normal,
	})

	cursorID := cursor.ID

	docs, done, err := cursor.NextBatch(int(batchSize), types.MaxDocumentLen)
	if err != nil {
		h.cursors.CloseAndRemove(cursor)
		return nil, wrapAggregateExecutorError(handleMaxTimeMSError(err, maxTimeMS, "aggregate"))
	}

	h.L.DebugContext(
		ctx,
		"Got first batch",
		slog.Int64("cursor_id", cursorID),
		slog.String("type", cursor.Type.String()),
		slog.Int("count", len(docs)),
		slog.Int64("batch_size", batchSize),
	)

	firstBatch := types.MakeArray(len(docs))
	for _, doc := range docs {
		firstBatch.Append(doc)
	}

	if done {
		// let the client know that there are no more results
		cursorID = 0

		cursor.Close()
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"cursor", must.NotFail(types.NewDocument(
				"firstBatch", firstBatch,
				"id", cursorID,
				"ns", dbName+"."+cName,
			)),
			"ok", float64(1),
		)),
	)
}

var dbLevelSourceStages = map[string]struct{}{
	"$currentOp":          {},
	"$documents":          {},
	"$listLocalSessions":  {},
	"$listSessions":       {},
	"$changeStream":       {},
	"$queryStats":         {},
	"$listCatalog":        {},
	"$querySettings":      {},
	"$listClusterCatalog": {},
}

// checkDatabasePipelineStages returns the error mongod reports while it parses
// the stages of an {aggregate: 1} pipeline, before it checks that each stage
// may run without a collection.
func checkDatabasePipelineStages(pipeline *types.Array, dbName string) error {
	for i := range pipeline.Len() {
		stage, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok || stage.Len() != 1 {
			return nil
		}
		switch name := stage.Command(); name {
		case "$queryStats", "$listCatalog":
			check := stages.CheckQueryStatsSpec
			if name == "$listCatalog" {
				check = stages.CheckListCatalogSpec
			}
			if err := check(stage); err != nil {
				return err
			}
			if dbName != "admin" {
				if name == "$listCatalog" {
					return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace,
						"Collectionless $listCatalog must be run against the 'admin' database with {aggregate: 1}", "$listCatalog (stage)")
				}
				_, err := stages.NewStage(stage.DeepCopy())
				return err
			}
			if i != 0 {
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCollStatsIsNotFirstStage,
					name+" is only valid as the first stage in a pipeline", name+" (stage)")
			}
		case "$facet":
			if _, err := stages.NewStage(stage.DeepCopy()); err != nil {
				return err
			}
		case "$documents", "$currentOp", "$listLocalSessions":
		default:
			if stages.FailsParsing(stage) {
				_, err := stages.NewStage(stage.DeepCopy())
				return err
			}
		}
	}
	return nil
}

func (h *Handler) aggregateDatabase(connCtx context.Context, document *types.Document, dbName string) (*wire.OpMsg, error) {
	v, _ := document.Get("cursor")
	if v == nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"The 'cursor' option is required, except for aggregate with the explain argument",
			document.Command(),
		)
	}
	if _, ok := v.(*types.Document); !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"cursor field must be missing or an object",
			document.Command(),
		)
	}

	pipeline, err := common.GetRequiredParam[*types.Array](document, "pipeline")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"'pipeline' option must be specified as an array",
			document.Command(),
		)
	}
	if err = aggregations.CheckPipelineLength(pipeline.Len()); err != nil {
		return nil, err
	}
	pipeline = stages.MoveSkipsBeforeTransforms(operators.BindNowInPipeline(pipeline, operators.CommandNow()))

	if pipeline.Len() == 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrInvalidNamespace,
			"{aggregate: 1} is not valid for an empty pipeline.",
			document.Command(),
		)
	}

	firstStage, ok := must.NotFail(pipeline.Get(0)).(*types.Document)
	if !ok || firstStage.Len() == 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"Each element of the 'pipeline' array must be an object",
			document.Command(),
		)
	}

	stageName := firstStage.Command()
	if err = checkDatabasePipelineStages(pipeline, dbName); err != nil {
		return nil, err
	}
	if !stages.IsKnownStage(stageName) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageUnrecognized,
			fmt.Sprintf("Unrecognized pipeline stage name: '%s'", stageName),
			document.Command(),
		)
	}
	if _, dbOK := dbLevelSourceStages[stageName]; !dbOK {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrInvalidNamespace,
			fmt.Sprintf("{aggregate: 1} is not valid for '%s'; a collection is required.", stageName),
			document.Command(),
		)
	}

	if stageName == "$currentOp" {
		if err := stages.CheckCurrentOpSpec(firstStage); err != nil {
			return nil, err
		}
	}
	if stageName == "$currentOp" && dbName != "admin" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrInvalidNamespace,
			"$currentOp must be run against the 'admin' database with {aggregate: 1}",
			document.Command(),
		)
	}

	switch stageName {
	case "$documents":
		return h.aggregateDocuments(connCtx, document, firstStage, pipeline, dbName)
	case "$queryStats", "$querySettings":
		// DumboDB records no query statistics and has no query settings.
		return h.aggregateDocuments(connCtx, document, must.NotFail(types.NewDocument("$documents", types.MakeArray(0))), pipeline, dbName)
	case "$listCatalog", "$listClusterCatalog":
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrNotImplemented,
			stageName+" is not implemented", stageName+" (stage)")
	}

	if stageName == "$listLocalSessions" {
		return h.aggregateListLocalSessions(connCtx, dbName, firstStage)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"cursor", must.NotFail(types.NewDocument(
				"firstBatch", must.NotFail(types.NewArray()),
				"id", int64(0),
				"ns", dbName+".$cmd.aggregate",
			)),
			"ok", float64(1),
		)),
	)
}

// aggregateListLocalSessions implements the $listLocalSessions source
// stage: one document per logical session cached on this node and owned by a
// principal the spec selects. Each session's _id.id is the lsid UUID and
// _id.uid is SHA256("user@db") (SHA256("") when unauthenticated), matching
// MongoDB's session identity encoding.
func (h *Handler) aggregateListLocalSessions(connCtx context.Context, dbName string, stage *types.Document) (*wire.OpMsg, error) {
	spec, err := parseListSessionsSpec(stage, conninfo.Get(connCtx).SessionPrincipal())
	if err != nil {
		return nil, err
	}

	firstBatch := types.MakeArray(0)
	if reg := h.SessionRegistry(); reg != nil {
		for _, s := range reg.Snapshot() {
			principal, id, ok := conninfo.SplitSessionKey(s.Lsid)
			if !ok || !spec.includes(principal) {
				continue
			}
			// Only real driver lsids (a 16-byte hex UUID) have a MongoDB
			// counterpart; skip synthetic ids assigned to connections that
			// never supplied an lsid.
			idBytes, err := hex.DecodeString(id)
			if err != nil || len(idBytes) != 16 {
				continue
			}
			uidSum := sha256.Sum256([]byte(sessionUserDigestName(principal)))
			firstBatch.Append(must.NotFail(types.NewDocument(
				"_id", must.NotFail(types.NewDocument(
					"id", types.Binary{Subtype: types.BinaryUUID, B: idBytes},
					"uid", types.Binary{Subtype: types.BinaryGeneric, B: uidSum[:]},
				)),
				"lastUse", s.LastUsed,
			)))
		}
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"cursor", must.NotFail(types.NewDocument(
				"firstBatch", firstBatch,
				"id", int64(0),
				"ns", dbName+".$cmd.aggregate",
			)),
			"ok", float64(1),
		)),
	)
}

// sessionUserDigestName is the "user@db" name MongoDB hashes into a session
// uid; "" for an unauthenticated principal.
func sessionUserDigestName(principal string) string {
	user, db, ok := conninfo.SplitPrincipal(principal)
	if !ok {
		return ""
	}
	return user + "@" + db
}

// listSessionsSpec is the parsed argument of $listLocalSessions or
// $listSessions. Without allUsers it selects sessions owned by the listed
// principals (see conninfo.Principal); an empty spec selects the caller's own.
type listSessionsSpec struct {
	allUsers   bool
	principals []string
}

func (spec listSessionsSpec) includes(principal string) bool {
	return spec.allUsers || slices.Contains(spec.principals, principal)
}

// requiresListSessionsPrivilege reports whether the spec reaches beyond the
// caller's own sessions.
func (spec listSessionsSpec) requiresListSessionsPrivilege(callerPrincipal string) bool {
	if spec.allUsers {
		return true
	}
	for _, p := range spec.principals {
		if p != callerPrincipal {
			return true
		}
	}
	return false
}

func parseListSessionsSpec(stage *types.Document, callerPrincipal string) (listSessionsSpec, error) {
	stageName := stage.Command()
	value := must.NotFail(stage.Get(stageName))
	options, ok := value.(*types.Document)
	if !ok {
		return listSessionsSpec{}, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrTypeMismatch,
			fmt.Sprintf("%s options must be specified in an object, but found: %s", stageName, handlerparams.AliasFromType(value)),
		)
	}

	var spec listSessionsSpec
	if v, err := options.Get("allUsers"); err == nil {
		spec.allUsers, _ = v.(bool)
	}

	if v, err := options.Get("users"); err == nil {
		users, ok := v.(*types.Array)
		if !ok {
			return listSessionsSpec{}, handlererrors.NewCommandErrorMsg(handlererrors.ErrTypeMismatch, stageName+" users must be an array")
		}
		for i := 0; i < users.Len(); i++ {
			user, _ := must.NotFail(users.Get(i)).(*types.Document)
			var name, db string
			var nameOK, dbOK bool
			if user != nil {
				nameValue, _ := user.Get("user")
				dbValue, _ := user.Get("db")
				name, nameOK = nameValue.(string)
				db, dbOK = dbValue.(string)
			}
			if !nameOK || !dbOK {
				return listSessionsSpec{}, handlererrors.NewCommandErrorMsg(
					handlererrors.ErrTypeMismatch,
					stageName+" users entries must be objects with string user and db fields",
				)
			}
			spec.principals = append(spec.principals, conninfo.Principal(name, db))
		}
	}

	if spec.allUsers && len(spec.principals) > 0 {
		return listSessionsSpec{}, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrBadValue,
			stageName+" may not specify {allUsers:true} and {users:[...]} at the same time",
		)
	}
	if !spec.allUsers && len(spec.principals) == 0 {
		spec.principals = []string{callerPrincipal}
	}
	return spec, nil
}

// aggregateDocuments runs a database-level pipeline whose source stage is
// $documents: an inline array of documents used as the pipeline input. The
// array elements are fed through any remaining pipeline stages and returned
// through the normal cursor/batch machinery.
func (h *Handler) aggregateDocuments(connCtx context.Context, document, firstStage *types.Document, pipeline *types.Array, dbName string) (*wire.OpMsg, error) {
	// $documents evaluates its argument as an expression.
	srcDocs, err := stages.EvalDocuments(firstStage)
	if err != nil {
		return nil, err
	}

	pipelineStages := make([]aggregations.Stage, 0, pipeline.Len()-1)
	for i := 1; i < pipeline.Len(); i++ {
		sd, ok := must.NotFail(pipeline.Get(i)).(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"Each element of the 'pipeline' array must be an object",
				document.Command(),
			)
		}
		if err := documentsNotFirst(sd, i); err != nil {
			return nil, err
		}
		var s aggregations.Stage
		var err error
		switch sd.Command() {
		case "$lookup", "$graphLookup", "$unionWith", "$facet":
			db, dbErr := h.b.Database(dbName)
			if dbErr != nil {
				return nil, lazyerrors.Error(dbErr)
			}
			s, err = newForeignCollectionStage(sd, db)
		default:
			s, err = stages.NewStage(sd)
		}
		if err != nil {
			return nil, err
		}
		pipelineStages = append(pipelineStages, s)
	}

	cursorDoc := must.NotFail(document.Get("cursor")).(*types.Document)
	bsVal, _ := cursorDoc.Get("batchSize")
	if bsVal == nil {
		bsVal = int32(101)
	}
	batchSize, err := handlerparams.GetValidatedNumberParamWithMinValue("cursor", "batchSize", bsVal, 0)
	if err != nil {
		return nil, err
	}

	closer := iterator.NewMultiCloser()
	iter := iterator.Values(iterator.ForSlice(srcDocs))
	for _, s := range pipelineStages {
		if iter, err = s.Process(connCtx, iter, closer); err != nil {
			closer.Close()
			return nil, err
		}
	}
	closer.Add(iter)

	cur := h.cursors.NewCursor(connCtx, iterator.WithClose(iter, closer.Close), &cursor.NewParams{
		DB:         dbName,
		Collection: "$cmd.aggregate",
		Owner:      conninfo.Get(connCtx).SessionPrincipal(),
		Type:       cursor.Normal,
	})

	cursorID := cur.ID

	docs, done, err := cur.NextBatch(int(batchSize), types.MaxDocumentLen)
	if err != nil {
		h.cursors.CloseAndRemove(cur)
		return nil, err
	}

	firstBatch := types.MakeArray(len(docs))
	for _, doc := range docs {
		firstBatch.Append(doc)
	}

	if done {
		cursorID = 0
		cur.Close()
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"cursor", must.NotFail(types.NewDocument(
				"firstBatch", firstBatch,
				"id", cursorID,
				"ns", dbName+".$cmd.aggregate",
			)),
			"ok", float64(1),
		)),
	)
}

type stagesDocumentsParams struct {
	c      backends.Collection
	qp     *backends.QueryParams
	stages []aggregations.Stage
	// sortStage is the $sort stage qp.Sort pushes down; it is skipped when
	// the backend returns documents already in that order.
	sortStage aggregations.Stage
	// hintOrder, set for an unsorted read hinted to an index with a
	// descending key, is that index's key pattern, the order mongod returns.
	hintOrder *types.Document
	// hintSortFilter is the filter hintOrder sorts by (hintedIndexOrder).
	hintSortFilter *types.Document
	cmp            *collation.Comparator
}

// pushdownSortStageIndex returns the position of the $sort stage
// aggregations.GetPushdownQuery pushes down.
func pushdownSortStageIndex(aggregationStages []any) int {
	for i := 0; i < len(aggregationStages) && i < 2; i++ {
		if d := stageDocAt(aggregationStages, i); d != nil && d.Command() == "$sort" {
			return i
		}
	}
	panic("pushdownSortStageIndex: no $sort among the pushdown stages")
}

// sortLimitBound returns the top-K bound a $sort at index i may use when a
// $limit (optionally preceded by a $skip) immediately follows it, else 0. This
// mirrors MongoDB's sort+limit and sort+skip+limit coalescence.
func sortLimitBound(aggregationStages []any, i int) int64 {
	next := stageDocAt(aggregationStages, i+1)
	if next == nil {
		return 0
	}

	switch next.Command() {
	case "$limit":
		if l, ok := stageInt64(next, "$limit", common.GetLimitStageParam); ok {
			return l
		}
	case "$skip":
		after := stageDocAt(aggregationStages, i+2)
		if after == nil || after.Command() != "$limit" {
			return 0
		}

		s, sok := stageInt64(next, "$skip", common.GetSkipStageParam)
		l, lok := stageInt64(after, "$limit", common.GetLimitStageParam)
		if sok && lok {
			return s + l
		}
	}

	return 0
}

// runsInSBE reports whether mongod runs a pipeline in SBE: one with a stage
// SBE executes, such as $group or $lookup.
func runsInSBE(stages []any) bool {
	for _, s := range stages {
		if d, _ := s.(*types.Document); d != nil && sbeStages[d.Command()] {
			return true
		}
	}
	return false
}

// pushdownLimit returns the $limit that directly follows the stages
// aggregations.GetPushdownQuery pushes down (at most one $match and one $sort
// leading the pipeline), else 0. The query layer applies it like a find limit.
func pushdownLimit(aggregationStages []any) int64 {
	seen := map[string]bool{}
	for i := 0; i < len(aggregationStages); i++ {
		d := stageDocAt(aggregationStages, i)
		if d == nil {
			return 0
		}
		switch cmd := d.Command(); cmd {
		case "$match", "$sort":
			if seen[cmd] || i >= 2 {
				return 0
			}
			seen[cmd] = true
		case "$limit":
			if i == 0 {
				return 0
			}
			l, _ := stageInt64(d, "$limit", common.GetLimitStageParam)
			return l
		default:
			return 0
		}
	}
	return 0
}

func stageDocAt(aggregationStages []any, i int) *types.Document {
	if i < 0 || i >= len(aggregationStages) {
		return nil
	}

	d, _ := aggregationStages[i].(*types.Document)

	return d
}

func stageInt64(d *types.Document, key string, parse func(any) (int64, error)) (int64, bool) {
	v, err := d.Get(key)
	if err != nil {
		return 0, false
	}

	n, err := parse(v)
	if err != nil {
		return 0, false
	}

	return n, true
}

// processStagesDocuments retrieves the documents from the database and then processes them through the stages.
func processStagesDocuments(ctx context.Context, closer *iterator.MultiCloser, p *stagesDocumentsParams) (types.DocumentsIterator, error) { //nolint:lll // for readability
	queryRes, err := stagesSource(ctx, p)
	if err != nil {
		closer.Close()
		return nil, lazyerrors.Error(err)
	}

	closer.Add(queryRes.Iter)

	// Stages evaluate expressions per document; stop between documents once
	// maxTimeMS expires or the client goes away.
	var iter types.DocumentsIterator = iterator.WithContext(ctx, queryRes.Iter)
	if p.hintOrder != nil {
		if iter, err = common.SortIteratorInIndexOrder(iter, closer, p.hintOrder, p.hintSortFilter, p.cmp); err != nil {
			return nil, err
		}
	}

	for _, s := range p.stages {
		if queryRes.Sorted && s == p.sortStage {
			iter = withoutRecordKeys(iter)
			continue
		}
		if iter, err = s.Process(ctx, iter, closer); err != nil {
			return nil, err
		}
		if !stages.KeepsRecordID(s) {
			iter = withoutRecordKeys(iter)
		}
	}

	return iter, nil
}

// stagesSource reads the pipeline's input: nearest first from a geo index
// when it starts with a $geoNear one serves, otherwise by p.qp.
func stagesSource(ctx context.Context, p *stagesDocumentsParams) (*backends.QueryResult, error) {
	if len(p.stages) > 0 {
		near, isNear := p.stages[0].(stages.GeoNearStage)
		scanner, canScan := p.c.(backends.GeoNearScanner)
		if isNear && canScan {
			res, err := scanner.GeoNear(ctx, near.NearParams())
			if err != nil {
				return nil, err
			}
			if res != nil {
				near.ReadsSorted(res.Field)
				return &backends.QueryResult{Iter: res.Iter}, nil
			}
		}
	}
	return p.c.Query(ctx, p.qp)
}

type stagesStatsParams struct {
	c          backends.Collection
	db         backends.Database
	dbName     string
	cName      string
	statistics map[stages.Statistic]struct{}
	stages     []aggregations.Stage
	tcpHost    string
	// subPipeline is set for $collStats at the start of a sub-pipeline,
	// which reads through a view to its base collection.
	subPipeline bool
}

// resolveStatsView points p at the base collection of a view, as mongod
// does for a sub-pipeline over a view without stages of its own. A view's
// stages come before $collStats, which then fails as not first (40602); at
// the top level mongod refuses a view (CommandNotSupportedOnView).
func resolveStatsView(ctx context.Context, p *stagesStatsParams) error {
	info, err := lookupCollectionInfo(ctx, p.db, p.cName)
	if err != nil || info == nil || !info.IsView {
		return nil
	}
	if !p.subPipeline {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCommandNotSupportedOnView,
			fmt.Sprintf("Namespace %s.%s is a view, not a collection", p.dbName, p.cName), "aggregate")
	}
	base, viewStages, _, err := resolveViewChain(ctx, p.db, info.Name, info.ViewOn, info.ViewPipeline, nil)
	if err != nil {
		return err
	}
	if len(viewStages) > 0 {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCollStatsIsNotFirstStage,
			"$collStats is only valid as the first stage in a pipeline", "$collStats")
	}
	if p.c, err = p.db.Collection(base); err != nil {
		return lazyerrors.Error(err)
	}
	p.cName = base
	return nil
}

// processStagesStats retrieves the statistics from the database and then processes them through the stages.
func processStagesStats(ctx context.Context, closer *iterator.MultiCloser, p *stagesStatsParams) (types.DocumentsIterator, error) { //nolint:lll // for readability
	if err := resolveStatsView(ctx, p); err != nil {
		return nil, err
	}
	_, hasCount := p.statistics[stages.StatisticCount]
	_, hasStorage := p.statistics[stages.StatisticStorage]

	var host string
	var err error

	hostname, err := os.Hostname()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	host = hostname
	if p.tcpHost != "" {
		if _, port, err := net.SplitHostPort(p.tcpHost); err == nil && port != "" {
			host = hostname + ":" + port
		}
	}

	doc := must.NotFail(types.NewDocument(
		"ns", p.dbName+"."+p.cName,
		"host", host,
		"localTime", time.Now().UTC(),
	))

	var (
		collStats      *backends.CollectionStatsResult
		cInfo          backends.CollectionInfo
		nIndexes       int64
		visibleIndexes []backends.IndexInfo
	)

	if hasCount || hasStorage {
		collStats, err = p.c.Stats(ctx, new(backends.CollectionStatsParams))
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) ||
			backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseDoesNotExist) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNamespaceNotFound,
				fmt.Sprintf(
					"PlanExecutor error during aggregation :: caused by :: "+
						"Unable to retrieve storageStats in $collStats stage :: "+
						"caused by :: Collection [%s.%s] not found.",
					p.dbName, p.cName,
				),
				"aggregate",
			)
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		var cList *backends.ListCollectionsResult
		collectionParam := backends.ListCollectionsParams{Name: p.cName}

		if cList, err = p.db.ListCollections(ctx, &collectionParam); err != nil {
			return nil, lazyerrors.Error(err)
		}

		if len(cList.Collections) > 0 {
			cInfo = cList.Collections[0]
		}

		var iList *backends.ListIndexesResult

		iList, err = p.c.ListIndexes(ctx, new(backends.ListIndexesParams))
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) {
			iList = new(backends.ListIndexesResult)
			err = nil
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		iList.Indexes = clientVisibleIndexes(ctx, p.db, p.cName, iList.Indexes)
		nIndexes = int64(len(iList.Indexes))
		visibleIndexes = iList.Indexes
	}

	if hasStorage {
		var avgObjSize int32
		if collStats.CountDocuments > 0 {
			avgObjSize = int32(collStats.SizeCollection / collStats.CountDocuments)
		}

		indexSizes := types.MakeDocument(len(collStats.IndexSizes))
		indexDetails := types.MakeDocument(len(collStats.IndexSizes))
		for _, indexSize := range collStats.IndexSizes {
			if visibleIndexes != nil && !hasIndexNamed(visibleIndexes, indexSize.Name) {
				continue
			}
			indexSizes.Set(indexSize.Name, int32(indexSize.Size))
			indexDetails.Set(indexSize.Name, must.NotFail(types.NewDocument()))
		}

		// storageSize is the allocated storage for documents (SizeTotal - index bytes).
		storageSize := collStats.SizeTotal - collStats.SizeIndexes

		doc.Set(
			"storageStats", must.NotFail(types.NewDocument(
				"size", int32(collStats.SizeCollection),
				"count", int32(collStats.CountDocuments),
				"avgObjSize", avgObjSize,
				"numOrphanDocs", int32(0),
				"storageSize", int32(storageSize),
				"freeStorageSize", int32(collStats.SizeFreeStorage),
				"capped", cInfo.Capped(),
				"nindexes", int32(nIndexes),
				"indexDetails", indexDetails,
				"indexBuilds", must.NotFail(types.NewArray()),
				"totalIndexSize", int32(collStats.SizeIndexes),
				"indexSizes", indexSizes,
				"totalSize", int32(collStats.SizeTotal),
			)),
		)
	}

	if hasCount {
		doc.Set(
			"count", int32(collStats.CountDocuments),
		)
	}

	iter := iterator.Values(iterator.ForSlice([]*types.Document{doc}))
	closer.Add(iter)

	for _, s := range p.stages {
		if iter, err = s.Process(ctx, iter, closer); err != nil {
			return nil, err
		}
	}

	return iter, nil
}

// outBatchSize and outBatchBytes bound how much of $out's input is held before
// it is inserted.
const (
	outBatchSize  = 1000
	outBatchBytes = 16 * 1024 * 1024
)

// makeOutWriter returns an OutWriter callback that replaces the target
// collection with the provided documents. As in MongoDB, they are written in
// batches to a temporary collection that is then renamed over the target, so
// the target is unchanged if the pipeline or a write fails.
func makeOutWriter(b backends.Backend, currentDB string) stages.OutWriter { //nolint:unparam // currentDB used by caller
	return func(ctx context.Context, dbName, collName string, docs types.DocumentsIterator) error {
		db, err := b.Database(dbName)
		if err != nil {
			return lazyerrors.Error(err)
		}

		tmpName := "tmp.agg_out." + uuid.NewString()
		if err = db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: tmpName}); err != nil {
			return lazyerrors.Error(err)
		}
		renamed := false
		defer func() {
			if !renamed {
				_ = db.DropCollection(ctx, &backends.DropCollectionParams{Name: tmpName})
			}
		}()

		tmp, err := db.Collection(tmpName)
		if err != nil {
			return lazyerrors.Error(err)
		}

		if err = copyOutTargetIndexes(ctx, db, collName, tmp); err != nil {
			return err
		}

		var batch []*types.Document
		var batchBytes int
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			_, insertErr := tmp.InsertAll(ctx, &backends.InsertAllParams{Docs: batch})
			batch, batchBytes = nil, 0
			return outWriteError(dbName, collName, insertErr)
		}

		for {
			_, doc, nextErr := docs.Next()
			if errors.Is(nextErr, iterator.ErrIteratorDone) {
				break
			}
			if nextErr != nil {
				return nextErr
			}
			if !doc.Has("_id") {
				doc.Set("_id", types.NewObjectID())
			}
			batch = append(batch, doc)
			batchBytes += doc.BSONSize()
			if len(batch) >= outBatchSize || batchBytes >= outBatchBytes {
				if err = flush(); err != nil {
					return err
				}
			}
		}
		if err = flush(); err != nil {
			return err
		}

		if err = db.RenameCollection(ctx, &backends.RenameCollectionParams{OldName: tmpName, NewName: collName, DropTarget: true}); err != nil {
			return lazyerrors.Error(err)
		}
		renamed = true

		return nil
	}
}

// copyOutTargetIndexes creates the indexes of an existing $out target on the
// temporary collection that replaces it, as mongod keeps them.
func copyOutTargetIndexes(ctx context.Context, db backends.Database, collName string, tmp backends.Collection) error {
	target, err := db.Collection(collName)
	if err != nil {
		return lazyerrors.Error(err)
	}
	res, err := target.ListIndexes(ctx, new(backends.ListIndexesParams))
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) ||
			backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseDoesNotExist) {
			return nil
		}
		return lazyerrors.Error(err)
	}
	var indexes []backends.IndexInfo
	for _, idx := range res.Indexes {
		if idx.Name != backends.DefaultIndexName {
			indexes = append(indexes, idx)
		}
	}
	if len(indexes) == 0 {
		return nil
	}
	if _, err = tmp.CreateIndexes(ctx, &backends.CreateIndexesParams{Indexes: indexes}); err != nil {
		return lazyerrors.Error(err)
	}
	return nil
}

// outWriteError returns mongod's error for a $out write that breaks a unique
// index of the target, or err itself.
func outWriteError(dbName, collName string, err error) error {
	if err == nil {
		return nil
	}
	var be *backends.Error
	if !errors.As(err, &be) || be.Code() != backends.ErrorCodeInsertDuplicateID {
		return err
	}
	msg := fmt.Sprintf("E11000 duplicate key error collection: %s.%s", dbName, collName)
	if be.DupIndex() != "" {
		msg += " index: " + be.DupIndex()
	}
	if k := be.DupKey(); k != nil {
		msg += " dup key: " + types.FormatAnyValue(k)
	}
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDuplicateKeyInsert, msg, "$out")
}

// mergeWriteError returns mongod's error for a $merge write that breaks a
// unique index, or err itself.
func mergeWriteError(params *stages.MergeParams, err error) error {
	if err == nil {
		return nil
	}
	var be *backends.Error
	if !errors.As(err, &be) || be.Code() != backends.ErrorCodeInsertDuplicateID {
		return lazyerrors.Error(err)
	}
	msg := fmt.Sprintf("$merge failed due to a DuplicateKey error :: caused by :: E11000 duplicate key error collection: %s.%s",
		params.DBName, params.CollName)
	if be.DupIndex() != "" {
		msg += " index: " + be.DupIndex()
	}
	if k := be.DupKey(); k != nil {
		msg += " dup key: " + types.FormatAnyValue(k)
	}
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDuplicateKeyInsert, msg, "$merge")
}

// mergeChangesID reports whether merging incoming into the matched existing
// document would change its _id.
func mergeChangesID(existing, incoming *types.Document) bool {
	id, err := incoming.Get("_id")
	if err != nil {
		return false
	}
	return !types.Identical(id, must.NotFail(existing.Get("_id")))
}

func mergeImmutableIDError(cause string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrImmutableField,
		"$merge failed to update the matching document, did you attempt to modify the _id or the shard key? :: caused by :: "+cause,
		"$merge (stage)")
}

// checkMergeOnIndex returns mongod's error when $merge's 'on' fields other than
// _id alone are not exactly the fields of a unique, non-partial index with the
// simple collation on the target collection.
func checkMergeOnIndex(ctx context.Context, b backends.Backend, stage aggregations.Stage) error {
	dbName, collName, on, ok := stages.MergeTarget(stage)
	if !ok || len(on) == 0 || (len(on) == 1 && on[0] == "_id") {
		return nil
	}

	noIndex := handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrMergeOnNoUniqueIndex,
		"Cannot find index to verify that join fields will be unique", "$merge")

	db, err := b.Database(dbName)
	if err != nil {
		return lazyerrors.Error(err)
	}
	coll, err := db.Collection(collName)
	if err != nil {
		return lazyerrors.Error(err)
	}
	res, err := coll.ListIndexes(ctx, new(backends.ListIndexesParams))
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) ||
			backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseDoesNotExist) {
			return noIndex
		}
		return lazyerrors.Error(err)
	}

	onFields := make(map[string]bool, len(on))
	for _, f := range on {
		onFields[f] = true
	}
	for _, idx := range res.Indexes {
		if !idx.Unique || idx.PartialFilterExpression != nil || idx.Collation != nil || len(idx.Key) != len(onFields) {
			continue
		}
		matches := true
		for _, k := range idx.Key {
			if !onFields[k.Field] {
				matches = false
				break
			}
		}
		if matches {
			return nil
		}
	}
	return noIndex
}

// makeMergeWriter returns a MergeFunc callback that merges documents into the
// target collection according to the $merge semantics.
func makeMergeWriter(b backends.Backend, currentDB string) stages.MergeFunc { //nolint:unparam // currentDB used by caller
	return func(ctx context.Context, params *stages.MergeParams) error {
		db, err := b.Database(params.DBName)
		if err != nil {
			return lazyerrors.Error(err)
		}

		coll, err := db.Collection(params.CollName)
		if err != nil {
			return lazyerrors.Error(err)
		}

		if len(params.Docs) == 0 {
			return nil
		}

		qRes, err := coll.Query(ctx, new(backends.QueryParams))
		if err != nil {
			if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) ||
				backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseDoesNotExist) {
				// Collection doesn't exist yet; all docs are "not matched".
				if params.WhenNotMatched == "insert" {
					for _, doc := range params.Docs {
						if !doc.Has("_id") {
							doc.Set("_id", types.NewObjectID())
						}
					}
					_, insertErr := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: params.Docs})
					return mergeWriteError(params, insertErr)
				}

				return nil
			}

			return lazyerrors.Error(err)
		}

		type existingEntry struct {
			doc   *types.Document
			index int
		}

		// Only the target documents this batch can match are kept, so memory
		// follows the batch rather than the target collection.
		wanted := make(map[string]bool, len(params.Docs))
		for _, incoming := range params.Docs {
			wanted[buildMergeKey(incoming, params.On)] = true
		}

		existingMap := make(map[string]*existingEntry, len(params.Docs))

		for i := 0; ; i++ {
			_, existing, nextErr := qRes.Iter.Next()
			if errors.Is(nextErr, iterator.ErrIteratorDone) {
				break
			}
			if nextErr != nil {
				qRes.Iter.Close()
				return lazyerrors.Error(nextErr)
			}
			if key := buildMergeKey(existing, params.On); wanted[key] {
				existingMap[key] = &existingEntry{doc: existing, index: i}
			}
		}
		qRes.Iter.Close()

		var toInsert []*types.Document
		var toUpdate []*types.Document

		for _, incoming := range params.Docs {
			key := buildMergeKey(incoming, params.On)
			entry, matched := existingMap[key]

			if matched {
				switch params.WhenMatched {
				case "merge":
					if mergeChangesID(entry.doc, incoming) {
						return mergeImmutableIDError("Performing an update on the path '_id' would modify the immutable field '_id'")
					}

					// Merge fields from incoming into existing.
					merged := entry.doc
					for _, k := range incoming.Keys() {
						v := must.NotFail(incoming.Get(k))
						merged.Set(k, v)
					}

					toUpdate = append(toUpdate, merged)

				case "replace":
					if mergeChangesID(entry.doc, incoming) {
						return mergeImmutableIDError(fmt.Sprintf(
							"After applying the update, the (immutable) field '_id' was found to have been altered to _id: %s",
							types.FormatAnyValue(must.NotFail(incoming.Get("_id")))))
					}

					// Replace existing doc (preserving its _id if incoming lacks one).
					if _, idErr := incoming.Get("_id"); idErr != nil {
						id := must.NotFail(entry.doc.Get("_id"))
						incoming.Set("_id", id)
					}

					toUpdate = append(toUpdate, incoming)

				case "keepExisting":
					// Do nothing; existing document wins.

				case "fail":
					return handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrDuplicateKeyInsert,
						"$merge with whenMatched: fail found an existing document with the same values for the 'on' fields",
						"$merge (stage)",
					)
				}
			} else {
				switch params.WhenNotMatched {
				case "insert":
					if !incoming.Has("_id") {
						incoming.Set("_id", types.NewObjectID())
					}
					toInsert = append(toInsert, incoming)

				case "discard":
					// Skip this document.

				case "fail":
					return handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrMergeStageNoMatchingDocument,
						"$merge could not find a matching document in the target collection for at least one document in the source collection",
						"$merge (stage)",
					)
				}
			}
		}

		if len(toInsert) > 0 {
			if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: toInsert}); err != nil {
				return mergeWriteError(params, err)
			}
		}

		if len(toUpdate) > 0 {
			if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: toUpdate}); err != nil {
				return mergeWriteError(params, err)
			}
		}

		return nil
	}
}

func wrapAggregateExecutorError(err error) error {
	if err == nil {
		return nil
	}
	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		return err
	}
	if executorWrapSkip[cmdErr.Code()] {
		return err
	}
	inner := cmdErr.Err().Error()
	if strings.Contains(inner, "PlanExecutor error during aggregation") {
		return err
	}
	return handlererrors.NewCommandErrorMsg(
		cmdErr.Code(),
		"PlanExecutor error during aggregation :: caused by :: "+inner,
	)
}

// This key is used to match incoming documents against existing documents in $merge.
func buildMergeKey(doc *types.Document, fields []string) string {
	if len(fields) == 1 {
		v, err := doc.Get(fields[0])
		if err != nil {
			return ""
		}

		return fmt.Sprintf("%v", v)
	}

	key := ""

	for _, f := range fields {
		v, err := doc.Get(f)
		if err != nil {
			key += "|<missing>"
			continue
		}

		key += fmt.Sprintf("|%v", v)
	}

	return key
}

// documentsNotFirst is mongod's error for $documents after the first stage.
func documentsNotFirst(stage *types.Document, i int) error {
	if i == 0 || stage.Command() != "$documents" {
		return nil
	}
	return handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrCollStatsIsNotFirstStage,
		"$documents is only valid as the first stage in a pipeline",
		"$documents",
	)
}

// withoutRecordKeys yields iter's documents without their RecordId key.
func withoutRecordKeys(iter types.DocumentsIterator) types.DocumentsIterator {
	return &recordKeyDropper{iter}
}

type recordKeyDropper struct {
	types.DocumentsIterator
}

func (d *recordKeyDropper) Next() (struct{}, *types.Document, error) {
	zero, doc, err := d.DocumentsIterator.Next()
	if doc != nil {
		doc.SetRecordKey(nil)
	}
	return zero, doc, err
}
