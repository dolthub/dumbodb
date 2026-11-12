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
	"strings"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) MsgCount(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	params, err := common.GetCountParams(document, h.L)
	if err != nil {
		return nil, err
	}

	connCtx, cancel := withMaxTimeMS(connCtx, params.MaxTimeMS)
	defer cancel()

	// Validate rootish before backend access so invalid forms (HEAD, reflog, range)
	// return OperationFailed (96) rather than silently succeeding or returning
	// InvalidNamespace (73) from MongoDB's own namespace check.
	if _, _, _, err := branchFromDBName(params.DB); err != nil {
		return nil, err
	}

	common.CanonicalizeFilter(params.Filter)
	params.Filter = operators.BindNowInFilter(params.Filter, operators.CommandNow())
	if err := common.ValidateFilter(params.Filter, nil); err != nil {
		return nil, err
	}

	db, err := h.b.Database(params.DB)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid namespace specified '%s.%s'", params.DB, params.Collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "count")
		}

		return nil, lazyerrors.Error(err)
	}

	c, err := db.Collection(params.Collection)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
			msg := fmt.Sprintf("Invalid collection name: %s", params.Collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "count")
		}

		return nil, lazyerrors.Error(err)
	}

	// A view has no backing store of its own: count runs as an aggregation over
	// the view's source with the view's defining pipeline applied, then the
	// filter/skip/limit. This bypasses the backend fast paths below, which would
	// count the (empty) view collection and return 0.
	collParam := backends.ListCollectionsParams{Name: params.Collection}
	cList, err := db.ListCollections(connCtx, &collParam)
	if err != nil {
		return nil, handleMaxTimeMSError(err, params.MaxTimeMS, "count")
	}

	isView := len(cList.Collections) > 0 && cList.Collections[0].IsView
	if params.Hint != nil {
		if len(cList.Collections) > 0 {
			params.Hint = clusteredHint(params.Hint, cList.Collections[0].ClusteredIndexName)
		}
		if err = checkHintType(params.Hint); err == nil && !isView {
			err = validateHintExists(connCtx, c, params.Hint, "count")
		}
		if err == nil && isView {
			err = validateViewHint(connCtx, db, &cList.Collections[0], params.Hint, "count", params.Filter.Len() > 0)
		}
		if err != nil {
			return nil, err
		}
	}
	if isView {
		if params.Collation, err = viewReadCollation(params.Collation, cList.Collections[0].Collation); err != nil {
			return nil, err
		}
	} else {
		params.Collation = h.effectiveCollation(connCtx, db, params.Collection, params.Collation)
	}

	cmp := collation.Parse(params.Collation).Comparator()

	if isView {
		view := cList.Collections[0]

		closer := iterator.NewMultiCloser()
		defer closer.Close()

		// mongod runs the count as the view's stages, its $match, $skip and
		// $limit, and a $group and $project that count.
		iter, verr := viewSourceIterator(withDBName(connCtx, params.DB), db, view.Name, view.ViewOn, view.ViewPipeline, cmp, closer, h.DisablePushdown,
			viewPipelineLength(2+boolInt(params.Filter.Len() > 0)+boolInt(params.Skip > 0)+boolInt(params.Limit != 0)))
		if verr != nil {
			return nil, verr
		}

		iter = common.FilterIteratorColl(iterator.WithContext(connCtx, iter), closer, params.Filter, cmp)
		iter = common.SkipIterator(iter, closer, params.Skip)
		iter = common.LimitIterator(iter, closer, params.Limit)
		iter = common.CountIterator(iter, closer, "count")

		_, res, cerr := iter.Next()
		if cerr != nil && !errors.Is(cerr, iterator.ErrIteratorDone) {
			return nil, handleMaxTimeMSError(cerr, params.MaxTimeMS, "count")
		}

		// CountIterator yields ErrIteratorDone with a nil document when nothing
		// matched (count 0); a match yields a {count: n} document.
		var n int32
		if res != nil {
			count, _ := res.Get("count")
			n, _ = count.(int32)
		}

		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"n", n,
				"ok", float64(1),
			)),
		)
	}

	n, err := h.execCount(connCtx, c, params, cmp)
	if err != nil {
		return nil, err
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"n", n,
			"ok", float64(1),
		)),
	)
}

