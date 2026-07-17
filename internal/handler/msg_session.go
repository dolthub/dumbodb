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
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/FerretDB/wire"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgStartSession implements the `startSession` command. It returns a
// freshly generated logical session id that the driver uses to annotate
// subsequent operations, including the lsid/txnNumber that scope a
// multi-document transaction.
func (h *Handler) MsgStartSession(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, lazyerrors.Error(err)
	}

	sessionID := bson.Binary{
		Subtype: 0x04, // UUID subtype
		Data:    id[:],
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"id", must.NotFail(types.NewDocument(
				"id", sessionID,
			)),
			"ok", float64(1),
		)),
	)
}

// MsgCommitTransaction implements the `commitTransaction` command.
func (h *Handler) MsgCommitTransaction(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	ci := conninfo.Get(connCtx)
	if ci.TxnAborted() {
		// dispatch's EnsureTxn opens a fresh dsess txn before this handler
		// runs; clear it so it doesn't leak writes into the next command.
		if sab, ok := h.b.(backends.SessionAwareBackend); ok {
			sab.OnTransactionAbort(ci.Owner())
		}
		ci.SetInTransaction(false)
		return nil, handlererrors.NewCommandError(
			handlererrors.ErrorCode(251),
			errors.New("Transaction was aborted by a prior server-side rejection."),
		)
	}
	if sab, ok := h.b.(backends.SessionAwareBackend); ok {
		if err := sab.OnTransactionCommit(connCtx, ci.Owner()); err != nil {
			return nil, lazyerrors.Error(err)
		}
	}
	ci.SetInTransaction(false)

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}

// MsgAbortTransaction implements the `abortTransaction` command.
func (h *Handler) MsgAbortTransaction(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	ci := conninfo.Get(connCtx)
	if sab, ok := h.b.(backends.SessionAwareBackend); ok {
		sab.OnTransactionAbort(ci.Owner())
	}
	ci.SetInTransaction(false)

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}

// MsgEndSessions implements the `endSessions` command. It must release
// every lsid in the wire body's array, not just ci.Owner(): a pooled
// driver checks out several implicit sessions per connection and only
// one of them is the connection's owner.
func (h *Handler) MsgEndSessions(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	ci := conninfo.Get(connCtx)
	if sab, ok := h.b.(backends.SessionAwareBackend); ok {
		sab.OnSessionEnd(ci.Owner())
	}
	ci.SetInTransaction(false)

	endLsidsFromMsg(h, msg)

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}

// endLsidsFromMsg silently skips malformed entries; endSessions is
// advisory and a bad wire frame should never produce a server error.
func endLsidsFromMsg(h *Handler, msg *wire.OpMsg) {
	reg := h.SessionRegistry()
	if reg == nil || msg == nil {
		return
	}
	doc, err := opMsgDocument(msg)
	if err != nil || doc == nil || !doc.Has("endSessions") {
		return
	}
	v, err := doc.Get("endSessions")
	if err != nil {
		return
	}
	arr, ok := v.(*types.Array)
	if !ok {
		return
	}
	for i := 0; i < arr.Len(); i++ {
		entry, err := arr.Get(i)
		if err != nil {
			continue
		}
		entryDoc, ok := entry.(*types.Document)
		if !ok || !entryDoc.Has("id") {
			continue
		}
		idVal, err := entryDoc.Get("id")
		if err != nil {
			continue
		}
		bin, ok := idVal.(types.Binary)
		if !ok {
			continue
		}
		reg.End(hex.EncodeToString(bin.B))
	}
}
