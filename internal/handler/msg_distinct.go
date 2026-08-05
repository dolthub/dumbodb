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
	"fmt"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgDistinct implements `distinct` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDistinct(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	params, err := common.GetDistinctParams(document, h.L)
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
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, document.Command())
		}

		return nil, lazyerrors.Error(err)
	}

	viewInfo, err := lookupCollectionInfo(connCtx, db, params.Collection)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}
	if viewInfo != nil && viewInfo.IsView {
		closer := iterator.NewMultiCloser()
		defer closer.Close()

		iter, verr := viewSourceIterator(connCtx, db, viewInfo.Name, viewInfo.ViewOn, viewInfo.ViewPipeline, closer)
		if verr != nil {
			return nil, verr
		}
		iter = common.FilterIterator(iter, closer, params.Filter)

		distinct, derr := common.FilterDistinctValues(iter, params.Key)
		if derr != nil {
			return nil, lazyerrors.Error(derr)
		}
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"values", distinct,
				"ok", float64(1),
			)),
		)
	}

	c, err := db.Collection(params.Collection)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
			msg := fmt.Sprintf("Invalid collection name: %s", params.Collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, document.Command())
		}

		return nil, lazyerrors.Error(err)
	}

	// Fast path: when the request has no filter and the backend exposes a
	// DistinctScanner, let it serve the query from a secondary index without
	// reading every document.
	if params.Filter.Len() == 0 {
		if ds, ok := c.(backends.DistinctScanner); ok {
			res, err := ds.DistinctScan(connCtx, &backends.DistinctParams{Key: params.Key})
			if err != nil {
				return nil, lazyerrors.Error(err)
			}
			if res != nil {
				distinct, err := common.DedupDistinctValues(res.Values)
				if err != nil {
					return nil, lazyerrors.Error(err)
				}
				return documentOpMsg(
					must.NotFail(types.NewDocument(
						"values", distinct,
						"ok", float64(1),
					)),
				)
			}
		}
	}

	closer := iterator.NewMultiCloser()
	defer closer.Close()

	var qp backends.QueryParams
	if !h.DisablePushdown {
		qp.Filter = params.Filter
	}

	queryRes, err := c.Query(connCtx, &qp)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	closer.Add(queryRes.Iter)

	iter := common.FilterIterator(queryRes.Iter, closer, params.Filter)

	distinct, err := common.FilterDistinctValues(iter, params.Key)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"values", distinct,
			"ok", float64(1),
		)),
	)
}
