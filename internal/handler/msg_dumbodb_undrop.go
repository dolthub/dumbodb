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
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgDumboDBUndrop implements the `dumboUndrop` command.
//
// With no "name", it lists the databases available to undrop. With "name", it
// restores that soft-deleted database; "dropId" disambiguates when the name has
// more than one quarantined drop. Admin-only.
func (h *Handler) MsgDumboDBUndrop(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	if err = requireAdminDB(document, "dumboUndrop"); err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboUndrop: versioning is not supported by the current backend",
		)
	}

	name, err := common.GetOptionalParam[string](document, "name", "")
	if err != nil {
		return nil, err
	}

	if name == "" {
		res, err := vb.ListDroppedDatabases(connCtx)
		if err != nil {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
		}

		arr := types.MakeArray(len(res.Databases))
		for _, d := range res.Databases {
			arr.Append(must.NotFail(types.NewDocument(
				"name", d.Name,
				"dropId", d.DropID,
				"droppedAt", time.Unix(0, d.DroppedAtUnixNano),
			)))
		}

		return documentOpMsg(must.NotFail(types.NewDocument(
			"dropped", arr,
			"ok", float64(1),
		)))
	}

	if strings.Contains(name, dbBranchSep) {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboUndrop: name must be a root database, not a revision-qualified name",
		)
	}

	dropID, err := common.GetOptionalParam[string](document, "dropId", "")
	if err != nil {
		return nil, err
	}

	res, err := vb.UndropDatabase(connCtx, &backends.UndropParams{Name: name, DropID: dropID})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	return documentOpMsg(must.NotFail(types.NewDocument(
		"undropped", res.Name,
		"dropId", res.DropID,
		"ok", float64(1),
	)))
}

// requireAdminDB returns an error unless the command targets the admin database.
// Instance-level operations (undrop) are not scoped to a single database and are
// only accepted against admin to avoid ambiguity and limit blast radius.
func requireAdminDB(document *types.Document, command string) error {
	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return err
	}
	if dbName != "admin" {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			command+": can only be run against the admin database",
		)
	}
	return nil
}
