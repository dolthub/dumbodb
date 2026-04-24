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
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/FerretDB/wire"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// WriteErrorDocument returns a document representation of the write error.
//
// Find a better place for this function.
// TODO https://github.com/dolthub/dumbodb/issues/3263
func WriteErrorDocument(we *mongo.WriteError) *types.Document {
	return must.NotFail(types.NewDocument(
		"index", int32(we.Index),
		"code", int32(we.Code),
		"errmsg", we.Message,
	))
}

// MsgInsert implements `insert` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgInsert(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	params, err := common.GetInsertParams(document, h.L)
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
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "insert")
		}

		return nil, lazyerrors.Error(err)
	}

	c, err := db.Collection(params.Collection)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
			msg := fmt.Sprintf("Invalid collection name: %s", params.Collection)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "insert")
		}

		return nil, lazyerrors.Error(err)
	}

	// Fetch collection info for validator, view, time series, and capped checks.
	var collValidator *types.Document
	var validationAction string
	var tsTimeField string
	var cInfo backends.CollectionInfo
	if collRes, collErr := db.ListCollections(connCtx, &backends.ListCollectionsParams{Name: params.Collection}); collErr == nil {
		if len(collRes.Collections) == 1 {
			cInfo = collRes.Collections[0]
			if cInfo.IsView {
				msg := fmt.Sprintf("namespace '%s.%s' is a view, not a collection", params.DB, params.Collection)
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCommandNotSupportedOnView, msg, "insert")
			}
			if cInfo.IsTimeSeries {
				tsTimeField = cInfo.TimeField
			}
			if cInfo.Validator != nil && cInfo.ValidationLevel != "off" {
				collValidator = cInfo.Validator
				validationAction = cInfo.ValidationAction
				if validationAction == "" {
					validationAction = "error"
				}
			}
		}
	}

	docsIter := params.Docs.Iterator()
	defer docsIter.Close()

	var inserted int32
	var writeErrors []*mongo.WriteError

	var done bool
	for !done {
		docs := make([]*types.Document, 0, h.BatchSize)
		docsIndexes := make([]int, 0, h.BatchSize)

		for j := 0; j < h.BatchSize; j++ {
			var i int
			var d any

			i, d, err = docsIter.Next()
			if errors.Is(err, iterator.ErrIteratorDone) {
				done = true
				break
			}

			if err != nil {
				return nil, lazyerrors.Error(err)
			}

			doc := d.(*types.Document)

			if !doc.Has("_id") {
				doc.Set("_id", types.NewObjectID())
			}

			// TODO https://github.com/dolthub/dumbodb/issues/3454
			if err = doc.ValidateData(); err != nil {
				var ve *types.ValidationError
				if !errors.As(err, &ve) {
					return nil, lazyerrors.Error(err)
				}

				var code handlererrors.ErrorCode
				switch ve.Code() {
				case types.ErrValidation, types.ErrIDNotFound:
					code = handlererrors.ErrBadValue
				case types.ErrWrongIDType:
					code = handlererrors.ErrInvalidID
				default:
					panic(fmt.Sprintf("Unknown error code: %v", ve.Code()))
				}

				writeErrors = append(writeErrors, &mongo.WriteError{
					Index:   i,
					Code:    int(code),
					Message: ve.Error(),
				})

				if params.Ordered {
					break
				}

				continue
			}

			// Apply time series validation if this is a time series collection.
			if tsTimeField != "" {
				tsFieldVal, tsErr := doc.Get(tsTimeField)
				if tsErr != nil {
					// Missing time field.
					writeErrors = append(writeErrors, &mongo.WriteError{
						Index:   i,
						Code:    int(handlererrors.ErrBadValue),
						Message: fmt.Sprintf("time series document is missing the '%s' field", tsTimeField),
					})
					if params.Ordered {
						break
					}
					continue
				}
				if _, ok := tsFieldVal.(time.Time); !ok {
					// Time field is not a Date type.
					writeErrors = append(writeErrors, &mongo.WriteError{
						Index:   i,
						Code:    int(handlererrors.ErrBadValue),
						Message: fmt.Sprintf("time series document '%s' field must be of type Date", tsTimeField),
					})
					if params.Ordered {
						break
					}
					continue
				}
			}

			// Apply schema validator if set.
			if collValidator != nil {
				matches, schemaErr := common.FilterDocument(doc, collValidator)
				if schemaErr != nil {
					return nil, lazyerrors.Error(schemaErr)
				}

				if !matches {
					const errMsg = "Document failed validation"
					if validationAction == "warn" {
						h.L.Warn("document failed schema validation", "collection", params.Collection)
					} else {
						writeErrors = append(writeErrors, &mongo.WriteError{
							Index:   i,
							Code:    int(handlererrors.ErrDocumentValidationFailure),
							Message: errMsg,
						})

						if params.Ordered {
							break
						}

						continue
					}
				}
			}

			docs = append(docs, doc)
			docsIndexes = append(docsIndexes, i)
		}

		if _, err = c.InsertAll(connCtx, &backends.InsertAllParams{Docs: docs, SkipDurableSync: params.SkipDurableSync}); err == nil {
			inserted += int32(len(docs))

			if params.Ordered && len(writeErrors) > 0 {
				break
			}

			continue
		}

		// insert doc one by one upon failing on batch insertion
		for j, doc := range docs {
			if _, err = c.InsertAll(connCtx, &backends.InsertAllParams{
				Docs:            []*types.Document{doc},
				SkipDurableSync: params.SkipDurableSync,
			}); err == nil {
				inserted++

				continue
			}

			if !backends.ErrorCodeIs(err, backends.ErrorCodeInsertDuplicateID) {
				return nil, lazyerrors.Error(err)
			}

			dupID, _ := doc.Get("_id")
			writeErrors = append(writeErrors, &mongo.WriteError{
				Index:   docsIndexes[j],
				Code:    int(handlererrors.ErrDuplicateKeyInsert),
				Message: fmt.Sprintf(
					`E11000 duplicate key error collection: %s.%s index: _id_ dup key: { _id: %s }`,
					params.DB, params.Collection, types.FormatAnyValue(dupID),
				),
			})

			if params.Ordered {
				break
			}
		}
	}

	// Enforce capped collection size limit by evicting oldest documents after insert.
	if inserted > 0 && cInfo.Capped() {
		if _, _, cleanupErr := h.cleanupCappedCollection(connCtx, db, &cInfo, false); cleanupErr != nil {
			h.L.Warn("capped collection cleanup after insert failed", "error", cleanupErr)
		}
	}

	res := must.NotFail(types.NewDocument(
		"n", inserted,
	))

	if len(writeErrors) > 0 {
		slices.SortFunc(writeErrors, func(a, b *mongo.WriteError) int {
			return cmp.Compare(a.Index, b.Index)
		})

		array := types.MakeArray(len(writeErrors))
		for _, we := range writeErrors {
			array.Append(WriteErrorDocument(we))
		}

		res.Set("writeErrors", array)
	}

	res.Set("ok", float64(1))

	return documentOpMsg(
		res,
	)
}
