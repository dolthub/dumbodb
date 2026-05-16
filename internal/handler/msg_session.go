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

	"github.com/FerretDB/wire"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgStartSession implements the `startSession` command.
//
// DumboDB does not implement multi-document transactions, but the driver
// requires a valid session ID to annotate operations. We return a
// well-formed session document so the driver can proceed. Operations
// tagged with lsid/txnNumber are handled on a per-command basis (the
// fields are accepted and ignored).
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
//
// Merges the connection's per-(owner, branch) pending working-set overlay
// into the committed working set across every open database, persists,
// and releases any document locks the owner holds. The session exits
// the in-transaction state.
//
// Calling commit when not in a transaction is harmless: the backend's
// overlay is empty for this owner so nothing changes, and the lock
// release is a no-op.
func (h *Handler) MsgCommitTransaction(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	ci := conninfo.Get(connCtx)
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
//
// Discards the connection's per-(owner, branch) pending working-set
// overlay without touching the committed working set, releases the
// owner's document locks, and exits the in-transaction state.
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

// MsgEndSessions implements the `endSessions` command.
//
// Per the session-isolation design, the backend may hold per-session
// state -- chiefly document locks and pending-transaction overlays --
// keyed by ConnInfo.Owner(). On endSessions we ask the backend to drop
// that state via the optional SessionAwareBackend interface. Backends
// without session state (the stub backend) need not implement it.
//
// Any in-progress transaction on this connection is implicitly aborted:
// pending writes are discarded, the in-transaction flag is cleared so
// follow-up commands behave as new implicit sessions.
func (h *Handler) MsgEndSessions(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	ci := conninfo.Get(connCtx)
	if sab, ok := h.b.(backends.SessionAwareBackend); ok {
		sab.OnSessionEnd(ci.Owner())
	}
	ci.SetInTransaction(false)

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}
