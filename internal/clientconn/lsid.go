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

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/types"
)

// Per the session-isolation design: startTransaction is unavailable in
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
//     from the wire field rather than carrying it forward).
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

	if doc.Has("autocommit") {
		if v, err := doc.Get("autocommit"); err == nil {
			if b, ok := v.(bool); ok && !b {
				ci.SetInTransaction(true)
			}
		}
	}

	return started
}
