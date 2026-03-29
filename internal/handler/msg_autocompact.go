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
	"github.com/dolthub/dongo/internal/util/lazyerrors"
)

// MsgAutoCompact implements `autoCompact` command.
//
// autoCompact is a MongoDB 8.0 admin-only command that enables/disables background
// compaction. Dongo returns Unauthorized to match MongoDB's behaviour when called
// outside the admin database.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgAutoCompact(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	_, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrUnauthorized,
		"autoCompact may only be run against the admin database.",
		"autoCompact",
	)
}
