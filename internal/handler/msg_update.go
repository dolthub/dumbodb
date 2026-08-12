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
	"time"

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

// MsgUpdate implements `update` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgUpdate(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	params, err := common.GetUpdateParams(document, h.L)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = enforceWritableRootish(params.DB); err != nil {
		return nil, err
	}

	_ = params.Ordered

	ctx := connCtx
	if params.MaxTimeMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(connCtx, time.Duration(params.MaxTimeMS)*time.Millisecond)
		defer cancel()
	}

	matched, modified, upserted, err := h.updateDocument(ctx, params)
	if err != nil {
		if params.MaxTimeMS > 0 && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrMaxTimeMSExpired,
				"Executor error during update command :: caused by :: operation exceeded time limit",
				"update",
			)
		}
		return nil, handleUpdateError(params.DB, params.Collection, "update", err)
	}

	res := must.NotFail(types.NewDocument(
		"n", matched,
	))

	if upserted.Len() != 0 {
		res.Set("upserted", upserted)
	}

	res.Set("nModified", modified)
	res.Set("ok", float64(1))

	return documentOpMsg(
		res,
	)
}

// updateDocument iterate through all documents in collection and update them.
func (h *Handler) updateDocument(ctx context.Context, params *common.UpdateParams) (int32, int32, *types.Array, error) {
	var matched, modified int32
	var upserted types.Array

	db, err := h.b.Database(params.DB)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid namespace specified '%s.%s'", params.DB, params.Collection)
			return 0, 0, nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "update")
		}

		return 0, 0, nil, lazyerrors.Error(err)
	}

	// Check if collection is a view  -- views don't support write operations.
	if collRes, collErr := db.ListCollections(ctx, &backends.ListCollectionsParams{Name: params.Collection}); collErr == nil {
		if len(collRes.Collections) == 1 && collRes.Collections[0].IsView {
			msg := fmt.Sprintf("namespace '%s.%s' is a view, not a collection", params.DB, params.Collection)
			return 0, 0, nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCommandNotSupportedOnView, msg, "update")
		}
	}

	err = db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: params.Collection})

	switch {
	case err == nil:
		// nothing
	case backends.ErrorCodeIs(err, backends.ErrorCodeCollectionAlreadyExists):
		// nothing
	case backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid):
		msg := fmt.Sprintf("Invalid collection name: %s", params.Collection)
		return 0, 0, nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "insert")
	default:
		return 0, 0, nil, lazyerrors.Error(err)
	}

	var validator *types.Document
	var valLevel, valAction string
	if !params.BypassDocumentValidation {
		validator, valLevel, valAction = collectionValidator(ctx, db, params.Collection)
	}

	for _, u := range params.Updates {
		c, err := db.Collection(params.Collection)
		if err != nil {
			if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
				msg := fmt.Sprintf("Invalid collection name: %s", params.Collection)
				return 0, 0, nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "insert")
			}

			return 0, 0, nil, lazyerrors.Error(err)
		}

		u.Validator = validator
		u.ValidationLevel = valLevel
		u.ValidationAction = valAction

		cmp := collation.Parse(u.Collation).Comparator()

		var qp backends.QueryParams
		qp.Collated = cmp != nil
		if !h.DisablePushdown {
			qp.Filter = u.Filter
		}

		res, err := c.Query(ctx, &qp)
		if err != nil {
			return 0, 0, nil, lazyerrors.Error(err)
		}

		closer := iterator.NewMultiCloser()
		defer closer.Close()

		closer.Add(res.Iter)

		iter := common.FilterIteratorColl(res.Iter, closer, u.Filter, cmp)

		if !u.Multi {
			iter = common.LimitIterator(iter, closer, 1)
		}

		result, err := common.UpdateDocument(ctx, c, "update", iter, &u, params.SkipDurableSync)
		if err != nil {
			if backends.ErrorCodeIs(err, backends.ErrorCodeReadOnlyDatabase) {
				return 0, 0, nil, handlererrors.NewCommandErrorMsg(
					handlererrors.ErrOperationFailed,
					"cannot write to a read-only database snapshot",
				)
			}
			if backends.ErrorCodeIs(err, backends.ErrorCodeWriteConflict) {
				return 0, 0, nil, common.TranslateBackendWriteError(err)
			}
			return 0, 0, nil, lazyerrors.Error(err)
		}

		matched += result.Matched.Count
		modified += result.Modified.Count

		if result.WarnAllowed > 0 {
			h.L.Warn("documents allowed despite failing validation (validationAction:warn)",
				"collection", params.Collection, "count", result.WarnAllowed)
		}

		if result.Upserted.Doc != nil {
			doc := result.Upserted.Doc
			upserted.Append(must.NotFail(types.NewDocument(
				"index", int32(upserted.Len()),
				"_id", must.NotFail(doc.Get("_id")),
			)))

			// in case of upsert, MongoDB sets the matched count to 1
			matched++
		}
	}

	return matched, modified, &upserted, nil
}
