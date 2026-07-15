// Copyright 2026 Dolthub, Inc.
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
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgBulkWrite implements the MongoDB 8.0 server-side `bulkWrite` admin command.
//
// Unlike the driver-side collection bulkWrite (which drivers decompose into
// individual insert/update/delete commands), this command batches operations
// across multiple collections  -- and multiple databases  -- in a single round
// trip. Ops reference their target namespace by index into a companion nsInfo
// array, both of which arrive as OP_MSG document sequences (payload type 1).
func (h *Handler) MsgBulkWrite(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// bulkWrite may only run against the admin database.
	dbName, _ := document.Get("$db")
	if s, _ := dbName.(string); s != "admin" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrOperationFailed,
			"bulkWrite may only be run against the admin database",
			"bulkWrite",
		)
	}

	ordered := true
	if v, err := document.Get("ordered"); err == nil {
		if b, ok := v.(bool); ok {
			ordered = b
		}
	}

	errorsOnly := false
	if v, err := document.Get("errorsOnly"); err == nil {
		if b, ok := v.(bool); ok {
			errorsOnly = b
		}
	}

	var wc *types.Document
	if v, err := document.Get("writeConcern"); err == nil {
		if d, ok := v.(*types.Document); ok {
			wc = d
		}
	}
	skipDurableSync := common.DecideWriteConcern(wc).SkipDurableSync

	nsInfo, err := getArrayField(document, "nsInfo")
	if err != nil {
		return nil, err
	}
	ops, err := getArrayField(document, "ops")
	if err != nil {
		return nil, err
	}

	// Pre-resolve the ns strings once so every op references a fixed list
	// and we fail fast if nsInfo is malformed.
	namespaces := make([]bulkWriteNS, nsInfo.Len())
	for i := 0; i < nsInfo.Len(); i++ {
		nsDoc, ok := must.NotFail(nsInfo.Get(i)).(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf("nsInfo[%d] is not a document", i),
				"bulkWrite",
			)
		}
		nsAny, err := nsDoc.Get("ns")
		if err != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("nsInfo[%d] missing 'ns' field", i),
				"bulkWrite",
			)
		}
		nsStr, ok := nsAny.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf("nsInfo[%d].ns must be a string", i),
				"bulkWrite",
			)
		}
		dot := strings.IndexByte(nsStr, '.')
		if dot <= 0 || dot == len(nsStr)-1 {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				fmt.Sprintf("Invalid namespace specified %q", nsStr),
				"bulkWrite",
			)
		}
		namespaces[i] = bulkWriteNS{db: nsStr[:dot], coll: nsStr[dot+1:]}
	}

	// Counters and per-op responses accumulate as we iterate.
	var (
		nInserted, nMatched, nModified, nUpserted, nDeleted, nErrors int32
	)
	firstBatch := types.MakeArray(ops.Len())

	for i := 0; i < ops.Len(); i++ {
		opDoc, ok := must.NotFail(ops.Get(i)).(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf("ops[%d] is not a document", i),
				"bulkWrite",
			)
		}

		res, opErr := h.execBulkWriteOp(connCtx, opDoc, namespaces, skipDurableSync)
		entry := must.NotFail(types.NewDocument("idx", int32(i)))

		if opErr != nil {
			nErrors++
			code, msg := extractCommandError(opErr)
			entry.Set("ok", float64(0))
			entry.Set("code", code)
			entry.Set("errmsg", msg)
			// Errors are always surfaced in firstBatch, even when errorsOnly
			// is set  -- that's the whole point of errorsOnly.
			firstBatch.Append(entry)
			if ordered {
				break
			}
			continue
		}

		nInserted += res.inserted
		nMatched += res.matched
		nModified += res.modified
		nUpserted += res.upserted
		nDeleted += res.deleted

		if errorsOnly {
			// Skip successful entries when the client only wants errors.
			continue
		}

		entry.Set("ok", float64(1))
		entry.Set("n", res.matched+res.inserted+res.deleted)
		if res.modified > 0 || res.kind == "update" {
			entry.Set("nModified", res.modified)
		}
		if res.upserted > 0 && res.upsertedID != nil {
			entry.Set("upserted", must.NotFail(types.NewDocument(
				"index", int32(i),
				"_id", res.upsertedID,
			)))
		}
		firstBatch.Append(entry)
	}

	conninfo.Get(connCtx).SetAutoCommitMessage(fmt.Sprintf(
		"auto: bulkWrite (%d inserted, %d updated, %d deleted)", nInserted, nModified+nUpserted, nDeleted))

	resDoc := must.NotFail(types.NewDocument(
		"cursor", must.NotFail(types.NewDocument(
			"id", int64(0),
			"firstBatch", firstBatch,
			"ns", "admin.$cmd.bulkWrite",
		)),
		"nErrors", nErrors,
		"nInserted", nInserted,
		"nMatched", nMatched,
		"nModified", nModified,
		"nUpserted", nUpserted,
		"nDeleted", nDeleted,
		"ok", float64(1),
	))

	return documentOpMsg(resDoc)
}

