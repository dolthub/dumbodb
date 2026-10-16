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

// MsgDumboDBBackup implements the `dumboBackup` command (alias `doltBackup`).
// It manages Dolt-style backups of one database: a backup holds the
// database's whole root, so every branch, tag and working set. It must be run
// against the admin database.
//
// Usage:
//
//	admin.runCommand({dumboBackup: 1, action: "add",      db: "mydb", name: "nightly", url: "file:///path"})
//	admin.runCommand({dumboBackup: 1, action: "list",     db: "mydb"})
//	admin.runCommand({dumboBackup: 1, action: "remove",   db: "mydb", name: "nightly"})
//	admin.runCommand({dumboBackup: 1, action: "sync",     db: "mydb", name: "nightly"})
//	admin.runCommand({dumboBackup: 1, action: "sync-url", db: "mydb", url: "file:///path"})
//	admin.runCommand({dumboBackup: 1, action: "restore",  url: "file:///path", as: "mydb2", force: false})
func (h *Handler) MsgDumboDBBackup(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	if err = h.rejectClientIdentityFields(document); err != nil {
		return nil, err
	}

	if err = common.RejectUnknownFields(document, "action", "db", "name", "url", "as", "force"); err != nil {
		return nil, err
	}

	if err = requireAdminDB(document, "dumboBackup"); err != nil {
		return nil, err
	}

	action, err := common.GetRequiredParam[string](document, "action")
	if err != nil {
		return nil, err
	}
	action = normalizeBackupAction(action)

	params := &backends.BackupParams{Action: action}
	if params.DBName, err = common.GetOptionalParam[string](document, "db", ""); err != nil {
		return nil, err
	}
	if params.Name, err = common.GetOptionalParam[string](document, "name", ""); err != nil {
		return nil, err
	}
	if params.URL, err = common.GetOptionalParam[string](document, "url", ""); err != nil {
		return nil, err
	}
	if params.As, err = common.GetOptionalParam[string](document, "as", ""); err != nil {
		return nil, err
	}
	if params.Force, err = common.GetOptionalBoolOrIntParam(document, "force", false); err != nil {
		return nil, err
	}

	if err = requireBackupFields(params); err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, "dumboBackup: versioning is not supported by the current backend")
	}

	res, err := vb.DumboDBBackup(connCtx, params)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	switch action {
	case "list":
		arr := types.MakeArray(len(res.Backups))
		for _, bk := range res.Backups {
			arr.Append(must.NotFail(types.NewDocument("name", bk.Name, "url", bk.URL)))
		}
		return documentOpMsg(must.NotFail(types.NewDocument("backups", arr, "ok", float64(1))))
	case "add":
		out := must.NotFail(types.NewDocument())
		if len(res.Backups) == 1 {
			out.Set("name", res.Backups[0].Name)
			out.Set("url", res.Backups[0].URL)
		}
		out.Set("ok", float64(1))
		return documentOpMsg(out)
	case "sync", "sync-url":
		return documentOpMsg(must.NotFail(types.NewDocument(
			"url", res.URL,
			"root", res.Root,
			"upToDate", res.UpToDate,
			"ok", float64(1),
		)))
	case "restore":
		return documentOpMsg(must.NotFail(types.NewDocument(
			"db", res.DB,
			"url", res.URL,
			"root", res.Root,
			"ok", float64(1),
		)))
	default: // remove
		return documentOpMsg(must.NotFail(types.NewDocument("ok", float64(1))))
	}
}

func normalizeBackupAction(action string) string {
	return strings.ToLower(strings.TrimSpace(action))
}

func requireBackupFields(params *backends.BackupParams) error {
	missing := func(field string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
			"dumboBackup "+params.Action+": "+field+" is required", field)
	}

	switch params.Action {
	case "add":
		if params.DBName == "" {
			return missing("db")
		}
		if params.Name == "" {
			return missing("name")
		}
		if params.URL == "" {
			return missing("url")
		}
	case "remove", "sync":
		if params.DBName == "" {
			return missing("db")
		}
		if params.Name == "" {
			return missing("name")
		}
	case "sync-url":
		if params.DBName == "" {
			return missing("db")
		}
		if params.URL == "" {
			return missing("url")
		}
	case "list":
		if params.DBName == "" {
			return missing("db")
		}
	case "restore":
		if params.URL == "" {
			return missing("url")
		}
		if params.As == "" {
			return missing("as")
		}
	default:
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
			"dumboBackup: action must be add, list, remove, sync, sync-url, or restore", "action")
	}
	return nil
}

// backupMutatesState reports whether a dumboBackup call writes to this
// server. list, sync and sync-url only read local data, so they may run on a
// secondary; anything else, including an unrecognized action, may not.
func backupMutatesState(msg *wire.OpMsg) bool {
	document, err := opMsgDocument(msg)
	if err != nil {
		return true
	}
	value, _ := document.Get("action")
	action, _ := value.(string)
	switch normalizeBackupAction(action) {
	case "list", "sync", "sync-url":
		return false
	default:
		return true
	}
}
