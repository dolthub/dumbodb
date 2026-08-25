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

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
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

	// Validate rootish before backend access so invalid forms (HEAD, reflog, range)
	// return OperationFailed (96) rather than silently succeeding or returning
	// InvalidNamespace (73) from MongoDB's own namespace check.
	if _, _, _, err := branchFromDBName(params.DB); err != nil {
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
	params.Collation = h.effectiveCollation(connCtx, db, params.Collection, params.Collation)

	cmp := collation.Parse(params.Collation).Comparator()

	collParam := backends.ListCollectionsParams{Name: params.Collection}
	cList, err := db.ListCollections(connCtx, &collParam)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if len(cList.Collections) > 0 && cList.Collections[0].IsView {
		view := cList.Collections[0]

		closer := iterator.NewMultiCloser()
		defer closer.Close()

		iter, verr := viewSourceIterator(connCtx, db, view.Name, view.ViewOn, view.ViewPipeline, closer, h.DisablePushdown, h.EnableNestedPushdown)
		if verr != nil {
			return nil, verr
		}

		iter = common.FilterIteratorColl(iter, closer, params.Filter, cmp)
		iter = common.SkipIterator(iter, closer, params.Skip)
		iter = common.LimitIterator(iter, closer, params.Limit)
		iter = common.CountIterator(iter, closer, "count")

		_, res, cerr := iter.Next()
		if cerr != nil && !errors.Is(cerr, iterator.ErrIteratorDone) {
			return nil, lazyerrors.Error(cerr)
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

	// Fast path: unfiltered count. The backend can return the entry count from
	// tree metadata in O(1) instead of scanning every document.
	if params.Filter.Len() == 0 {
		countRes, err := c.Count(connCtx, &backends.CountParams{})
		if err != nil {
			return nil, lazyerrors.Error(err)
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

		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"n", int32(total),
				"ok", float64(1),
			)),
		)
	}

	// Fast path: ask the backend whether it can satisfy the filtered count
	// without fetching documents (e.g. via a covering single-field index).
	// The backend signals success via Filtered=true; otherwise we fall
	// through to the scan path below.
	if !h.DisablePushdown && cmp == nil {
		countRes, cerr := c.Count(connCtx, &backends.CountParams{Filter: params.Filter, Collation: params.Collation})
		if cerr != nil {
			return nil, lazyerrors.Error(cerr)
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

			return documentOpMsg(
				must.NotFail(types.NewDocument(
					"n", int32(total),
					"ok", float64(1),
				)),
			)
		}
	}

	var qp backends.QueryParams
	qp.Collated = cmp != nil
	if !h.DisablePushdown {
		qp.Filter = params.Filter
	}

	queryRes, err := c.Query(connCtx, &qp)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	iter := queryRes.Iter

	closer := iterator.NewMultiCloser(iter)
	defer closer.Close()

	iter = common.FilterIteratorColl(iter, closer, params.Filter, cmp)

	iter = common.SkipIterator(iter, closer, params.Skip)

	iter = common.LimitIterator(iter, closer, params.Limit)

	iter = common.CountIterator(iter, closer, "count")

	_, res, err := iter.Next()
	if errors.Is(err, iterator.ErrIteratorDone) {
		err = nil
	}

	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	count, _ := res.Get("count")
	n, _ := count.(int32)

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"n", n,
			"ok", float64(1),
		)),
	)
}
