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

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgDumboDBClone implements the `dumboClone` command (alias `doltClone`). It
// creates a new server-side database by cloning a remote (file:// or an http(s)
// gRPC remote such as DoltHub). It must be run against the admin database.
//
// Usage:
//
//	admin.runCommand({dumboClone: 1, from: "file:///path/to/remote", as: "newdb"})
//	admin.runCommand({dumboClone: 1, from: "https://doltremoteapi.dolthub.com/org/repo", as: "newdb"})
func (h *Handler) MsgDumboDBClone(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	if err = h.rejectClientIdentityFields(document); err != nil {
		return nil, err
	}

	if err = common.RejectUnknownFields(document, "from", "as"); err != nil {
		return nil, err
	}

	if err = requireAdminDB(document, "dumboClone"); err != nil {
		return nil, err
	}

	from, err := common.GetRequiredParam[string](document, "from")
	if err != nil {
		return nil, err
	}

	as, err := common.GetRequiredParam[string](document, "as")
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, "dumboClone: versioning is not supported by the current backend")
	}

	res, err := vb.DumboDBClone(connCtx, &backends.CloneParams{From: from, As: as})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	return documentOpMsg(must.NotFail(types.NewDocument(
		"db", res.DB,
		"from", res.URL,
		"ok", float64(1),
	)))
}
