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
	"math"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/clientconn/cursor"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/ctxutil"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgGetMore implements `getMore` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgGetMore(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = common.RejectUnknownFields(document, "batchSize", "collection"); err != nil {
		return nil, err
	}

	db, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	// Validate rootish before backend access so invalid forms (HEAD, reflog, range)
	// return OperationFailed (96) rather than silently succeeding or returning
	// InvalidNamespace (73) from MongoDB's own namespace check.
	if _, _, _, err := branchFromDBName(db); err != nil {
		return nil, err
	}

	v, _ := document.Get("collection")
	if v == nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrMissingField,
			"BSON field 'getMore.collection' is missing but a required field",
			document.Command(),
		)
	}

	collection, ok := v.(string)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			fmt.Sprintf(
				"BSON field 'getMore.collection' is the wrong type '%s', expected type 'string'",
				handlerparams.AliasFromType(v),
			),
			document.Command(),
		)
	}

	if collection == "" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrInvalidNamespace,
			"Collection names cannot be empty",
			document.Command(),
		)
	}

	cursorID, err := common.GetRequiredParam[int64](document, document.Command())
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"BSON field 'getMore.getMore' is the wrong type, expected type 'long'",
			document.Command(),
		)
	}

	v, _ = document.Get("maxTimeMS")

	maxTimeMSPresent := v != nil

	// GetOptionalParam cannot be used to set default value, as we need to return error if
	// maxTimeMS was provided for non-awaitData cursors.
	if v == nil {
		v = int64(1000)
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
					`BSON field 'getMore.maxTimeMS' is the wrong type '%s', expected types '[long, int, decimal, double]'`,
					handlerparams.AliasFromType(v),
				),
				document.Command(),
			)
		case errors.Is(err, handlerparams.ErrNotWholeNumber):
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"maxTimeMS has non-integral value",
				document.Command(),
			)
		case errors.Is(err, handlerparams.ErrLongExceededPositive) || errors.Is(err, handlerparams.ErrLongExceededNegative):
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("%s value for maxTimeMS is out of range", types.FormatAnyValue(v)),
				document.Command(),
			)
		default:
			return nil, lazyerrors.Error(err)
		}
	}

	if maxTimeMS < int64(0) || maxTimeMS > math.MaxInt32 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("%v value for maxTimeMS is out of range", v),
			document.Command(),
		)
	}

	owner := conninfo.Get(connCtx).SessionPrincipal()

	c := h.cursors.Get(cursorID)
	if c == nil || c.Owner != owner {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrCursorNotFound,
			fmt.Sprintf("cursor id %d not found", cursorID),
			document.Command(),
		)
	}

	if maxTimeMSPresent && c.Type != cursor.TailableAwait {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"cannot set maxTimeMS on getMore command for a non-awaitData cursor",
			document.Command(),
		)
	}

	// A missing or zero batchSize puts no count limit on the batch; the size
	// limit still applies.
	batchSize := int64(-1)
	if v, _ = document.Get("batchSize"); v != nil && types.Compare(v, int32(0)) != types.Equal {
		if batchSize, err = handlerparams.GetValidatedNumberParamWithMinValue(document.Command(), "batchSize", v, 0); err != nil {
			return nil, err
		}
	}

	if c.DB != db || c.Collection != collection {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrUnauthorized,
			fmt.Sprintf(
				"Requested getMore on namespace '%s.%s', but cursor belongs to a different namespace %s.%s",
				db,
				collection,
				c.DB,
				c.Collection,
			),
			document.Command(),
		)
	}
	if h.ReplicationTopology != nil && c.DB == "local" && c.Collection == "oplog.rs" {
		return nil, downstreamReplicationUnsupportedError()
	}

	nextBatch, done, err := h.makeNextBatch(c, batchSize)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	switch c.Type {
	case cursor.Normal:
		if done {
			// The cursor is already closed and removed;
			// let the client know that there are no more results.
			cursorID = 0
		}

	case cursor.Tailable:
		if nextBatch.Len() == 0 {
			// The previous iterator is already closed there.

			data := c.Data.(*findCursorData)

			var queryRes *backends.QueryResult

			queryRes, err = data.coll.Query(connCtx, data.qp)
			if err != nil {
				return nil, lazyerrors.Error(err)
			}

			closer := iterator.NewMultiCloser()
			defer closer.Close()

			iter, err := h.makeFindIter(queryRes.Iter, closer, data.findParams, queryRes.Sorted)
			if err != nil {
				return nil, lazyerrors.Error(err)
			}

			if err = c.Reset(iter); err != nil {
				return nil, lazyerrors.Error(err)
			}

			if nextBatch.Len() == 0 {
				nextBatch, _, err = h.makeNextBatch(c, batchSize)
				if err != nil {
					return nil, lazyerrors.Error(err)
				}
			}
		}

	case cursor.TailableAwait:
		if nextBatch.Len() == 0 {
			nextBatch, err = h.awaitData(connCtx, &awaitDataParams{
				cursor:    c,
				batchSize: batchSize,
				maxTimeMS: maxTimeMS,
			})
			if err != nil {
				return nil, lazyerrors.Error(err)
			}
		}

	default:
		panic(fmt.Sprintf("unknown cursor type %s", c.Type))
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"cursor", must.NotFail(types.NewDocument(
				"nextBatch", nextBatch,
				"id", cursorID,
				"ns", db+"."+collection,
			)),
			"ok", float64(1),
		)),
	)
}

