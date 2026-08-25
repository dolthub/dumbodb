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
	"log/slog"
	"strings"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/clientconn/cursor"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) MsgFind(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	params, err := common.GetFindParams(document, h.L)
	if err != nil {
		return nil, err
	}

	// Validate rootish before backend access so invalid forms (HEAD, reflog, range)
	// return OperationFailed (96) rather than silently succeeding or returning
	// InvalidNamespace (73) from MongoDB's own namespace check.
	if _, _, _, err := branchFromDBName(params.DB); err != nil {
		return nil, err
	}

	username := conninfo.Get(connCtx).Username()

	db, err := h.b.Database(params.DB)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid namespace specified '%s.%s'", params.DB, params.Collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "find")
		}

		return nil, lazyerrors.Error(err)
	}

	coll, err := db.Collection(params.Collection)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
			msg := fmt.Sprintf("Invalid collection name: %s", params.Collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "find")
		}

		return nil, lazyerrors.Error(err)
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

	// If the target is a view, the read runs as an aggregation over the view's
	// source collection with the view's defining pipeline applied; the find's
	// own filter/sort/skip/limit/projection are layered on top below.
	var (
		isView       bool
		viewName     string
		viewOn       string
		viewPipeline *types.Array
	)

	if cInfo.IsView {
		isView = true
		viewName = cInfo.Name
		viewOn = cInfo.ViewOn
		viewPipeline = cInfo.ViewPipeline

		params.Collection = cInfo.ViewOn
		viewSourceParam := backends.ListCollectionsParams{Name: cInfo.ViewOn}
		var srcList *backends.ListCollectionsResult
		if srcList, err = db.ListCollections(connCtx, &viewSourceParam); err != nil {
			return nil, err
		}
		cInfo = backends.CollectionInfo{}
		if len(srcList.Collections) > 0 {
			cInfo = srcList.Collections[0]
		}
		coll, err = db.Collection(params.Collection)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
	}

	// Validate the operation's own collation (MongoDB rejects, for example, a
	// locale whose tailored caseFirst/backwards conflicts with a low strength)
	// before resolving it against the collection default.
	if err = validateOpCollation(params.Collation, "find"); err != nil {
		return nil, err
	}

	// Resolve the effective collation: a find with no collation of its own
	// inherits the collection's default.
	params.Collation = collation.Effective(params.Collation, cInfo.Collation)

	capped := cInfo.Capped()
	if params.Tailable {
		if !capped {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"tailable cursor requested on non capped collection",
				"tailable",
			)
		}
	}

	if err = common.ValidateGeoFilter(params.Filter); err != nil {
		return nil, err
	}

	// A hint naming an index that does not exist is an error, matching MongoDB.
	if !isView {
		if err = validateHintExists(connCtx, coll, params.Hint, "find"); err != nil {
			return nil, err
		}
	}

	qp, err := h.makeFindQueryParams(connCtx, params, &cInfo)
	if err != nil {
		return nil, err
	}

	ctx := connCtx
	cancel := func() {}

	if params.MaxTimeMS != 0 {
		findDone := make(chan struct{})
		defer close(findDone)

		ctx, cancel = context.WithCancel(ctx)

		go func() {
			t := time.NewTimer(time.Duration(params.MaxTimeMS) * time.Millisecond)
			defer t.Stop()

			select {
			case <-t.C:
				cancel()
			case <-findDone:
			}
		}()
	}

	closer := iterator.NewMultiCloser(iterator.CloserFunc(cancel))

	var srcIter types.DocumentsIterator
	if isView {
		srcIter, err = viewSourceIterator(ctx, db, viewName, viewOn, viewPipeline, closer, h.DisablePushdown, h.EnableNestedPushdown)
	} else {
		var queryRes *backends.QueryResult
		if queryRes, err = coll.Query(ctx, qp); err != nil {
			return nil, handleMaxTimeMSError(err, params.MaxTimeMS, "find")
		}
		srcIter = queryRes.Iter
	}

	if err != nil {
		return nil, handleMaxTimeMSError(err, params.MaxTimeMS, "find")
	}

	iter, err := h.makeFindIter(srcIter, closer, params)
	if err != nil {
		return nil, handleMaxTimeMSError(err, params.MaxTimeMS, "find")
	}

	t := cursor.Normal

	if params.Tailable {
		t = cursor.Tailable
	}

	if params.AwaitData {
		t = cursor.TailableAwait
	}

	c := h.cursors.NewCursor(ctx, iter, &cursor.NewParams{
		Data: &findCursorData{
			coll:       coll,
			qp:         qp,
			findParams: params,
		},
		DB:           params.DB,
		Collection:   params.Collection,
		Username:     username,
		Type:         t,
		ShowRecordID: params.ShowRecordId,
	})

	cursorID := c.ID

	docs, err := iterator.ConsumeValuesN(c, int(params.BatchSize))
	if err != nil {
		h.cursors.CloseAndRemove(c)
		return nil, wrapFindExecutorError(handleMaxTimeMSError(err, params.MaxTimeMS, "find"), params.DB+"."+params.Collection)
	}

	h.L.DebugContext(
		ctx,
		"Got first batch",
		slog.Int64("cursor_id", cursorID),
		slog.String("type", c.Type.String()),
		slog.Int("count", len(docs)),
		slog.Int64("batch_size", params.BatchSize),
		slog.Bool("single_batch", params.SingleBatch),
	)

	if params.SingleBatch || len(docs) < int(params.BatchSize) {
		c.Close()

		// It is not entirely clear if we should do that; more tests are needed.
		if c.Type != cursor.Normal {
			h.cursors.CloseAndRemove(c)
		}

		cursorID = 0
	}

	firstBatch := types.MakeArray(len(docs))
	for _, doc := range docs {
		firstBatch.Append(doc)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"cursor", must.NotFail(types.NewDocument(
				"firstBatch", firstBatch,
				"id", cursorID,
				"ns", params.DB+"."+params.Collection,
			)),
			"ok", float64(1),
		)),
	)
}

