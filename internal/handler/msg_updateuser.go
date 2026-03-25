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
	"fmt"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/handler/common"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/handler/handlerparams"
	"github.com/dolthub/dongo/internal/handler/users"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
	"github.com/dolthub/dongo/internal/util/must"
	"github.com/dolthub/dongo/internal/util/password"
)

// MsgUpdateUser implements `updateUser` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgUpdateUser(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	username, err := common.GetRequiredParam[string](document, document.Command())
	if err != nil {
		return nil, err
	}

	if err = common.UnimplementedNonDefault(document, "customData", func(v any) bool {
		if v == nil || v == types.Null {
			return true
		}

		cd, ok := v.(*types.Document)
		return ok && cd.Len() == 0
	}); err != nil {
		return nil, err
	}

	// Accept any roles value; dongo doesn't enforce RBAC but must not reject
	// non-empty roles to be compatible with MongoDB clients.
	common.Ignored(document, h.L, "writeConcern", "authenticationRestrictions", "comment", "roles")

	defMechanisms := must.NotFail(types.NewArray("SCRAM-SHA-1", "SCRAM-SHA-256"))

	mechanisms, err := common.GetOptionalParam(document, "mechanisms", defMechanisms)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	iter := mechanisms.Iterator()
	defer iter.Close()

	for {
		var v any
		_, v, err = iter.Next()

		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		switch v {
		case "SCRAM-SHA-1", "SCRAM-SHA-256":
			// do nothing
		default:
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrBadValue,
				fmt.Sprintf("Unknown auth mechanism '%s'", v),
			)
		}
	}

	var credentials *types.Document

	if document.Has("pwd") {
		pwd, _ := document.Get("pwd")
		userPassword, ok := pwd.(string)

		if !ok {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf("BSON field 'updateUser.pwd' is the wrong type '%s', expected type 'string'",
					handlerparams.AliasFromType(pwd),
				),
			)
		}

		if userPassword == "" {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrSetEmptyPassword,
				"Password cannot be empty",
			)
		}

		credentials, err = users.MakeCredentials(username, password.WrapPassword(userPassword), mechanisms)
		if err != nil {
			return nil, err
		}
	}

	adminDB, err := h.b.Database("admin")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	usersCol, err := adminDB.Collection("system.users")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	filter, err := usersInfoFilter(false, false, "", []usersInfoPair{{username: username, db: dbName}})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Filter isn't being passed to the query as we are filtering after retrieving all data
	// from the database due to limitations of the internal/backends filters.
	qr, err := usersCol.Query(connCtx, nil)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	defer qr.Iter.Close()

	var saved *types.Document

	for {
		var v *types.Document
		_, v, err = qr.Iter.Next()

		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		var matches bool
		matches, err = common.FilterDocument(v, filter)
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		if matches {
			saved = v
			break
		}
	}

	if saved == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrUserNotFound,
			fmt.Sprintf("User %s@%s not found", username, dbName),
		)
	}

	var changes bool

	if credentials != nil {
		changes = true

		saved.Set("credentials", credentials)
	}

	if document.Has("roles") {
		changes = true
		// Roles are accepted but not enforced (no RBAC). The stored value remains
		// an empty array; we only set changes=true to satisfy the "at least one
		// field" requirement.
	}

	if !changes {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrBadValue,
			"Must specify at least one field to update in updateUser",
		)
	}

	_, err = usersCol.UpdateAll(connCtx, &backends.UpdateAllParams{Docs: []*types.Document{saved}})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}