// bulkWriteNS is a parsed entry from the bulkWrite nsInfo array.
type bulkWriteNS struct {
	db   string
	coll string
}

// bulkWriteOpResult is the per-op tally returned by execBulkWriteOp.
type bulkWriteOpResult struct {
	kind       string // "insert" | "update" | "delete"
	inserted   int32
	matched    int32
	modified   int32
	upserted   int32
	deleted    int32
	upsertedID any
}

// execBulkWriteOp dispatches a single op document to the matching backend
// call. It returns either a populated result or a command error that the
// caller wraps into the per-op firstBatch entry.
func (h *Handler) execBulkWriteOp(ctx context.Context, op *types.Document, namespaces []bulkWriteNS, skipDurableSync bool) (bulkWriteOpResult, error) {
	// Identify the op kind by looking for the first of insert / update /
	// delete. The value is the index into the namespaces array.
	var (
		kind    string
		nsIndex int64
	)
	for _, candidate := range []string{"insert", "update", "delete"} {
		v, err := op.Get(candidate)
		if err != nil {
			continue
		}
		n, err := getInt64Value(v)
		if err != nil {
			return bulkWriteOpResult{}, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf("op '%s' index must be numeric", candidate),
				"bulkWrite",
			)
		}
		kind = candidate
		nsIndex = n
		break
	}

	if kind == "" {
		return bulkWriteOpResult{}, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"each op must specify one of insert, update, delete",
			"bulkWrite",
		)
	}

	if nsIndex < 0 || nsIndex >= int64(len(namespaces)) {
		return bulkWriteOpResult{}, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("op nsIndex %d out of range (nsInfo has %d entries)", nsIndex, len(namespaces)),
			"bulkWrite",
		)
	}

	ns := namespaces[nsIndex]

	if err := enforceWritableRootish(ns.db); err != nil {
		return bulkWriteOpResult{}, err
	}

	db, err := h.b.Database(ns.db)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			return bulkWriteOpResult{}, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				fmt.Sprintf("Invalid namespace specified '%s.%s'", ns.db, ns.coll),
				"bulkWrite",
			)
		}
		return bulkWriteOpResult{}, lazyerrors.Error(err)
	}

	c, err := db.Collection(ns.coll)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
			return bulkWriteOpResult{}, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				fmt.Sprintf("Invalid collection name: %s", ns.coll),
				"bulkWrite",
			)
		}
		return bulkWriteOpResult{}, lazyerrors.Error(err)
	}

	switch kind {
	case "insert":
		return execBulkWriteInsert(ctx, c, op, skipDurableSync)
	case "update":
		return execBulkWriteUpdate(ctx, db, c, ns, op, skipDurableSync, h.DisablePushdown)
	case "delete":
		return execBulkWriteDelete(ctx, c, op, skipDurableSync, h.DisablePushdown)
	}

	// Unreachable  -- kind is known to be one of the three.
	return bulkWriteOpResult{}, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrFailedToParse,
		fmt.Sprintf("unknown op kind %q", kind),
		"bulkWrite",
	)
}

