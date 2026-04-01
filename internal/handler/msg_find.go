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

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/clientconn/conninfo"
	"github.com/dolthub/dongo/internal/clientconn/cursor"
	"github.com/dolthub/dongo/internal/handler/common"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
	"github.com/dolthub/dongo/internal/util/must"
)

// MsgFind implements `find` command.
//
// The passed context is canceled when the client connection is closed.
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

	// If the target is a view, redirect to the source collection for reading.
	if cInfo.IsView {
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
		// Re-obtain the collection object for the source collection.
		coll, err = db.Collection(params.Collection)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}
	}

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

	// Validate geospatial operators in the filter before executing (query-time validation).
	if err = common.ValidateGeoFilter(params.Filter); err != nil {
		return nil, err
	}

	qp, err := h.makeFindQueryParams(connCtx, params, &cInfo)
	if err != nil {
		return nil, err
	}

	ctx := connCtx
	cancel := func() {}

	// TODO https://github.com/dolthub/dongo/issues/2983
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

	queryRes, err := coll.Query(ctx, qp)
	if err != nil {
		return nil, handleMaxTimeMSError(err, params.MaxTimeMS, "find")
	}

	// closer accumulates all things that should be closed / canceled.
	closer := iterator.NewMultiCloser(iterator.CloserFunc(cancel))

	iter, err := h.makeFindIter(queryRes.Iter, closer, params)
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
		return nil, handleMaxTimeMSError(err, params.MaxTimeMS, "find")
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

		// let the client know that there are no more results
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

// makeFindQueryParams creates the backend's query parameters for the find command.
func (h *Handler) makeFindQueryParams(ctx context.Context, params *common.FindParams, cInfo *backends.CollectionInfo) (*backends.QueryParams, error) { //nolint:lll // for readability
	qp := &backends.QueryParams{
		Comment: params.Comment,
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
		// Pushdown disabled
	case params.Sort.Len() == 0 && cInfo.Capped():
		// Pushdown default recordID sorting for capped collections
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

// makeFindIter creates an iterator chain for the find command.
//
// Iter is passed from the backend's query.
// All iterators, including the initial one, are added to the passed closer,
// and the returned iterator is wrapped with it.
//
//nolint:lll // for readability
func (h *Handler) makeFindIter(iter types.DocumentsIterator, closer *iterator.MultiCloser, params *common.FindParams) (types.DocumentsIterator, error) {
	closer.Add(iter)

	// When a case-insensitive collation is active, transform string equality
	// filters into case-insensitive regex matches before applying the filter.
	filterDoc := params.Filter
	caseInsensitive := params.ParsedCollation.CaseInsensitive()

	if caseInsensitive {
		filterDoc = common.TransformFilterForCollation(params.Filter, params.ParsedCollation)
	}

	iter = common.FilterIterator(iter, closer, filterDoc)

	// Apply min/max index bounds filter if specified (used with hint to constrain index scan range).
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
		iter, sortErr = common.SortIteratorWithCollation(iter, closer, params.Sort, caseInsensitive)
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

	// If returnKey is true, project only the index key fields (from hint) instead of regular projection.
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

// buildReturnKeyProjection creates a projection document that includes only the
// fields specified in the hint (index key fields), with _id excluded.
// This is used when returnKey=true is set in a find command.
func buildReturnKeyProjection(hint any) *types.Document {
	hintDoc, ok := hint.(*types.Document)
	if !ok || hintDoc == nil {
		return must.NotFail(types.NewDocument("_id", int32(0)))
	}

	pairs := make([]any, 0, hintDoc.Len()*2+2)
	for _, field := range hintDoc.Keys() {
		pairs = append(pairs, field, int32(1))
	}
	// Exclude _id by default for returnKey projections.
	pairs = append(pairs, "_id", int32(0))
	return must.NotFail(types.NewDocument(pairs...))
}

// minMaxBoundsIterator wraps an iterator and filters out documents that fall
// outside the min/max index bounds. min is inclusive, max is exclusive.
// If hintDoc is provided, only its fields are checked; otherwise all fields
// in min/max documents are checked.
func minMaxBoundsIterator(iter types.DocumentsIterator, closer *iterator.MultiCloser, hintDoc, minDoc, maxDoc *types.Document) types.DocumentsIterator {
	res := &minMaxIter{iter: iter, hintDoc: hintDoc, minDoc: minDoc, maxDoc: maxDoc}
	closer.Add(res)
	return res
}

// minMaxIter filters documents based on min/max index bounds.
type minMaxIter struct {
	iter    types.DocumentsIterator
	hintDoc *types.Document
	minDoc  *types.Document
	maxDoc  *types.Document
}

// Next implements iterator.Interface.
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

// Close implements iterator.Interface.
func (it *minMaxIter) Close() {
	it.iter.Close()
}

// matchesBounds returns true if the document satisfies the min/max bounds.
// min is inclusive, max is exclusive.
func (it *minMaxIter) matchesBounds(doc *types.Document) bool {
	// Determine which fields to check: use hint fields if available,
	// otherwise use all fields from min/max documents.
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
			// Field missing from document — doesn't satisfy bounds.
			return false
		}

		if it.minDoc != nil {
			minVal, err := it.minDoc.Get(field)
			if err == nil {
				// min is inclusive: docVal >= minVal
				if cmp := types.Compare(docVal, minVal); cmp == types.Less {
					return false
				}
			}
		}

		if it.maxDoc != nil {
			maxVal, err := it.maxDoc.Get(field)
			if err == nil {
				// max is exclusive: docVal < maxVal
				if cmp := types.Compare(docVal, maxVal); cmp != types.Less {
					return false
				}
			}
		}
	}

	return true
}

// check interfaces
var _ types.DocumentsIterator = (*minMaxIter)(nil)

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
