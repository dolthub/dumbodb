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
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

// rejectRetryableWriteOnStandalone reproduces MongoDB's refusal to accept a
// retryable write on a node that is not a replica set member or mongos.
//
// A retryable write is a CRUD command carrying txnNumber with no autocommit
// field; the driver attaches it only against a replica set, expecting the
// server to apply it at most once even across an automatic retry. A standalone
// mongod does not track (lsid, txnNumber) and answers IllegalOperation(20)
// rather than silently applying the write and breaking that contract.
// DumboDB advertises standalone whenever ReplSetName is empty (see MsgHello),
// so it must answer the same way instead of ignoring txnNumber.
//
// A multi-document transaction carries autocommit (false) and is served on the
// session-isolation path even when standalone, so it is deliberately left
// alone.
func rejectRetryableWriteOnStandalone(doc *types.Document, replSetName string) error {
	if replSetName != "" {
		return nil
	}
	if !doc.Has("txnNumber") || doc.Has("autocommit") {
		return nil
	}
	return handlererrors.NewCommandErrorMsg(
		handlererrors.ErrIllegalOperation,
		"Transaction numbers are only allowed on a replica set member or mongos",
	)
}