// execCount counts the documents of collection c that match params, by the
// fastest path available. cmp is params' collation comparator, nil when simple.
func (h *Handler) execCount(ctx context.Context, c backends.Collection, params *common.CountParams, cmp *collation.Comparator) (int32, error) {
	n, _, err := h.execCountReading(ctx, c, params, cmp)
	return n, err
}

// execCountReading is execCount that also returns, when it fetched documents,
// how many matched before the skip: what explain's FETCH returns. It is zero
// when the count came from an index.
func (h *Handler) execCountReading(ctx context.Context, c backends.Collection, params *common.CountParams, cmp *collation.Comparator) (n, read int32, err error) {
	ctx = backends.WithCountScan(ctx)
	// A hinted index decides which documents are counted (a sparse one holds
	// only some), so a hint skips the unfiltered shortcut.
	hinted := params.Hint != nil && !backends.HintIsNatural(params.Hint)

	// Fast path: unfiltered count. The backend can return the entry count from
	// tree metadata in O(1) instead of scanning every document.
	if params.Filter.Len() == 0 && !hinted {
		countRes, err := c.Count(ctx, &backends.CountParams{})
		if err != nil {
			return 0, 0, lazyerrors.Error(err)
		}

		total := countRes.Count
		if params.Skip > 0 {
			total -= params.Skip
			if total < 0 {
				total = 0
			}
		}
		if params.Limit > 0 && total > params.Limit {
			total = params.Limit
		}

		return int32(total), 0, nil
	}

	// Fast path: ask the backend whether it can satisfy the filtered count
	// without fetching documents (e.g. via a covering single-field index).
	// The backend signals success via Filtered=true; otherwise we fall
	// through to the scan path below.
	// A hint still allows the index count when it names the index the count
	// would read; the backend declines any other. Unfiltered, a hint counts
	// the index's entries.
	if !h.DisablePushdown && (cmp == nil || filterCollationFree(params.Filter)) && (params.Filter.Len() > 0 || hinted) {
		stop := int64(0)
		if params.Limit > 0 {
			stop = params.Skip + params.Limit
		}
		countRes, cerr := c.Count(ctx, &backends.CountParams{Filter: params.Filter, Collation: params.Collation, Stop: stop, Hint: params.Hint})
		if cerr != nil {
			return 0, 0, handleMaxTimeMSError(cerr, params.MaxTimeMS, "count")
		}
		if countRes.Filtered {
			total := countRes.Count
			if params.Skip > 0 {
				total -= params.Skip
				if total < 0 {
					total = 0
				}
			}
			if params.Limit > 0 && total > params.Limit {
				total = params.Limit
			}

			return int32(total), 0, nil
		}
	}

	var qp backends.QueryParams
	qp.Collated = cmp != nil
	qp.Hint = params.Hint
	if !h.DisablePushdown {
		qp.Filter = params.Filter
	}

	queryRes, err := c.Query(ctx, &qp)
	if err != nil {
		return 0, 0, handleMaxTimeMSError(err, params.MaxTimeMS, "count")
	}

	iter := iterator.WithContext(ctx, queryRes.Iter)

	closer := iterator.NewMultiCloser(queryRes.Iter)
	defer closer.Close()

	iter = common.FilterIteratorColl(iter, closer, params.Filter, cmp)

	matched := &countingIterator{DocumentsIterator: iter}
	iter = common.SkipIterator(matched, closer, params.Skip)

	iter = common.LimitIterator(iter, closer, params.Limit)

	iter = common.CountIterator(iter, closer, "count")

	_, res, err := iter.Next()
	if errors.Is(err, iterator.ErrIteratorDone) {
		err = nil
	}

	if err != nil {
		return 0, 0, handleMaxTimeMSError(err, params.MaxTimeMS, "count")
	}

	count, _ := res.Get("count")
	n, _ = count.(int32)
	return n, matched.n, nil
}

// filterCollationFree reports whether filter compares no string, so an index
// of any collation counts it as the query's collation would.
func filterCollationFree(filter *types.Document) bool {
	if filter.Len() == 0 {
		return false
	}
	for _, k := range filter.Keys() {
		if strings.HasPrefix(k, "$") || !backends.CollationFree(must.NotFail(filter.Get(k))) {
			return false
		}
	}
	return true
}

// countingIterator counts the documents it yields.
type countingIterator struct {
	types.DocumentsIterator
	n int32
}

func (it *countingIterator) Next() (struct{}, *types.Document, error) {
	zero, doc, err := it.DocumentsIterator.Next()
	if err == nil {
		it.n++
	}
	return zero, doc, err
}
