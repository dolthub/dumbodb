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

	"github.com/dolthub/dumbodb/internal/authz"
	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) MsgDropRole(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	common.Ignored(document, h.L, "writeConcern", "comment")

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	roleName, err := common.GetRequiredParam[string](document, document.Command())
	if err != nil {
		return nil, err
	}

	if authz.IsBuiltinRole(roleName) {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrBadValue,
			fmt.Sprintf("%s@%s is a built-in role and cannot be modified", roleName, dbName),
		)
	}

	coll, err := h.systemRolesCollection()
	if err != nil {
		return nil, err
	}

	res, err := coll.DeleteAll(connCtx, &backends.DeleteAllParams{IDs: []any{dbName + "." + roleName}})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if res.Deleted == 0 {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrRoleNotFound,
			fmt.Sprintf("Could not find role: %s@%s", roleName, dbName),
		)
	}

	if err = h.cascadeRoleRemoval(connCtx, dbName, roleName); err != nil {
		return nil, err
	}

	h.BumpAuthGeneration()

	return documentOpMsg(must.NotFail(types.NewDocument("ok", float64(1))))
}
