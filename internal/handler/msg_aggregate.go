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
	"strings"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/clientconn/cursor"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
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

	common.Ignored(document, h.L, "lsid")

	if err = common.Unimplemented(document, "explain", "collation", "let"); err != nil {
		return nil, err
	}

	common.Ignored(
		document, h.L,
		"allowDiskUse", "bypassDocumentValidation", "readConcern", "hint", "comment", "writeConcern",
	)

	var dbName string

	if dbName, err = common.GetRequiredParam[string](document, "$db"); err != nil {
		return nil, err
	}

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

	username := conninfo.Get(connCtx).Username()

	v, _ := document.Get("maxTimeMS")
	if v == nil {
		v = int64(0)
	}

	// cannot use other existing handlerparams function, they return different error codes
	maxTimeMS, err := handlerparams.GetWholeNumberParam(v)
	if err != nil {
		switch {
		case errors.Is(err, handlerparams.ErrUnexpectedType):
			if _, ok = v.(types.NullType); ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"maxTimeMS must be a number",
					document.Command(),
				)
			}

			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf(
					`BSON field 'aggregate.maxTimeMS' is the wrong type '%s', expected types '[long, int, decimal, double']`,
					handlerparams.AliasFromType(v),
				),
				document.Command(),
			)
		case errors.Is(err, handlerparams.ErrNotWholeNumber):
			// For negative non-integral floats, MongoDB reports a "must be >= 0" error
			// using the floor (truncated toward -inf) integer value.
			if fv, ok := v.(float64); ok && fv < 0 {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrValueNegative,
					fmt.Sprintf("BSON field 'maxTimeMS' value must be >= 0, actual value '%d'", int64(math.Floor(fv))),
					document.Command(),
				)
			}

			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"maxTimeMS has non-integral value",
				document.Command(),
			)
		case errors.Is(err, handlerparams.ErrLongExceededPositive):
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("%d value for maxTimeMS is out of range [0, 2147483647]", int64(math.MaxInt64)),
				document.Command(),
			)
		case errors.Is(err, handlerparams.ErrLongExceededNegative):
			// For floats that exceed the int64 range in the negative direction, MongoDB
			// reports the saturated int64 value (math.MinInt64) rather than the original float.
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrValueNegative,
				fmt.Sprintf("BSON field 'maxTimeMS' value must be >= 0, actual value '%d'", int64(math.MinInt64)),
				document.Command(),
			)
		default:
			return nil, lazyerrors.Error(err)
		}
	}

	if maxTimeMS < int64(0) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrValueNegative,
			fmt.Sprintf("BSON field 'maxTimeMS' value must be >= 0, actual value '%s'", types.FormatAnyValue(v)),
			document.Command(),
		)
	}

	if maxTimeMS > math.MaxInt32 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("%v value for maxTimeMS is out of range [0, 2147483647]", v),
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

	aggregationStages := must.NotFail(iterator.ConsumeValues(pipeline.Iterator()))
	stagesDocuments := make([]aggregations.Stage, 0, len(aggregationStages))
	collStatsDocuments := make([]aggregations.Stage, 0, len(aggregationStages))

	for i, v := range aggregationStages {
		var d *types.Document

		if d, ok = v.(*types.Document); !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"Each element of the 'pipeline' array must be an object",
				document.Command(),
			)
		}

		var s aggregations.Stage

		switch d.Command() {
		case "$lookup", "$graphLookup":
			// $lookup and $graphLookup require database access to fetch the "from" collection.
			fetcher := func(ctx context.Context, collName string) ([]*types.Document, error) {
				fromColl, collErr := db.Collection(collName)
				if collErr != nil {
					return nil, collErr
				}

				qRes, qErr := fromColl.Query(ctx, new(backends.QueryParams))
				if qErr != nil {
					return nil, qErr
				}

				defer qRes.Iter.Close()

				return iterator.ConsumeValues(qRes.Iter)
			}

			if d.Command() == "$graphLookup" {
				s, err = stages.NewGraphLookupStage(d, fetcher)
			} else {
				s, err = stages.NewLookupStage(d, fetcher)
			}

		case "$out":
			// $out requires database write access to replace a collection.
			outWriter := makeOutWriter(h.b, dbName)
			s, err = stages.NewOutStage(d, dbName, outWriter)

		case "$merge":
			// $merge requires database read/write access to merge into a collection.
			mergeWriter := makeMergeWriter(h.b, dbName)
			s, err = stages.NewMergeStage(d, dbName, mergeWriter)

		case "$indexStats":
			// $indexStats requires collection access to retrieve index metadata.
			s, err = stages.NewIndexStatsStage(d, connCtx, c, h.TCPHost)

		case "$sort":
			// Coalesce a following $limit (or $skip + $limit) into a top-K bound
			// so $sort holds only the needed documents instead of the whole input.
			s, err = stages.NewSortStage(d, sortLimitBound(aggregationStages, i))

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
			collStatsDocuments = append(collStatsDocuments, s) // It's possible to apply any stage after $collStats stage
		}
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

		// only documents stages or no stages - fetch documents from the DB and apply stages to them
		qp := new(backends.QueryParams)

		if !h.DisablePushdown {
			qp.Filter = filter
		}

		if !h.EnableNestedPushdown && filter != nil {
			qp.Filter = filter.DeepCopy()

			for _, k := range qp.Filter.Keys() {
				if !strings.ContainsRune(k, '.') {
					continue
				}

				qp.Filter.Remove(k)
			}
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

		// Fast path: drivers (notably the official Go driver) implement
		// CountDocuments by issuing an aggregate pipeline of the form
		// [{$match: <filter>}, optional $skip/$limit, {$group: {_id: <c>, n: {$sum: 1}}}].
		// When the target is a real collection (not a view), the backend can
		// satisfy this from a covering index (filtered) or O(1) tree count
		// (unfiltered) and skip the full scan + group accumulator entirely.
		// Filtered counts where the backend cannot answer from an index fall
		// through to the regular pipeline path.
		if !cInfo.IsView {
			if info, ok := tryCountAggregateShortcut(aggregationStages); ok {
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

					return documentOpMsg(
						must.NotFail(types.NewDocument(
							"cursor", must.NotFail(types.NewDocument(
								"firstBatch", must.NotFail(types.NewArray(
									must.NotFail(types.NewDocument(
										"_id", info.idValue,
										info.outField, nVal,
									)),
								)),
								"id", int64(0),
								"ns", dbName+"."+cName,
							)),
							"ok", float64(1),
						)),
					)
				}
			}
		}

		// If the target is a view, redirect to the source collection and prepend
		// the view's pipeline to the user-supplied stages.
		if cInfo.IsView {
			viewSourceName := cInfo.ViewOn
			// Reload collection info for the source.
			srcParam := backends.ListCollectionsParams{Name: viewSourceName}
			var srcList *backends.ListCollectionsResult
			if srcList, err = db.ListCollections(ctx, &srcParam); err != nil {
				closer.Close()
				return nil, handleMaxTimeMSError(err, maxTimeMS, "aggregate")
			}
			cInfo = backends.CollectionInfo{}
			if len(srcList.Collections) > 0 {
				cInfo = srcList.Collections[0]
			}
			c, err = db.Collection(viewSourceName)
			if err != nil {
				closer.Close()
				return nil, lazyerrors.Error(err)
			}
			// Prepend view pipeline stages (if any) ahead of the user stages.
			if viewPipeline := cList.Collections[0].ViewPipeline; viewPipeline != nil && viewPipeline.Len() > 0 {
				viewStages, vErr := buildViewPipelineStages(db, viewPipeline)
				if vErr != nil {
					closer.Close()
					return nil, vErr
				}
				stagesDocuments = append(viewStages, stagesDocuments...)
			}
		}

		switch {
		case h.DisablePushdown:
			// Pushdown disabled
		case sort.Len() == 0 && cInfo.Capped():
			// Pushdown default recordID sorting for capped collections
			qp.Sort = must.NotFail(types.NewDocument("$natural", int64(1)))
		case sort.Len() == 1:
			if sort.Keys()[0] != "$natural" {
				break
			}

			qp.Sort = sort
		}

		iter, err = processStagesDocuments(ctx, closer, &stagesDocumentsParams{c, qp, stagesDocuments})
	} else {
		statistics := stages.GetStatistics(collStatsDocuments)

		iter, err = processStagesStats(ctx, closer, &stagesStatsParams{
			c, db, dbName, cName, statistics, collStatsDocuments, h.TCPHost,
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
		Username:   username,
		Type:       cursor.Normal,
	})

	cursorID := cursor.ID

	docs, err := iterator.ConsumeValuesN(cursor, int(batchSize))
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

	if firstBatch.Len() < int(batchSize) {
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
	"$currentOp":         {},
	"$documents":         {},
	"$listLocalSessions": {},
	"$listSessions":      {},
	"$changeStream":      {},
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
	if _, dbOK := dbLevelSourceStages[stageName]; !dbOK {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrInvalidNamespace,
			fmt.Sprintf("{aggregate: 1} is not valid for '%s'; a collection is required.", stageName),
			document.Command(),
		)
	}

	if stageName == "$currentOp" && dbName != "admin" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrInvalidNamespace,
			"$currentOp must be run against the 'admin' database with {aggregate: 1}",
			document.Command(),
		)
	}

	if stageName == "$documents" {
		return h.aggregateDocuments(connCtx, document, firstStage, pipeline, dbName)
	}

	if stageName == "$listLocalSessions" {
		return h.aggregateListLocalSessions(connCtx, dbName)
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
// stage: one document per logical session cached on this node. Each
// session's _id.id is the lsid UUID and _id.uid is the SHA-256 digest of
// the authenticated user (the empty-string digest when unauthenticated),
// matching MongoDB's session identity encoding.
func (h *Handler) aggregateListLocalSessions(connCtx context.Context, dbName string) (*wire.OpMsg, error) {
	firstBatch := types.MakeArray(0)
	if reg := h.SessionRegistry(); reg != nil {
		for _, s := range reg.Snapshot() {
			// The registry key is sessionKey(user, id) == user + "\x00" + id.
			// Only real driver lsids (a 16-byte hex UUID) have a MongoDB
			// counterpart; skip synthetic ids assigned to connections that
			// never supplied an lsid.
			user, id, ok := strings.Cut(s.Lsid, "\x00")
			if !ok {
				continue
			}
			idBytes, err := hex.DecodeString(id)
			if err != nil || len(idBytes) != 16 {
				continue
			}
			uidSum := sha256.Sum256([]byte(user))
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

// aggregateDocuments runs a database-level pipeline whose source stage is
// $documents: an inline array of documents used as the pipeline input. The
// array elements are fed through any remaining pipeline stages and returned
// through the normal cursor/batch machinery.
func (h *Handler) aggregateDocuments(connCtx context.Context, document, firstStage *types.Document, pipeline *types.Array, dbName string) (*wire.OpMsg, error) {
	arr, ok := must.NotFail(firstStage.Get("$documents")).(*types.Array)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"$documents value must be an array of objects",
			document.Command(),
		)
	}

	srcDocs := make([]*types.Document, 0, arr.Len())
	for i := 0; i < arr.Len(); i++ {
		d, ok := must.NotFail(arr.Get(i)).(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"$documents array element must be an object",
				document.Command(),
			)
		}
		srcDocs = append(srcDocs, d)
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
		s, err := stages.NewStage(sd)
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
		Username:   conninfo.Get(connCtx).Username(),
		Type:       cursor.Normal,
	})

	cursorID := cur.ID

	docs, err := iterator.ConsumeValuesN(cur, int(batchSize))
	if err != nil {
		h.cursors.CloseAndRemove(cur)
		return nil, err
	}

	firstBatch := types.MakeArray(len(docs))
	for _, doc := range docs {
		firstBatch.Append(doc)
	}

	if firstBatch.Len() < int(batchSize) {
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
}

// processStagesDocuments retrieves the documents from the database and then processes them through the stages.
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

func processStagesDocuments(ctx context.Context, closer *iterator.MultiCloser, p *stagesDocumentsParams) (types.DocumentsIterator, error) { //nolint:lll // for readability
	queryRes, err := p.c.Query(ctx, p.qp)
	if err != nil {
		closer.Close()
		return nil, lazyerrors.Error(err)
	}

	closer.Add(queryRes.Iter)

	iter := queryRes.Iter

	for _, s := range p.stages {
		if iter, err = s.Process(ctx, iter, closer); err != nil {
			return nil, err
		}
	}

	return iter, nil
}

type stagesStatsParams struct {
	c          backends.Collection
	db         backends.Database
	dbName     string
	cName      string
	statistics map[stages.Statistic]struct{}
	stages     []aggregations.Stage
	tcpHost    string
}

// processStagesStats retrieves the statistics from the database and then processes them through the stages.
func processStagesStats(ctx context.Context, closer *iterator.MultiCloser, p *stagesStatsParams) (types.DocumentsIterator, error) { //nolint:lll // for readability
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
		collStats *backends.CollectionStatsResult
		cInfo     backends.CollectionInfo
		nIndexes  int64
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

		nIndexes = int64(len(iList.Indexes))
	}

	if hasStorage {
		var avgObjSize int32
		if collStats.CountDocuments > 0 {
			avgObjSize = int32(collStats.SizeCollection / collStats.CountDocuments)
		}

		indexSizes := types.MakeDocument(len(collStats.IndexSizes))
		indexDetails := types.MakeDocument(len(collStats.IndexSizes))
		for _, indexSize := range collStats.IndexSizes {
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

// makeOutWriter returns an OutWriter callback that replaces the target collection
// with the provided documents. It drops the existing collection (if any),
// then inserts all documents into a fresh collection.
func makeOutWriter(b backends.Backend, currentDB string) stages.OutWriter { //nolint:unparam // currentDB used by caller
	return func(ctx context.Context, dbName, collName string, docs []*types.Document) error {
		db, err := b.Database(dbName)
		if err != nil {
			return lazyerrors.Error(err)
		}

		// Drop collection if it exists to match MongoDB $out semantics.
		dropErr := db.DropCollection(ctx, &backends.DropCollectionParams{Name: collName})
		if dropErr != nil && !backends.ErrorCodeIs(dropErr, backends.ErrorCodeCollectionDoesNotExist) {
			return lazyerrors.Error(dropErr)
		}

		// InsertAll creates the database and collection automatically if needed.
		coll, err := db.Collection(collName)
		if err != nil {
			return lazyerrors.Error(err)
		}

		if len(docs) == 0 {
			// Create an empty collection explicitly so $out with zero results still leaves a collection.
			if createErr := db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: collName}); createErr != nil {
				if !backends.ErrorCodeIs(createErr, backends.ErrorCodeCollectionAlreadyExists) {
					return lazyerrors.Error(createErr)
				}
			}

			return nil
		}

		_, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: docs})

		return err
	}
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
					_, insertErr := coll.InsertAll(ctx, &backends.InsertAllParams{Docs: params.Docs})
					return insertErr
				}

				return nil
			}

			return lazyerrors.Error(err)
		}

		existingDocs, err := iterator.ConsumeValues(qRes.Iter)
		if err != nil {
			return lazyerrors.Error(err)
		}

		type existingEntry struct {
			doc   *types.Document
			index int
		}

		existingMap := make(map[string]*existingEntry, len(existingDocs))

		for i, existing := range existingDocs {
			key := buildMergeKey(existing, params.On)
			existingMap[key] = &existingEntry{doc: existing, index: i}
		}

		var toInsert []*types.Document
		var toUpdate []*types.Document

		for _, incoming := range params.Docs {
			key := buildMergeKey(incoming, params.On)
			entry, matched := existingMap[key]

			if matched {
				switch params.WhenMatched {
				case "merge":
					// Merge fields from incoming into existing.
					merged := entry.doc
					for _, k := range incoming.Keys() {
						v := must.NotFail(incoming.Get(k))
						merged.Set(k, v)
					}

					toUpdate = append(toUpdate, merged)

				case "replace":
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
						handlererrors.ErrBadValue,
						"$merge with whenMatched: 'fail' found a matching document",
						"$merge (stage)",
					)
				}
			} else {
				switch params.WhenNotMatched {
				case "insert":
					toInsert = append(toInsert, incoming)

				case "discard":
					// Skip this document.

				case "fail":
					return handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrBadValue,
						"$merge with whenNotMatched: 'fail' found an unmatched document",
						"$merge (stage)",
					)
				}
			}
		}

		if len(toInsert) > 0 {
			if _, err = coll.InsertAll(ctx, &backends.InsertAllParams{Docs: toInsert}); err != nil {
				return lazyerrors.Error(err)
			}
		}

		if len(toUpdate) > 0 {
			if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: toUpdate}); err != nil {
				return lazyerrors.Error(err)
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
