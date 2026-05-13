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
// Since DumboDB does not support multi-document ACID transactions, this
// is a no-op acknowledgement. Individual operations within a
// "transaction" are applied immediately without isolation.
func (h *Handler) MsgCommitTransaction(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}

// MsgAbortTransaction implements the `abortTransaction` command.
//
// Since DumboDB does not support multi-document ACID transactions, this
// is a no-op acknowledgement. Operations already applied cannot be
// rolled back.
func (h *Handler) MsgAbortTransaction(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}

func (h *Handler) MsgEndSessions(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}