type findCursorData struct {
	coll       backends.Collection
	qp         *backends.QueryParams
	findParams *common.FindParams
}

func (h *Handler) makeFindQueryParams(ctx context.Context, params *common.FindParams, cInfo *backends.CollectionInfo) (*backends.QueryParams, error) { //nolint:lll // for readability
	qp := &backends.QueryParams{
		Comment:   params.Comment,
		Collated:  !collation.Parse(params.Collation).IsSimple(),
		Collation: params.Collation,
		Hint:      params.Hint,
	}

	var err error
	if params.Filter != nil {
		if qp.Comment, err = common.GetOptionalParam(params.Filter, "$comment", qp.Comment); err != nil {
			return nil, err
		}
	}

	if !h.DisablePushdown {
		qp.Filter = params.Filter
	}

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
				"find",
			)
		}

		return nil, err
	}

	switch {
	case h.DisablePushdown:
	case params.Sort.Len() == 0 && cInfo.Capped():
		// Capped collections default to $natural (recordID) order.
		qp.Sort = must.NotFail(types.NewDocument("$natural", int64(1)))
	case params.Sort.Len() == 1:
		if params.Sort.Keys()[0] != "$natural" {
			break
		}

		qp.Sort = params.Sort
	}

	// Limit pushdown is not applied if:
	//  - pushdown is disabled;
	//  - `filter` is set, it must fetch all documents to filter them in memory;
	//  - `sort` is set, it must fetch all documents and sort them in memory;
	//  - `skip` is non-zero value, skip pushdown is not supported yet.
	if !h.DisablePushdown && params.Filter.Len() == 0 && params.Sort.Len() == 0 && params.Skip == 0 {
		qp.Limit = params.Limit
	}

	h.L.DebugContext(ctx, fmt.Sprintf("Converted %+v for %+v to %+v.", params, cInfo, qp))

	return qp, nil
}

// makeFindIter builds the find iterator chain. All iterators, including the
// initial one, are added to the passed closer, and the returned iterator is
// wrapped with it.
//
//nolint:lll // for readability
func (h *Handler) makeFindIter(iter types.DocumentsIterator, closer *iterator.MultiCloser, params *common.FindParams) (types.DocumentsIterator, error) {
	closer.Add(iter)

	// A non-simple collation compares strings locale-aware in both the filter
	// and the sort.
	filterDoc := params.Filter
	cmp := collation.Parse(params.Collation).Comparator()

	iter = common.FilterIteratorColl(iter, closer, filterDoc, cmp)

	if params.Min != nil || params.Max != nil {
		hintDoc, _ := params.Hint.(*types.Document)
		iter = minMaxBoundsIterator(iter, closer, hintDoc, params.Min, params.Max)
	}

	// If the filter contains $near or $nearSphere, sort results by geo distance.
	// Otherwise use the regular sort (with collation-aware comparison if needed).
	var sortErr error
	if geoSort := common.FindGeoSortKey(filterDoc); geoSort != nil {
		iter, sortErr = common.GeoDistanceSortIterator(iter, closer, geoSort)
	} else {
		iter, sortErr = common.SortIteratorWithCollation(iter, closer, params.Sort, cmp)
	}

	if sortErr != nil {
		closer.Close()

		var pathErr *types.PathError
		if errors.As(sortErr, &pathErr) && pathErr.Code() == types.ErrPathElementEmpty {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrPathContainsEmptyElement,
				"Empty field names in path are not allowed",
				"find",
			)
		}

		return nil, lazyerrors.Error(sortErr)
	}

	iter = common.SkipIterator(iter, closer, params.Skip)

	iter = common.LimitIterator(iter, closer, params.Limit)

	if params.ReturnKey {
		keyProj := buildReturnKeyProjection(params.Hint)
		var err error
		if iter, err = common.ProjectionIterator(iter, closer, keyProj, params.Filter); err != nil {
			closer.Close()
			return nil, lazyerrors.Error(err)
		}
		return iterator.WithClose(iter, closer.Close), nil
	}

	var err error
	if iter, err = common.ProjectionIterator(iter, closer, params.Projection, params.Filter); err != nil {
		closer.Close()
		return nil, lazyerrors.Error(err)
	}

	return iterator.WithClose(iter, closer.Close), nil
}

