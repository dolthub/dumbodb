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
	"slices"
	"time"

	"github.com/FerretDB/wire"
	"go.mongodb.org/mongo-driver/v2/mongo"

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

// MsgUpdate implements `update` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgUpdate(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err := rejectRetryableWriteOnStandalone(document, h.ReplSetName); err != nil {
		return nil, err
	}

	params, err := common.GetUpdateParams(document, h.L)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = enforceWritableRootish(params.DB); err != nil {
		return nil, err
	}

	ctx := connCtx
	if params.MaxTimeMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(connCtx, time.Duration(params.MaxTimeMS)*time.Millisecond)
		defer cancel()
	}

	matched, modified, upserted, writeErrors, err := h.updateDocument(ctx, params)
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

	// mongod's reply order.
	if writeErrors.Len() > 0 {
		res.Set("writeErrors", writeErrors)
	}
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
// updateDocument runs params' statements and returns the documents matched and
// modified, the upserted ids, and the statements' write errors. An ordered
// update stops at its first write error, as mongod's does.
func (h *Handler) updateDocument(ctx context.Context, params *common.UpdateParams) (int32, int32, *types.Array, *types.Array, error) {
	var matched, modified int32
	var upserted types.Array

	db, err := h.b.Database(params.DB)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid namespace specified '%s.%s'", params.DB, params.Collection)
			return 0, 0, nil, nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "update")
		}

		return 0, 0, nil, nil, lazyerrors.Error(err)
	}

	// Check if collection is a view  -- views don't support write operations.
	if collRes, collErr := db.ListCollections(ctx, &backends.ListCollectionsParams{Name: params.Collection}); collErr == nil {
		if len(collRes.Collections) == 1 && collRes.Collections[0].IsView {
			msg := fmt.Sprintf("namespace '%s.%s' is a view, not a collection", params.DB, params.Collection)
			return 0, 0, nil, nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCommandNotSupportedOnView, msg, "update")
		}
	}

	// Only an upsert creates the collection, as on mongod; an update of a
	// missing collection otherwise matches nothing.
	err = nil
	if slices.ContainsFunc(params.Updates, func(u common.Update) bool { return u.Upsert }) {
		err = db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: params.Collection})
	}

	switch {
	case err == nil:
		// nothing
	case backends.ErrorCodeIs(err, backends.ErrorCodeCollectionAlreadyExists):
		// nothing
	case backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid):
		msg := fmt.Sprintf("Invalid collection name: %s", params.Collection)
		return 0, 0, nil, nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "insert")
	default:
		return 0, 0, nil, nil, common.TranslateBackendWriteError(ctx, err)
	}

	var validator *types.Document
	var valLevel, valAction string
	if !params.BypassDocumentValidation {
		validator, valLevel, valAction = collectionValidator(ctx, db, params.Collection)
	}

	clusteredName := recordedClusteredName(ctx, db, params.Collection)
	for i := range params.Updates {
		params.Updates[i].Hint = clusteredHint(params.Updates[i].Hint, clusteredName)
	}
	// mongod refuses a hint of the wrong type for the whole command.
	for _, u := range params.Updates {
		if err := checkHintType(u.Hint); err != nil {
			return 0, 0, nil, nil, err
		}
	}

	writeErrors := types.MakeArray(0)
	// statementFailed records statement i's write error and reports whether
	// the update stops there.
	statementFailed := func(i int, ce *handlererrors.CommandError) bool {
		writeErrors.Append(WriteErrorDocument(&mongo.WriteError{Index: i, Code: int(ce.Code()), Message: ce.Err().Error()}))
		return params.Ordered
	}

