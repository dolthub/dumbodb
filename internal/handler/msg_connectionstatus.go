// Copyright 2021 FerretDB Inc.
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
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgConnectionStatus implements `connectionStatus` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgConnectionStatus(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	users := types.MakeArray(1)
	roles := types.MakeArray(0)

	if username, _, _, db := conninfo.Get(connCtx).Auth(); username != "" {
		users.Append(must.NotFail(types.NewDocument(
			"user", username,
			"db", db,
		)))

		stored, err := h.authenticatedUserRoles(connCtx, db, username)
		if err != nil {
			return nil, err
		}
		if stored != nil {
			roles = stored
		}
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"authInfo", must.NotFail(types.NewDocument(
				"authenticatedUsers", users,
				"authenticatedUserRoles", roles,
			)),
			"ok", float64(1),
		)),
	)
}

func (h *Handler) authenticatedUserRoles(ctx context.Context, db, username string) (*types.Array, error) {
	adminDB, err := h.b.Database("admin")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	usersCol, err := adminDB.Collection("system.users")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	filter := must.NotFail(types.NewDocument("_id", db+"."+username))

	qr, err := usersCol.Query(ctx, &backends.QueryParams{Filter: filter})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	defer qr.Iter.Close()

	for {
		_, doc, err := qr.Iter.Next()

		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		matches, err := common.FilterDocument(doc, filter)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		if matches {
			if r, ok := must.NotFail(doc.Get("roles")).(*types.Array); ok {
				return r, nil
			}
			break
		}
	}

	return types.MakeArray(0), nil
}
