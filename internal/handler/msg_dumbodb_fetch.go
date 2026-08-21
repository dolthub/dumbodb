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

// MsgDumboDBFetch implements the `dumboFetch` command (alias `doltFetch`). It
// fetches a branch from a configured remote into the local store and updates
// the local remote-tracking ref, without touching the local branch head.
//
// Usage:
//
//	db.runCommand({dumboFetch: 1, from: "origin"})
//	db.runCommand({dumboFetch: 1, from: "origin", branch: "main"})
//
// The target database is implicit from the connection; branch defaults to the
// connection's branch, then to the default branch.
func (h *Handler) MsgDumboDBFetch(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	if err = h.rejectClientIdentityFields(document); err != nil {
		return nil, err
	}

	if err = common.RejectUnknownFields(document, "from", "branch"); err != nil {
		return nil, err
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, connBranch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	remote, err := common.GetRequiredParam[string](document, "from")
	if err != nil {
		return nil, err
	}

	branch, err := common.GetOptionalParam[string](document, "branch", "")
	if err != nil {
		return nil, err
	}
	if branch == "" {
		branch = connBranch
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, "dumboFetch: versioning is not supported by the current backend")
	}

	res, err := vb.DumboDBFetch(connCtx, &backends.FetchParams{
		DBName: dbName,
		Remote: remote,
		Branch: branch,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	return documentOpMsg(must.NotFail(types.NewDocument(
		"remote", res.Remote,
		"branch", res.Branch,
		"commit", res.Commit,
		"upToDate", res.UpToDate,
		"ok", float64(1),
	)))
}
