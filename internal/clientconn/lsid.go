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

package clientconn

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

// startTransaction is unavailable in
// --session-isolation mode; the user is expected to issue writes directly
// and call doltCommit to merge them back to HEAD.
var errSessionIsolationRejectStartTransaction = errors.New(
	"Transactions are not available in session-isolation mode. Use doltCommit instead.",
)

func extractAndSetLSID(ctx context.Context, doc *types.Document) {
	if doc == nil || !doc.Has("lsid") {
		return
	}
	v, err := doc.Get("lsid")
	if err != nil {
		return
	}
	lsidDoc, ok := v.(*types.Document)
	if !ok || !lsidDoc.Has("id") {
		return
	}
	idVal, err := lsidDoc.Get("id")
	if err != nil {
		return
	}
	bin, ok := idVal.(types.Binary)
	if !ok {
		return
	}

	conninfo.Get(ctx).SetLSID(hex.EncodeToString(bin.B))
}

// extractAndSetTransactionFlag observes the wire-protocol transaction
// markers and reflects them onto ConnInfo for the current frame. Two
// markers, distinct semantics:
//
//   - startTransaction:true marks the FIRST frame of a new txn. Clears any
//     prior aborted state, sets InTransaction.
//   - autocommit:false marks ANY frame as part of a txn (including a
//     continuation that arrives on a fresh TCP connection -- the
//     SessionRegistry resumes the underlying DoltSession but ConnInfo is
//     per-TCP-connection, so the second frame must set InTransaction
//     from the wire field rather than carrying it forward). A frame
//     without it clears InTransaction for the same reason.
//
// Returns whether startTransaction:true was observed; the conn-level
// routing uses that specifically to reject the wire frame in
// --session-isolation mode.
func extractAndSetTransactionFlag(ctx context.Context, doc *types.Document) (started bool) {
	if doc == nil {
		return false
	}
	ci := conninfo.Get(ctx)

	if doc.Has("startTransaction") {
		if v, err := doc.Get("startTransaction"); err == nil {
			if b, ok := v.(bool); ok && b {
				ci.SetTxnAborted(false)
				ci.SetInTransaction(true)
				started = true
			}
		}
	}

	// A transaction belongs to its session, and every command in one says so
	// with autocommit:false. One without it is not in a transaction, whatever
	// an earlier command on this connection was in: a driver sends any
	// session's commands down any pooled connection.
	inTxn := started
	if v, err := doc.Get("autocommit"); err == nil {
		if b, ok := v.(bool); ok && !b {
			inTxn = true
		}
	}
	if !inTxn {
		ci.SetInTransaction(false)
	} else if !started {
		ci.SetInTransaction(true)
	}

	return started
}

// abortedTransactionError is mongod's error for a statement in a
// transaction that an earlier failure aborted.
func abortedTransactionError(doc *types.Document) error {
	msg := "Transaction has been aborted."
	if n, err := doc.Get("txnNumber"); err == nil {
		msg = fmt.Sprintf("Transaction with { txnNumber: %v } has been aborted.", n)
	}
	return handlererrors.NewCommandErrorWithLabels(
		handlererrors.ErrNoSuchTransaction,
		errors.New(msg),
		handlererrors.TransientTransactionErrorLabel,
	)
}

// replyHasWriteErrors reports whether a successful write command's reply
// carries writeErrors, which also abort a transaction in mongod.
func replyHasWriteErrors(resMsg *wire.OpMsg) bool {
	if resMsg == nil {
		return false
	}
	raw, err := resMsg.RawDocument()
	if err != nil {
		return false
	}
	doc, err := bson.DecodeRawDocument(raw)
	if err != nil {
		return false
	}
	writeErrors, err := doc.Get("writeErrors")
	if err != nil {
		return false
	}
	arr, ok := writeErrors.(*types.Array)
	return ok && arr.Len() > 0
}