func execBulkWriteInsert(ctx context.Context, c backends.Collection, op *types.Document, skipDurableSync bool) (bulkWriteOpResult, error) {
	docVal, err := op.Get("document")
	if err != nil {
		return bulkWriteOpResult{}, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"insert op missing 'document'",
			"bulkWrite",
		)
	}
	doc, ok := docVal.(*types.Document)
	if !ok {
		return bulkWriteOpResult{}, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			"insert op 'document' must be a document",
			"bulkWrite",
		)
	}

	if !doc.Has("_id") {
		doc.Set("_id", types.NewObjectID())
	}

	if err := doc.ValidateData(); err != nil {
		var ve *types.ValidationError
		if errors.As(err, &ve) {
			return bulkWriteOpResult{kind: "insert"}, validationErrToUpdateErr("bulkWrite", ve)
		}
		return bulkWriteOpResult{}, lazyerrors.Error(err)
	}

	if _, err := c.InsertAll(ctx, &backends.InsertAllParams{
		Docs:            []*types.Document{doc},
		SkipDurableSync: skipDurableSync,
	}); err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeInsertDuplicateID) {
			return bulkWriteOpResult{kind: "insert"}, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrDuplicateKeyInsert,
				"E11000 duplicate key error",
				"bulkWrite",
			)
		}
		return bulkWriteOpResult{}, lazyerrors.Error(err)
	}

	return bulkWriteOpResult{kind: "insert", inserted: 1}, nil
}

func execBulkWriteUpdate(ctx context.Context, db backends.Database, c backends.Collection, ns bulkWriteNS, op *types.Document, skipDurableSync, disablePushdown bool) (bulkWriteOpResult, error) {
	filter, _ := getDocumentField(op, "filter")
	if filter == nil {
		filter = must.NotFail(types.NewDocument())
	}

	// updateMods carries the $-operator doc or replacement doc. Server-side
	// bulkWrite uses "updateMods"; the rest of the common helpers expect
	// "u" / UpdateRaw, so we translate by building a common.Update directly.
	modsVal, err := op.Get("updateMods")
	if err != nil {
		return bulkWriteOpResult{}, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"update op missing 'updateMods'",
			"bulkWrite",
		)
	}

	multi := false
	if v, err := op.Get("multi"); err == nil {
		if b, ok := v.(bool); ok {
			multi = b
		}
	}

	upsert := false
	if v, err := op.Get("upsert"); err == nil {
		if b, ok := v.(bool); ok {
			upsert = b
		}
	}

	var arrayFilters *types.Array
	if v, err := op.Get("arrayFilters"); err == nil {
		if a, ok := v.(*types.Array); ok {
			arrayFilters = a
		}
	}

	update := &common.Update{
		Filter:       filter,
		UpdateRaw:    modsVal,
		Multi:        multi,
		Upsert:       upsert,
		ArrayFilters: arrayFilters,
	}

	switch u := modsVal.(type) {
	case *types.Document:
		update.Update = u
		hasOps, err := common.HasSupportedUpdateModifiers("bulkWrite", u)
		if err != nil {
			return bulkWriteOpResult{}, err
		}
		if hasOps {
			update.HasUpdateOperators = true
			if err := common.ValidateUpdateOperators("bulkWrite", u); err != nil {
				return bulkWriteOpResult{}, err
			}
		} else if multi {
			return bulkWriteOpResult{}, common.NewUpdateError(
				handlererrors.ErrFailedToParse,
				"multi update is not supported for replacement-style update",
				"bulkWrite",
			)
		}
	case *types.Array:
		update.IsPipeline = true
		update.Pipeline = u
	default:
		return bulkWriteOpResult{}, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"updateMods must be a document or pipeline array",
			"bulkWrite",
		)
	}

	// A matching-target collection may not exist yet when upsert=true. mirror
	// msg_update's behavior: ensure the collection is reachable, creating it
	// if necessary, before we run the update iterator.
	if err := db.CreateCollection(ctx, &backends.CreateCollectionParams{Name: ns.coll}); err != nil &&
		!backends.ErrorCodeIs(err, backends.ErrorCodeCollectionAlreadyExists) &&
		!backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid) {
		return bulkWriteOpResult{}, lazyerrors.Error(err)
	}

	var qp backends.QueryParams
	if !disablePushdown {
		qp.Filter = filter
	}

	queryRes, err := c.Query(ctx, &qp)
	if err != nil {
		return bulkWriteOpResult{}, lazyerrors.Error(err)
	}

	closer := iterator.NewMultiCloser()
	defer closer.Close()
	closer.Add(queryRes.Iter)

	iter := common.FilterIterator(queryRes.Iter, closer, filter)
	if !multi {
		iter = common.LimitIterator(iter, closer, 1)
	}

	updRes, err := common.UpdateDocument(ctx, c, "bulkWrite", iter, update, skipDurableSync)
	if err != nil {
		return bulkWriteOpResult{kind: "update"}, handleUpdateError(ns.db, ns.coll, "bulkWrite", err)
	}

	out := bulkWriteOpResult{
		kind:     "update",
		matched:  updRes.Matched.Count,
		modified: updRes.Modified.Count,
	}

	if updRes.Upserted.Doc != nil {
		out.upserted = 1
		// Per MongoDB semantics, upsert causes matched=1 but it's reported as
		// upserted rather than modified; the cumulative counters at the top
		// level add matched separately, so mirror that here.
		out.matched = 1
		out.upsertedID, _ = updRes.Upserted.Doc.Get("_id")
	}

	return out, nil
}

