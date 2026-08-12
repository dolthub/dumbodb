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
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgDelete implements `delete` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDelete(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	params, err := common.GetDeleteParams(document, h.L)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = enforceWritableRootish(params.DB); err != nil {
		return nil, err
	}

	db, err := h.b.Database(params.DB)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid namespace specified '%s.%s'", params.DB, params.Collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "delete")
		}

		return nil, lazyerrors.Error(err)
	}

	// Check if collection is a view  -- views don't support write operations.
	if collRes, collErr := db.ListCollections(connCtx, &backends.ListCollectionsParams{Name: params.Collection}); collErr == nil {
		if len(collRes.Collections) == 1 && collRes.Collections[0].IsView {
			msg := fmt.Sprintf("namespace '%s.%s' is a view, not a collection", params.DB, params.Collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCommandNotSupportedOnView, msg, "delete")
		}
	}

	c, err := db.Collection(params.Collection)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
			msg := fmt.Sprintf("Invalid collection name: %s", params.Collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "delete")
		}

		return nil, lazyerrors.Error(err)
	}

	var deleted int32
	writeErrors := types.MakeArray(0)

	for i, p := range params.Deletes {
		p.Collation = h.effectiveCollation(connCtx, db, params.Collection, p.Collation)
		var d int32
		d, err = h.execDelete(connCtx, c, &p, params.SkipDurableSync)

		deleted += d

		if err != nil {
			var ce *handlererrors.CommandError
			if errors.As(err, &ce) {
				we := &mongo.WriteError{
					Index:   i,
					Code:    int(ce.Code()),
					Message: ce.Err().Error(),
				}

				writeErrors.Append(WriteErrorDocument(we))

				if params.Ordered {
					break
				}

				continue
			}

			return nil, lazyerrors.Error(err)
		}
	}

	res := must.NotFail(types.NewDocument(
		"n", deleted,
	))

	if writeErrors.Len() > 0 {
		res.Set("writeErrors", writeErrors)
	}

	res.Set("ok", float64(1))

	return documentOpMsg(
		res,
	)
}

// execDelete performs a single delete operation.
//
// It returns a number of deleted documents or error.
// The error is either a (wrapped) *handlererrors.CommandError or something fatal.
func (h *Handler) execDelete(ctx context.Context, c backends.Collection, p *common.Delete, skipDurableSync bool) (int32, error) {
	cmp := collation.Parse(p.Collation).Comparator()

	var qp backends.QueryParams
	qp.Collated = cmp != nil
	if !h.DisablePushdown {
		qp.Filter = p.Filter
	}

	q, err := c.Query(ctx, &qp)
	if err != nil {
		return 0, lazyerrors.Error(err)
	}

	var ids []any
	for {
		var doc *types.Document

		if _, doc, err = q.Iter.Next(); err != nil {
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}

			q.Iter.Close()
			return 0, lazyerrors.Error(err)
		}

		var matches bool

		if matches, err = common.FilterDocumentColl(doc, p.Filter, cmp); err != nil {
			q.Iter.Close()
			return 0, lazyerrors.Error(err)
		}

		if !matches {
			continue
		}

		ids = append(ids, must.NotFail(doc.Get("_id")))

		if p.Limited {
			break
		}
	}

	// close read transaction before starting write transaction
	q.Iter.Close()

	if len(ids) == 0 {
		return 0, nil
	}

	d, err := c.DeleteAll(ctx, &backends.DeleteAllParams{IDs: ids, SkipDurableSync: skipDurableSync})
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeReadOnlyDatabase) {
			return 0, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrOperationFailed,
				"cannot write to a read-only database snapshot",
			)
		}
		if backends.ErrorCodeIs(err, backends.ErrorCodeWriteConflict) {
			return 0, common.TranslateBackendWriteError(err)
		}
		return 0, lazyerrors.Error(err)
	}

	return d.Deleted, nil
}
