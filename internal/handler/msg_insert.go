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
	"strings"
	"time"

	"github.com/FerretDB/wire"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// appendDocHashes records one {index, _id, hash} entry per stored document.
// hashes runs parallel to docs, and idxs gives each document's position in the
// client's documents array (the index writeErrors reports too).
func appendDocHashes(out *types.Array, hashes []string, docs []*types.Document, idxs []int) {
	for i, h := range hashes {
		if i >= len(docs) || i >= len(idxs) {
			break
		}

		id, err := docs[i].Get("_id")
		if err != nil {
			continue
		}

		out.Append(must.NotFail(types.NewDocument(
			"index", int32(idxs[i]),
			"_id", id,
			"hash", h,
		)))
	}
}

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
			if cInfo.Validator != nil && cInfo.ValidationLevel != "off" && !params.BypassDocumentValidation {
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
	docHashes := types.MakeArray(0)

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
				case types.ErrDollarPrefixedID:
					code = handlererrors.ErrDollarPrefixedFieldName
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

		var insertRes *backends.InsertAllResult

		if insertRes, err = c.InsertAll(connCtx, &backends.InsertAllParams{
			Docs:            docs,
			SkipDurableSync: params.SkipDurableSync,
			ReturnDocHashes: params.DumboDocHashes,
		}); err == nil {
			inserted += int32(len(docs))
			appendDocHashes(docHashes, insertRes.DocHashes, docs, docsIndexes)

			if params.Ordered && len(writeErrors) > 0 {
				break
			}

			continue
		}

		// insert doc one by one upon failing on batch insertion
		for j, doc := range docs {
			if insertRes, err = c.InsertAll(connCtx, &backends.InsertAllParams{
				Docs:            []*types.Document{doc},
				SkipDurableSync: params.SkipDurableSync,
				ReturnDocHashes: params.DumboDocHashes,
			}); err == nil {
				inserted++
				appendDocHashes(docHashes, insertRes.DocHashes, []*types.Document{doc}, docsIndexes[j:j+1])

				continue
			}

			if backends.ErrorCodeIs(err, backends.ErrorCodeReadOnlyDatabase) {
				return nil, handlererrors.NewCommandErrorMsg(
					handlererrors.ErrOperationFailed,
					"cannot write to a read-only database snapshot",
				)
			}

			if backends.ErrorCodeIs(err, backends.ErrorCodeWriteConflict) {
				return nil, common.TranslateBackendWriteError(err)
			}

			if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
				errMsg := fmt.Sprintf("Invalid namespace specified '%s.%s'", params.DB, params.Collection)
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, errMsg, "insert")
			}

			if !backends.ErrorCodeIs(err, backends.ErrorCodeInsertDuplicateID) {
				return nil, lazyerrors.Error(err)
			}

			// Name the actual index and key that collided (the backend attaches
			// them for a duplicate-key error); fall back to the _id index.
			idxName := backends.DefaultIndexName
			dupID, _ := doc.Get("_id")
			if dupID == nil {
				dupID = types.Null
			}
			keyDoc := must.NotFail(types.NewDocument("_id", dupID))
			var be *backends.Error
			if errors.As(err, &be) {
				if be.DupIndex() != "" {
					idxName = be.DupIndex()
				}
				if k := be.DupKey(); k != nil {
					keyDoc = k
				}
			}
			writeErrors = append(writeErrors, &mongo.WriteError{
				Index: docsIndexes[j],
				Code:  int(handlererrors.ErrDuplicateKeyInsert),
				Message: fmt.Sprintf(
					`E11000 duplicate key error collection: %s.%s index: %s dup key: %s`,
					params.DB, params.Collection, idxName, formatDupKey(keyDoc),
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

	if params.DumboDocHashes {
		res.Set("dumboDocHashes", docHashes)
	}

	res.Set("ok", float64(1))

	return documentOpMsg(
		res,
	)
}

// formatDupKey renders a duplicate key the way MongoDB does: { field: value, ... }.
func formatDupKey(d *types.Document) string {
	var b strings.Builder
	b.WriteString("{ ")
	for i, k := range d.Keys() {
		if i > 0 {
			b.WriteString(", ")
		}
		v, _ := d.Get(k)
		fmt.Fprintf(&b, "%s: %s", k, types.FormatAnyValue(v))
	}
	b.WriteString(" }")
	return b.String()
}
