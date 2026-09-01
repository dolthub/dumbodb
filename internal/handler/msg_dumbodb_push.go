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

// MsgDumboDBPush implements the `dumboPush` command (alias `doltPush`). It
// pushes a branch's committed HEAD to a configured remote.
//
// Usage:
//
//	db.runCommand({dumboPush: 1, to: "origin"})
//	db.runCommand({dumboPush: 1, to: "origin", branch: "main", force: true})
//
// The target database is implicit from the connection; branch defaults to the
// connection's branch, then to the default branch.
func (h *Handler) MsgDumboDBPush(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	if err = h.rejectClientIdentityFields(document); err != nil {
		return nil, err
	}

	if err = common.RejectUnknownFields(document, "to", "refSpec", "force", "setUpstream"); err != nil {
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

	// 'to' is optional: an omitted target defaults to the branch's upstream.
	remote, err := common.GetOptionalParam[string](document, "to", "")
	if err != nil {
		return nil, err
	}

	// refSpec is an optional git-style [+]<src>[:<dst>]; empty means a bare push
	// of the connection branch to its upstream.
	refSpec, err := common.GetOptionalParam[string](document, "refSpec", "")
	if err != nil {
		return nil, err
	}

	force, err := common.GetOptionalBoolOrIntParam(document, "force", false)
	if err != nil {
		return nil, err
	}

	setUpstream, err := common.GetOptionalBoolOrIntParam(document, "setUpstream", false)
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, "dumboPush: versioning is not supported by the current backend")
	}

	res, err := vb.DumboDBPush(connCtx, &backends.PushParams{
		DBName:      dbName,
		Remote:      remote,
		ConnBranch:  connBranch,
		RefSpec:     refSpec,
		Force:       force,
		SetUpstream: setUpstream,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	out := must.NotFail(types.NewDocument(
		"remote", res.Remote,
	))
	// branch is the local branch pushed; empty when the source was a revision
	// expression rather than a branch.
	if res.Branch != "" {
		out.Set("branch", res.Branch)
	}
	// remoteBranch is shown when it differs from the local branch: a refspec
	// rename, or a revision source with no local branch name.
	if res.RemoteBranch != res.Branch {
		out.Set("remoteBranch", res.RemoteBranch)
	}
	// commitBefore is omitted when the push created the remote branch.
	if res.CommitBefore != "" {
		out.Set("commitBefore", res.CommitBefore)
	}
	out.Set("commitPushed", res.CommitPushed)
	out.Set("upToDate", res.UpToDate)
	out.Set("ok", float64(1))
	return documentOpMsg(out)
}
