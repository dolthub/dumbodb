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

// MongoDB has no separate startTransaction command; the driver flags the
// first command in a txn with startTransaction:true. Returns whether the
// flag was observed; conn-level routing uses this to reject the wire
// frame in --session-isolation mode.
func extractAndSetTransactionFlag(ctx context.Context, doc *types.Document) (started bool) {
	if doc == nil || !doc.Has("startTransaction") {
		return false
	}
	v, err := doc.Get("startTransaction")
	if err != nil {
		return false
	}
	if b, ok := v.(bool); ok && b {
		conninfo.Get(ctx).SetInTransaction(true)
		return true
	}
	return false
}
