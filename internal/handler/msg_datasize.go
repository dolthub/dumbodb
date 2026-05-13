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
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	internalbson "github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgDataSize implements `dataSize` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDataSize(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	common.Ignored(document, h.L, "estimate")

	var namespaceParam any

	if namespaceParam, err = document.Get(document.Command()); err != nil {
		return nil, err
	}

	namespace, ok := namespaceParam.(string)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			fmt.Sprintf(
				"BSON field 'dataSize.dataSize' is the wrong type '%s', expected type 'string'",
				handlerparams.AliasFromType(namespaceParam),
			),
			document.Command(),
		)
	}

	dbName, cName, err := handlerparams.SplitNamespace(namespace, document.Command())
	if err != nil {
		return nil, err
	}

	db, err := h.b.Database(dbName)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid database specified '%s'", dbName)
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

	started := time.Now()

	// Build key range filter if keyPattern, min, and max are provided.
	var rangeFilter *types.Document
	if kp, kpErr := document.Get("keyPattern"); kpErr == nil {
		if kpDoc, ok := kp.(*types.Document); ok && kpDoc.Len() > 0 {
			fieldName := kpDoc.Keys()[0]

			minDoc, minErr := document.Get("min")
			maxDoc, maxErr := document.Get("max")

			if minErr == nil && maxErr == nil {
				if minD, ok := minDoc.(*types.Document); ok {
					if maxD, ok := maxDoc.(*types.Document); ok {
						minVal, _ := minD.Get(fieldName)
						maxVal, _ := maxD.Get(fieldName)
						if minVal != nil && maxVal != nil {
							rangeFilter = must.NotFail(types.NewDocument(
								fieldName, must.NotFail(types.NewDocument(
									"$gte", minVal,
									"$lt", maxVal,
								)),
							))
						}
					}
				}
			}
		}
	}

	bsonSize, numDocs, err := collectionBSONDataSize(connCtx, c, rangeFilter)
	if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist) ||
		backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseDoesNotExist) {
		bsonSize = 0
		numDocs = 0
		err = nil
	}

	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	pairs := []any{
		"size", bsonSize,
		"numObjects", numDocs,
		"millis", int64(time.Since(started).Milliseconds()),
	}

	if numDocs > 0 || bsonSize > 0 {
		pairs = append(pairs, "estimate", false)
	}

	pairs = append(pairs, "ok", float64(1))

	return documentOpMsg(
		must.NotFail(types.NewDocument(pairs...)),
	)
}

// collectionBSONDataSize iterates documents in a collection and returns the sum
// of their encoded BSON sizes and the document count. filter, if non-nil, is
// passed to Query to limit the scan (e.g. for a key range). This matches
// MongoDB's dataSize semantics (raw BSON bytes, not storage overhead).
func collectionBSONDataSize(ctx context.Context, c backends.Collection, filter *types.Document) (int64, int64, error) {
	var qp *backends.QueryParams
	if filter != nil {
		qp = &backends.QueryParams{Filter: filter}
	}

	qRes, err := c.Query(ctx, qp)
	if err != nil {
		return 0, 0, err
	}

	iter := qRes.Iter
	defer iter.Close()

	var totalSize, numDocs int64

	for {
		_, doc, iterErr := iter.Next()
		if iterErr != nil {
			break
		}

		// Post-filter: the backend may not fully apply the filter, so we check here.
		if filter != nil {
			matches, filterErr := common.FilterDocument(doc, filter)
			if filterErr != nil {
				return 0, 0, lazyerrors.Error(filterErr)
			}
			if !matches {
				continue
			}
		}

		wdoc, convErr := internalbson.FromDocument(doc)
		if convErr != nil {
			return 0, 0, lazyerrors.Error(convErr)
		}

		raw, encErr := wdoc.Encode()
		if encErr != nil {
			return 0, 0, lazyerrors.Error(encErr)
		}

		totalSize += int64(len(raw))
		numDocs++
	}

	return totalSize, numDocs, nil
}