// buildReturnKeyProjection projects only the hint's index key fields, _id excluded.
func buildReturnKeyProjection(hint any) *types.Document {
	hintDoc, ok := hint.(*types.Document)
	if !ok || hintDoc == nil {
		return must.NotFail(types.NewDocument("_id", int32(0)))
	}

	pairs := make([]any, 0, hintDoc.Len()*2+2)
	for _, field := range hintDoc.Keys() {
		pairs = append(pairs, field, int32(1))
	}
	pairs = append(pairs, "_id", int32(0))
	return must.NotFail(types.NewDocument(pairs...))
}

// minMaxBoundsIterator filters out documents outside the min/max index bounds
// (min inclusive, max exclusive). If hintDoc is non-empty only its fields are
// checked; otherwise all fields in the min/max documents are.
func minMaxBoundsIterator(iter types.DocumentsIterator, closer *iterator.MultiCloser, hintDoc, minDoc, maxDoc *types.Document) types.DocumentsIterator {
	res := &minMaxIter{iter: iter, hintDoc: hintDoc, minDoc: minDoc, maxDoc: maxDoc}
	closer.Add(res)
	return res
}

type minMaxIter struct {
	iter    types.DocumentsIterator
	hintDoc *types.Document
	minDoc  *types.Document
	maxDoc  *types.Document
}

func (it *minMaxIter) Next() (struct{}, *types.Document, error) {
	var unused struct{}

	for {
		_, doc, err := it.iter.Next()
		if err != nil {
			return unused, nil, err
		}

		if it.matchesBounds(doc) {
			return unused, doc, nil
		}
	}
}

func (it *minMaxIter) Close() {
	it.iter.Close()
}

func (it *minMaxIter) matchesBounds(doc *types.Document) bool {
	var fields []string
	if it.hintDoc != nil && it.hintDoc.Len() > 0 {
		fields = it.hintDoc.Keys()
	} else {
		seen := make(map[string]bool)
		if it.minDoc != nil {
			for _, f := range it.minDoc.Keys() {
				if !seen[f] {
					fields = append(fields, f)
					seen[f] = true
				}
			}
		}
		if it.maxDoc != nil {
			for _, f := range it.maxDoc.Keys() {
				if !seen[f] {
					fields = append(fields, f)
					seen[f] = true
				}
			}
		}
	}

	for _, field := range fields {
		docVal, err := doc.Get(field)
		if err != nil {
			// A field missing from the document does not satisfy the bounds.
			return false
		}

		if it.minDoc != nil {
			minVal, err := it.minDoc.Get(field)
			if err == nil {
				if cmp := types.Compare(docVal, minVal); cmp == types.Less {
					return false
				}
			}
		}

		if it.maxDoc != nil {
			maxVal, err := it.maxDoc.Get(field)
			if err == nil {
				if cmp := types.Compare(docVal, maxVal); cmp != types.Less {
					return false
				}
			}
		}
	}

	return true
}

var _ types.DocumentsIterator = (*minMaxIter)(nil)

func wrapFindExecutorError(err error, ns string) error {
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
	if strings.Contains(inner, "Executor error during") {
		return err
	}
	return handlererrors.NewCommandErrorMsg(
		cmdErr.Code(),
		"Executor error during find command: "+ns+" :: caused by :: "+inner,
	)
}

var executorWrapSkip = map[handlererrors.ErrorCode]bool{
	handlererrors.ErrBadValue:               true,
	handlererrors.ErrFailedToParse:          true,
	handlererrors.ErrGroupInvalidFieldPath:  true,
	handlererrors.ErrGroupUndefinedVariable: true,
	// A malformed query (e.g. an invalid $jsonSchema structure) is a
	// validation error MongoDB reports at parse time, bare, rather than
	// wrapped in "Executor error during find command".
	handlererrors.ErrTypeMismatch: true,
}

// handleMaxTimeMSError returns the MaxTimeMSExpired error if provided error is a result of context cancellation.
// The MaxTimeMSExpired error won't be returned if maxTimeMS wasn't set.
func handleMaxTimeMSError(err error, maxTimeMS int64, cmd string) error {
	switch {
	case err == nil:
		return nil
	case maxTimeMS != 0 && errors.Is(err, context.Canceled):
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrMaxTimeMSExpired,
			"Executor error during "+cmd+" command :: caused by :: operation exceeded time limit",
			cmd,
		)
	default:
		return lazyerrors.Error(err)
	}
}