statements:
	for i, u := range params.Updates {
		u.Collation = h.effectiveCollation(ctx, db, params.Collection, u.Collation)
		c, err := db.Collection(params.Collection)
		if err != nil {
			if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
				msg := fmt.Sprintf("Invalid collection name: %s", params.Collection)
				return 0, 0, nil, nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "insert")
			}

			return 0, 0, nil, nil, lazyerrors.Error(err)
		}

		u.Validator = validator
		u.ValidationLevel = valLevel
		u.ValidationAction = valAction

		cmp := collation.Parse(u.Collation).Comparator()
		operators.BindCollation(u.Filter, cmp)
		operators.BindCollation(u.Pipeline, cmp)

		if u.InvalidUpdate != nil {
			ce := statementWriteError(params.DB, params.Collection, u.InvalidUpdate)
			if ce == nil {
				return 0, 0, nil, nil, u.InvalidUpdate
			}
			if statementFailed(i, ce) {
				break statements
			}
			continue
		}

		common.CanonicalizeFilter(u.Filter)
		err = common.ValidateFilter(u.Filter, cmp)
		pipelineMalformed := false
		if err == nil && u.IsPipeline {
			u.PipelineStages, err = common.ParseUpdatePipeline(u.Pipeline)
			var ce *handlererrors.CommandError
			var malformed *common.MalformedPipelineError
			pipelineMalformed = errors.As(err, &malformed) || errors.As(err, &ce) && (ce.Code() == handlererrors.ErrStageInvalid ||
				ce.Code() == handlererrors.ErrStageUnrecognized || hasNonDocumentElement(u.Pipeline))
		}
		if err != nil {
			// MongoDB reports an update's invalid filter or pipeline as a write
			// error, except a pipeline element that is not one stage document
			// or names an unknown stage, which fails the batch.
			var ce *handlererrors.CommandError
			if errors.As(err, &ce) && !pipelineMalformed {
				if statementFailed(i, ce) {
					break statements
				}
				continue
			}
			return 0, 0, nil, nil, err
		}

		if err = validateHint(ctx, db, params.Collection, c, u.Hint, "update", nil, u.Upsert); err != nil {
			// mongod reports an unusable hint as the statement's write error.
			var ce *handlererrors.CommandError
			if errors.As(err, &ce) {
				if statementFailed(i, ce) {
					break statements
				}
				continue
			}
			return 0, 0, nil, nil, err
		}

		var qp backends.QueryParams
		qp.Collated = cmp != nil
		qp.Hint = u.Hint
		if !h.DisablePushdown {
			qp.Filter = u.Filter
		}

		res, err := c.Query(ctx, &qp)
		if err != nil {
			return 0, 0, nil, nil, lazyerrors.Error(err)
		}

		closer := iterator.NewMultiCloser()
		defer closer.Close()

		closer.Add(res.Iter)

		iter := common.FilterIteratorPlan(iterator.WithContext(ctx, res.Iter), closer, u.Filter, cmp, queryPlanMode(ctx, c, u.Filter, u.Hint, res))

		if !u.Multi {
			// The one document updated is the first match in the hinted
			// index's order.
			if iter, err = hintedFirstMatches(ctx, c, u.Hint, u.Filter, u.Collation, iter, closer, cmp); err != nil {
				return 0, 0, nil, nil, handleMaxTimeMSError(err, params.MaxTimeMS, "update")
			}
			iter = common.LimitIterator(iter, closer, 1)
		}

		result, err := common.UpdateDocument(ctx, c, "update", iter, &u)
		if err != nil {
			if backends.ErrorCodeIs(err, backends.ErrorCodeReadOnlyDatabase, backends.ErrorCodeReadOnlyCollection,
				backends.ErrorCodeBranchInConflict, backends.ErrorCodeWriteConflict) {
				return 0, 0, nil, nil, common.TranslateBackendWriteError(ctx, err)
			}
			// mongod reports a statement that fails as it writes as that
			// statement's write error and keeps the statements before it.
			if ce := statementWriteError(params.DB, params.Collection, err); ce != nil {
				if statementFailed(i, ce) {
					break statements
				}
				continue
			}
			return 0, 0, nil, nil, lazyerrors.Error(err)
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
				"index", int32(i),
				"_id", must.NotFail(doc.Get("_id")),
			)))

			// in case of upsert, MongoDB sets the matched count to 1
			matched++
		}
	}

	return matched, modified, &upserted, writeErrors, nil
}

// hasNonDocumentElement reports whether an update pipeline has an element
// that is not a stage document.
func hasNonDocumentElement(pipeline *types.Array) bool {
	for i := range pipeline.Len() {
		if _, ok := must.NotFail(pipeline.Get(i)).(*types.Document); !ok {
			return true
		}
	}
	return false
}

// statementWriteError returns the write error an update statement's failed
// write reports, or nil when err is not one.
func statementWriteError(db, coll string, err error) *handlererrors.CommandError {
	var be *backends.Error
	if errors.As(err, &be) && be.Code() == backends.ErrorCodeInsertDuplicateID {
		idxName, keyDoc := backends.DefaultIndexName, types.MakeDocument(0)
		if be.DupIndex() != "" {
			idxName = be.DupIndex()
		}
		if k := be.DupKey(); k != nil {
			keyDoc = k
		}
		msg := fmt.Sprintf(`E11000 duplicate key error collection: %s.%s index: %s dup key: %s`, db, coll, idxName, formatDupKey(keyDoc))
		ce, _ := handlererrors.NewCommandErrorMsg(handlererrors.ErrDuplicateKeyInsert, msg).(*handlererrors.CommandError)
		return ce
	}
	var ve *types.ValidationError
	if errors.As(err, &ve) {
		err = validationErrToUpdateErr("update", ve)
	}
	var we *handlererrors.WriteErrors
	if errors.As(err, &we) {
		if code, msg, ok := we.First(); ok {
			ce, _ := handlererrors.NewCommandErrorMsg(code, msg).(*handlererrors.CommandError)
			return ce
		}
		return nil
	}
	var ce *handlererrors.CommandError
	if errors.As(err, &ce) {
		return ce
	}
	return nil
}
