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
	"strings"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgDumboDBRemote implements the `dumboRemote` command (alias `doltRemote`).
// It adds, lists, or removes a named remote for the database, stored in
// admin.system.remotes. The command value is the action.
//
// Usage:
//
//	db.runCommand({dumboRemote: "add",    name: "origin", url: "file:///path"})
//	db.runCommand({dumboRemote: "list"})
//	db.runCommand({dumboRemote: "remove", name: "origin"})
//
// The target database is implicit from the connection.
func (h *Handler) MsgDumboDBRemote(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	if err = h.rejectClientIdentityFields(document); err != nil {
		return nil, err
	}

	if err = common.RejectUnknownFields(document, "name", "url"); err != nil {
		return nil, err
	}

	action, err := common.GetRequiredParam[string](document, document.Command())
	if err != nil {
		return nil, err
	}
	action = strings.ToLower(strings.TrimSpace(action))

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, _, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	name, err := common.GetOptionalParam[string](document, "name", "")
	if err != nil {
		return nil, err
	}

	remoteURL, err := common.GetOptionalParam[string](document, "url", "")
	if err != nil {
		return nil, err
	}

	switch action {
	case "add":
		if name == "" {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, "dumboRemote add: name is required", "name")
		}
		if remoteURL == "" {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, "dumboRemote add: url is required", "url")
		}
	case "remove":
		if name == "" {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, "dumboRemote remove: name is required", "name")
		}
	case "list":
		// no required fields
	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue, "dumboRemote: action must be add, list, or remove", document.Command())
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, "dumboRemote: versioning is not supported by the current backend")
	}

	res, err := vb.DumboDBRemote(connCtx, &backends.RemoteParams{
		DBName: dbName,
		Action: action,
		Name:   name,
		URL:    remoteURL,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	switch action {
	case "list":
		arr := types.MakeArray(len(res.Remotes))
		for _, r := range res.Remotes {
			arr.Append(must.NotFail(types.NewDocument("name", r.Name, "url", r.URL)))
		}
		return documentOpMsg(must.NotFail(types.NewDocument("remotes", arr, "ok", float64(1))))
	case "add":
		if len(res.Remotes) == 1 {
			return documentOpMsg(must.NotFail(types.NewDocument(
				"name", res.Remotes[0].Name,
				"url", res.Remotes[0].URL,
				"ok", float64(1),
			)))
		}
		return documentOpMsg(must.NotFail(types.NewDocument("ok", float64(1))))
	default: // remove
		return documentOpMsg(must.NotFail(types.NewDocument("ok", float64(1))))
	}
}