func execBulkWriteDelete(ctx context.Context, c backends.Collection, op *types.Document, skipDurableSync, disablePushdown bool) (bulkWriteOpResult, error) {
	filter, _ := getDocumentField(op, "filter")
	if filter == nil {
		filter = must.NotFail(types.NewDocument())
	}

	multi := false
	if v, err := op.Get("multi"); err == nil {
		if b, ok := v.(bool); ok {
			multi = b
		}
	}

	var qp backends.QueryParams
	if !disablePushdown {
		qp.Filter = filter
	}

	queryRes, err := c.Query(ctx, &qp)
	if err != nil {
		return bulkWriteOpResult{}, lazyerrors.Error(err)
	}

	var ids []any
	for {
		_, doc, err := queryRes.Iter.Next()
		if err != nil {
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}
			queryRes.Iter.Close()
			return bulkWriteOpResult{}, lazyerrors.Error(err)
		}

		matches, err := common.FilterDocument(doc, filter)
		if err != nil {
			queryRes.Iter.Close()
			return bulkWriteOpResult{}, lazyerrors.Error(err)
		}
		if !matches {
			continue
		}

		ids = append(ids, must.NotFail(doc.Get("_id")))

		if !multi {
			break
		}
	}
	queryRes.Iter.Close()

	if len(ids) == 0 {
		return bulkWriteOpResult{kind: "delete"}, nil
	}

	res, err := c.DeleteAll(ctx, &backends.DeleteAllParams{
		IDs:             ids,
		SkipDurableSync: skipDurableSync,
	})
	if err != nil {
		return bulkWriteOpResult{}, lazyerrors.Error(err)
	}

	return bulkWriteOpResult{kind: "delete", deleted: res.Deleted}, nil
}

// getArrayField fetches a required array-valued field from a command document,
// returning a ErrFailedToParse command error if absent or the wrong type.
func getArrayField(doc *types.Document, name string) (*types.Array, error) {
	v, err := doc.Get(name)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			fmt.Sprintf("missing required field %q", name),
			"bulkWrite",
		)
	}
	arr, ok := v.(*types.Array)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			fmt.Sprintf("field %q must be an array", name),
			"bulkWrite",
		)
	}
	return arr, nil
}

// getDocumentField fetches an optional document-valued field from a doc.
// Returns (nil, nil) when the field is absent; type mismatch is an error.
func getDocumentField(doc *types.Document, name string) (*types.Document, error) {
	v, err := doc.Get(name)
	if err != nil {
		return nil, nil
	}
	d, ok := v.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			fmt.Sprintf("field %q must be a document", name),
			"bulkWrite",
		)
	}
	return d, nil
}

// getInt64Value normalizes numeric BSON types (int32 / int64 / float64) to
// int64. Used to decode the nsIndex from an op's {insert: N} / {update: N} /
// {delete: N} field  -- drivers may send any of the three wire forms.
func getInt64Value(v any) (int64, error) {
	switch x := v.(type) {
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case float64:
		return int64(x), nil
	}
	return 0, fmt.Errorf("not a numeric value")
}

// extractCommandError pulls the MongoDB wire code and message from a handler
// error so we can embed it in a per-op response entry. Falls back to
// ErrOperationFailed when the error shape is opaque to avoid leaking raw Go
// error strings as if they were MongoDB codes.
func extractCommandError(err error) (int32, string) {
	var ce *handlererrors.CommandError
	if errors.As(err, &ce) {
		return int32(ce.Code()), ce.Err().Error()
	}
	return int32(handlererrors.ErrOperationFailed), err.Error()
}
