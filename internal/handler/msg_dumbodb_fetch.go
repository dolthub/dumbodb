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

// MsgDumboDBFetch implements the `dumboFetch` command (alias `doltFetch`). Like
// git fetch, it pulls every branch from a configured remote into local
// remote-tracking refs refs/remotes/<remote>/<branch>, without touching any
// local branch head.
//
// Usage:
//
//	db.runCommand({dumboFetch: 1, from: "origin"})
//
// The target database is implicit from the connection.
func (h *Handler) MsgDumboDBFetch(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	if err = h.rejectClientIdentityFields(document); err != nil {
		return nil, err
	}

	if err = common.RejectUnknownFields(document, "from"); err != nil {
		return nil, err
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, _, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	// 'from' is optional: an omitted remote defaults to the default branch's upstream.
	remote, err := common.GetOptionalParam[string](document, "from", "")
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, "dumboFetch: versioning is not supported by the current backend")
	}

	res, err := vb.DumboDBFetch(connCtx, &backends.FetchParams{
		DBName: dbName,
		Remote: remote,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	branches := types.MakeArray(len(res.Branches))
	for _, fr := range res.Branches {
		branches.Append(must.NotFail(types.NewDocument("branch", fr.Branch, "commit", fr.Commit)))
	}

	return documentOpMsg(must.NotFail(types.NewDocument(
		"remote", res.Remote,
		"branches", branches,
		"ok", float64(1),
	)))
}
