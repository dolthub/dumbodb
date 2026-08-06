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
	"fmt"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) MsgUpdateRole(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = common.RejectUnknownFields(document, "privileges", "roles", "authenticationRestrictions"); err != nil {
		return nil, err
	}

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	roleName, err := common.GetRequiredParam[string](document, document.Command())
	if err != nil {
		return nil, err
	}

	common.Ignored(document, h.L, "writeConcern", "comment")

	stored, err := h.loadRoleDoc(connCtx, dbName, roleName)
	if err != nil {
		return nil, err
	}

	if stored == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrRoleNotFound,
			fmt.Sprintf("Could not find role: %s@%s", roleName, dbName),
		)
	}

	privileges, err := common.GetOptionalParam[*types.Array](document, "privileges", nil)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if privileges != nil {
		storedPrivileges, err := normalizePrivileges(privileges, dbName)
		if err != nil {
			return nil, err
		}
		stored.Set("privileges", storedPrivileges)
	}

	inherited, err := common.GetOptionalParam[*types.Array](document, "roles", nil)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if inherited != nil {
		storedRoles, err := h.normalizeInheritedRoles(connCtx, inherited, roleName, dbName)
		if err != nil {
			return nil, err
		}
		stored.Set("roles", storedRoles)
	}

	if restrictions, err := common.GetOptionalParam[*types.Array](document, "authenticationRestrictions", nil); err == nil && restrictions != nil {
		stored.Set("authenticationRestrictions", restrictions)
	}

	coll, err := h.systemRolesCollection()
	if err != nil {
		return nil, err
	}

	if _, err = coll.UpdateAll(connCtx, &backends.UpdateAllParams{Docs: []*types.Document{stored}}); err != nil {
		return nil, lazyerrors.Error(err)
	}

	h.BumpAuthGeneration()

	return documentOpMsg(must.NotFail(types.NewDocument("ok", float64(1))))
}
