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
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// rejectRevisionQualifiedName rejects a real branch/revision after '@', matching
// dropDatabase (branchFromDBName): an all-digit '@' suffix stays a plain name.
func rejectRevisionQualifiedName(s, label string) error {
	base, _, _, err := branchFromDBName(s)
	if err != nil {
		return err
	}
	if base != s {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboUndrop: "+label+" must be a root database, not a revision-qualified name",
		)
	}
	return nil
}

// MsgDumboDBUndrop implements the `dumboUndrop` command.
//
// With no "name", it lists the databases available to undrop. With "name", it
// restores that soft-deleted database; "dropId" disambiguates when the name has
// more than one preserved drop. Admin-only.
func (h *Handler) MsgDumboDBUndrop(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	if err = common.RejectUnknownFields(document, "name", "toDatabase", "dropId", "purgeMatching"); err != nil {
		return nil, err
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

	purgeMatching, err := common.GetOptionalParam[*types.Document](document, "purgeMatching", nil)
	if err != nil {
		return nil, err
	}
	if purgeMatching != nil {
		return h.purgeMatchingDroppedDatabases(connCtx, vb, document, purgeMatching)
	}

	name, err := common.GetOptionalParam[string](document, "name", "")
	if err != nil {
		return nil, err
	}

	toDatabase, err := common.GetOptionalParam[string](document, "toDatabase", "")
	if err != nil {
		return nil, err
	}

	if toDatabase != "" && name == "" {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboUndrop: toDatabase requires name",
		)
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

	if err = rejectRevisionQualifiedName(name, "name"); err != nil {
		return nil, err
	}

	if err = rejectRevisionQualifiedName(toDatabase, "toDatabase"); err != nil {
		return nil, err
	}

	target := name
	if toDatabase != "" {
		target = toDatabase
	}
	if isSystemDatabase(target) {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboUndrop: cannot restore to system database "+target,
		)
	}

	dropID, err := common.GetOptionalParam[string](document, "dropId", "")
	if err != nil {
		return nil, err
	}

	res, err := vb.UndropDatabase(connCtx, &backends.UndropParams{Name: name, DropID: dropID, ToDatabase: toDatabase})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	return documentOpMsg(must.NotFail(types.NewDocument(
		"undropped", res.Name,
		"dropId", res.DropID,
		"ok", float64(1),
	)))
}

// purgeMatchingDroppedDatabaseFields are the only keys allowed in a purgeMatching
// filter; others are rejected so a typo cannot silently widen the purge.
var purgeMatchingDroppedDatabaseFields = map[string]struct{}{
	"name":          {},
	"dropId":        {},
	"droppedBefore": {},
}

// purgeMatchingDroppedDatabases handles `dumboUndrop` with a purgeMatching filter:
// it permanently removes preserved drops matching {name, dropId, droppedBefore}.
func (h *Handler) purgeMatchingDroppedDatabases(connCtx context.Context, vb backends.VersioningBackend, document, pm *types.Document) (*wire.OpMsg, error) {
	for _, k := range []string{"name", "dropId", "toDatabase"} {
		if document.Has(k) {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrOperationFailed,
				"dumboUndrop: purgeMatching cannot be combined with "+k,
			)
		}
	}

	for _, k := range pm.Keys() {
		if _, ok := purgeMatchingDroppedDatabaseFields[k]; !ok {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrOperationFailed,
				"dumboUndrop: purgeMatching has unknown field "+k+" (allowed: name, dropId, droppedBefore)",
			)
		}
	}

	name, err := common.GetOptionalParam[string](pm, "name", "")
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboUndrop: purgeMatching requires name",
		)
	}
	dropID, err := common.GetOptionalParam[string](pm, "dropId", "")
	if err != nil {
		return nil, err
	}
	droppedBefore, err := common.GetOptionalParam[time.Time](pm, "droppedBefore", time.Time{})
	if err != nil {
		return nil, err
	}

	params := &backends.PurgeDroppedParams{Name: name, DropID: dropID, DroppedBefore: droppedBefore}

	res, err := vb.PurgeDroppedDatabases(connCtx, params)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	arr := types.MakeArray(len(res.Purged))
	for _, d := range res.Purged {
		arr.Append(must.NotFail(types.NewDocument(
			"name", d.Name,
			"dropId", d.DropID,
			"droppedAt", time.Unix(0, d.DroppedAtUnixNano),
		)))
	}

	return documentOpMsg(must.NotFail(types.NewDocument(
		"purged", arr,
		"ok", float64(1),
	)))
}

// requireAdminDB returns an error unless the command targets the admin database.
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
