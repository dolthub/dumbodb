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
	"fmt"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) loadUserDoc(ctx context.Context, db, user string) (*types.Document, error) {
	adminDB, err := h.b.Database("admin")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	coll, err := adminDB.Collection("system.users")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	id := db + "." + user
	qr, err := coll.Query(ctx, &backends.QueryParams{Filter: must.NotFail(types.NewDocument("_id", id))})
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

		if idVal, _ := doc.Get("_id"); idVal == id {
			return doc, nil
		}
	}

	return nil, nil
}

func (h *Handler) validateUserRoles(ctx context.Context, refs *types.Array) error {
	for i := 0; i < refs.Len(); i++ {
		ref := must.NotFail(refs.Get(i)).(*types.Document)
		role := must.NotFail(ref.Get("role")).(string)
		db := must.NotFail(ref.Get("db")).(string)

		exists, err := h.roleExists(ctx, db, role)
		if err != nil {
			return err
		}

		if !exists {
			return handlererrors.NewCommandErrorMsg(
				handlererrors.ErrRoleNotFound,
				fmt.Sprintf("Could not find role: %s@%s", role, db),
			)
		}
	}

	return nil
}

func (h *Handler) modifyUser(ctx context.Context, dbName, username string, fn func(doc *types.Document) error) error {
	stored, err := h.loadUserDoc(ctx, dbName, username)
	if err != nil {
		return err
	}

	if stored == nil {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrUserNotFound,
			fmt.Sprintf("Could not find user %q for db %q", username, dbName),
		)
	}

	if err = fn(stored); err != nil {
		return err
	}

	adminDB, err := h.b.Database("admin")
	if err != nil {
		return lazyerrors.Error(err)
	}

	coll, err := adminDB.Collection("system.users")
	if err != nil {
		return lazyerrors.Error(err)
	}

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{stored}}); err != nil {
		return lazyerrors.Error(err)
	}

	h.BumpAuthGeneration()
	return nil
}

func (h *Handler) MsgGrantRolesToUser(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = common.RejectUnknownFields(document, "roles"); err != nil {
		return nil, err
	}

	dbName, username, err := roleCommandTarget(document)
	if err != nil {
		return nil, err
	}

	common.Ignored(document, h.L, "writeConcern", "comment")

	granted, err := requiredArray(document, "grantRolesToUser", "roles")
	if err != nil {
		return nil, err
	}

	refs, err := normalizeRoleRefs(granted, dbName)
	if err != nil {
		return nil, err
	}

	err = h.modifyUser(connCtx, dbName, username, func(doc *types.Document) error {
		if err := h.validateUserRoles(connCtx, refs); err != nil {
			return err
		}

		existing, _ := doc.Get("roles")
		roles, _ := existing.(*types.Array)
		doc.Set("roles", unionRoles(roles, refs))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return documentOpMsg(must.NotFail(types.NewDocument("ok", float64(1))))
}

func (h *Handler) MsgRevokeRolesFromUser(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = common.RejectUnknownFields(document, "roles"); err != nil {
		return nil, err
	}

	dbName, username, err := roleCommandTarget(document)
	if err != nil {
		return nil, err
	}

	common.Ignored(document, h.L, "writeConcern", "comment")

	revoked, err := requiredArray(document, "revokeRolesFromUser", "roles")
	if err != nil {
		return nil, err
	}

	refs, err := normalizeRoleRefs(revoked, dbName)
	if err != nil {
		return nil, err
	}

	err = h.modifyUser(connCtx, dbName, username, func(doc *types.Document) error {
		existing, _ := doc.Get("roles")
		roles, _ := existing.(*types.Array)
		doc.Set("roles", removeRoles(roles, refs))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return documentOpMsg(must.NotFail(types.NewDocument("ok", float64(1))))
}
