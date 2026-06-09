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
)

// MsgConvertToCapped implements `convertToCapped` command.
//
// DumboDB does not support capped collections: eviction depends on a global
// insertion order that is not well-defined across branches and merges. The
// command is rejected unconditionally.
func (h *Handler) MsgConvertToCapped(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrInvalidOptions,
		"capped collections are not supported by DumboDB",
		"convertToCapped",
	)
}
