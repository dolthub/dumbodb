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

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

// MsgAutoCompact implements the `autoCompact` command (MongoDB 8.0+).
//
// autoCompact enables background auto-compaction in MongoDB. DumboDB's Dolt-backed
// storage has no such background process, so rather than return a misleading
// success for a call that does nothing, it reports the command as unsupported.
func (h *Handler) MsgAutoCompact(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	if _, err := opMsgDocument(msg); err != nil {
		return nil, lazyerrors.Error(err)
	}

	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrNotImplemented,
		"autoCompact is not supported by DumboDB: background auto-compaction does not apply to its Dolt-backed storage",
		"autoCompact",
	)
}