// makeNextBatch returns the next batch of documents from the cursor and
// whether the cursor is exhausted. A negative batchSize sets no count limit.
func (h *Handler) makeNextBatch(c *cursor.Cursor, batchSize int64) (*types.Array, bool, error) {
	docs, done, err := c.NextBatch(int(batchSize), types.MaxDocumentLen)
	if err != nil {
		return nil, false, lazyerrors.Error(err)
	}

	h.L.Debug(
		"Got next batch",
		slog.Int64("cursor_id", c.ID),
		slog.String("type", c.Type.String()),
		slog.Int("count", len(docs)),
		slog.Int64("batch_size", batchSize),
	)

	nextBatch := types.MakeArray(len(docs))
	for _, doc := range docs {
		nextBatch.Append(doc)
	}

	return nextBatch, done, nil
}

// awaitDataParams contains parameters that can be passed to awaitData function.
type awaitDataParams struct {
	cursor    *cursor.Cursor
	maxTimeMS int64
	batchSize int64
}

// awaitData stops the goroutine, and waits for a new data for the cursor.
// If there's a new document, or the maxTimeMS have passed it returns the nextBatch.
func (h *Handler) awaitData(ctx context.Context, params *awaitDataParams) (resBatch *types.Array, err error) {
	resBatch = types.MakeArray(0)

	closer := iterator.NewMultiCloser()
	defer closer.Close()

	c := params.cursor
	data := c.Data.(*findCursorData)

	sleepDur := time.Duration(params.maxTimeMS) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, sleepDur)

	defer func() {
		cancel()

		if err == nil {
			return
		}

		// Return empty batch and no error if context timeout exceeded
		if errors.Is(err, context.DeadlineExceeded) {
			resBatch = types.MakeArray(0)
			err = nil

			return
		}

		err = lazyerrors.Error(err)
	}()

	for {
		var queryRes *backends.QueryResult

		queryRes, err = data.coll.Query(ctx, data.qp)
		if err != nil {
			return
		}

		var iter types.DocumentsIterator

		iter, err = h.makeFindIter(queryRes.Iter, closer, data.findParams, queryRes.Sorted)
		if err != nil {
			return
		}

		if err = c.Reset(iter); err != nil {
			return
		}

		if resBatch.Len() != 0 {
			return
		}

		resBatch, _, err = h.makeNextBatch(c, params.batchSize)
		if err != nil {
			return
		}

		if params.maxTimeMS > 10 {
			ctxutil.Sleep(ctx, 10*time.Millisecond)
		}
	}
}
