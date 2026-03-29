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

	"github.com/FerretDB/wire"

	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
	"github.com/dolthub/dongo/internal/util/must"
)

// MsgAutoCompact implements the `autoCompact` command (MongoDB 8.0+).
//
// autoCompact must be run against the admin database. When run against any other
// database, MongoDB returns Unauthorized (code 13). Dongo mirrors this behavior.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgAutoCompact(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	dbName, _ := document.Get("$db")
	if db, ok := dbName.(string); !ok || db != "admin" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrUnauthorized,
			"autoCompact may only be run against the admin database.",
			"autoCompact",
		)
	}

	// When run against admin, return success (background compaction is a no-op in Dongo).
	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}
