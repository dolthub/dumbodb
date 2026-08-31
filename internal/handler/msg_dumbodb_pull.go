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
	"errors"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgDumboDBPull implements the `dumboPull` command (alias `doltPull`). Like git
// pull, it fetches from the branch's remote and merges the fetched commit into
// the current branch encoded in $db (format: "dbname@branch").
//
// Usage:
//
//	db.getSiblingDB("mydb@main").runCommand({dumboPull: 1})
//	db.getSiblingDB("mydb@main").runCommand({dumboPull: 1, from: "origin"})
//	db.getSiblingDB("mydb@main").runCommand({dumboPull: 1, ffOnly: true})
func (h *Handler) MsgDumboDBPull(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	if err = h.rejectClientIdentityFields(document); err != nil {
		return nil, err
	}

	if err = common.RejectUnknownFields(document, "from", "noFF", "ffOnly", "message", "author"); err != nil {
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

	remote, err := common.GetOptionalParam[string](document, "from", "")
	if err != nil {
		return nil, err
	}
	noFF, err := common.GetOptionalBoolOrIntParam(document, "noFF", false)
	if err != nil {
		return nil, err
	}
	ffOnly, err := common.GetOptionalBoolOrIntParam(document, "ffOnly", false)
	if err != nil {
		return nil, err
	}
	message, err := common.GetOptionalParam[string](document, "message", "")
	if err != nil {
		return nil, err
	}
	author, err := common.GetOptionalParam[string](document, "author", "")
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, "dumboPull: versioning is not supported by the current backend")
	}

	res, err := vb.DumboDBPull(connCtx, &backends.PullParams{
		DBName:  dbName,
		Branch:  connBranch,
		Remote:  remote,
		NoFF:    noFF,
		FFOnly:  ffOnly,
		Message: message,
		Author:  author,
	})
	if err != nil {
		// A conflicting merge reports per-collection conflicts, like dumboMerge.
		var conflictErr *backends.MergeConflictError
		if errors.As(err, &conflictErr) {
			conflictsArr := types.MakeArray(len(conflictErr.Conflicts))
			for _, c := range conflictErr.Conflicts {
				conflictsArr.Append(must.NotFail(types.NewDocument(
					"collection", c.Collection,
					"count", int32(c.Count),
				)))
			}
			return documentOpMsg(must.NotFail(types.NewDocument(
				"conflicts", conflictsArr,
				"ok", float64(0),
				"code", int32(handlererrors.ErrOperationFailed),
				"errmsg", conflictErr.Error(),
			)))
		}
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	out := must.NotFail(types.NewDocument(
		"remote", res.Remote,
		"branch", res.Branch,
	))
	if res.CommitBefore != "" {
		out.Set("commitBefore", res.CommitBefore)
	}
	out.Set("commitAfter", res.CommitAfter)
	out.Set("fastForward", res.FastForward)
	out.Set("alreadyUpToDate", res.AlreadyUpToDate)
	out.Set("ok", float64(1))
	return documentOpMsg(out)
}
