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

func (h *Handler) modifyRole(ctx context.Context, dbName, roleName string, fn func(doc *types.Document) error) error {
	stored, err := h.loadRoleDoc(ctx, dbName, roleName)
	if err != nil {
		return err
	}

	if stored == nil {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrRoleNotFound,
			fmt.Sprintf("Role %s@%s not found", roleName, dbName),
		)
	}

	if err = fn(stored); err != nil {
		return err
	}

	coll, err := h.systemRolesCollection()
	if err != nil {
		return err
	}

	if _, err = coll.UpdateAll(ctx, &backends.UpdateAllParams{Docs: []*types.Document{stored}}); err != nil {
		return lazyerrors.Error(err)
	}

	h.BumpAuthGeneration()
	return nil
}

func (h *Handler) MsgGrantPrivilegesToRole(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	dbName, roleName, err := roleCommandTarget(document)
	if err != nil {
		return nil, err
	}

	common.Ignored(document, h.L, "writeConcern", "comment")

	granted, err := requiredArray(document, "grantPrivilegesToRole", "privileges")
	if err != nil {
		return nil, err
	}

	normalized, err := normalizePrivileges(granted, dbName)
	if err != nil {
		return nil, err
	}

	err = h.modifyRole(connCtx, dbName, roleName, func(doc *types.Document) error {
		existing, _ := doc.Get("privileges")
		privs, _ := existing.(*types.Array)
		doc.Set("privileges", unionPrivileges(privs, normalized))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return documentOpMsg(must.NotFail(types.NewDocument("ok", float64(1))))
}

func (h *Handler) MsgRevokePrivilegesFromRole(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	dbName, roleName, err := roleCommandTarget(document)
	if err != nil {
		return nil, err
	}

	common.Ignored(document, h.L, "writeConcern", "comment")

	revoked, err := requiredArray(document, "revokePrivilegesFromRole", "privileges")
	if err != nil {
		return nil, err
	}

	err = h.modifyRole(connCtx, dbName, roleName, func(doc *types.Document) error {
		existing, _ := doc.Get("privileges")
		privs, _ := existing.(*types.Array)
		doc.Set("privileges", revokePrivileges(privs, revoked))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return documentOpMsg(must.NotFail(types.NewDocument("ok", float64(1))))
}

func (h *Handler) MsgGrantRolesToRole(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	dbName, roleName, err := roleCommandTarget(document)
	if err != nil {
		return nil, err
	}

	common.Ignored(document, h.L, "writeConcern", "comment")

	granted, err := requiredArray(document, "grantRolesToRole", "roles")
	if err != nil {
		return nil, err
	}

	refs, err := h.normalizeInheritedRoles(connCtx, granted, roleName, dbName)
	if err != nil {
		return nil, err
	}

	target := authz.Role{Role: roleName, DB: dbName}
	for _, r := range h.roleRefClosure(connCtx, rolesFromArray(refs)) {
		if r == target {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrInvalidRoleModification,
				fmt.Sprintf("Granting roles to %s@%s would introduce a cycle in the role graph", roleName, dbName),
			)
		}
	}

	err = h.modifyRole(connCtx, dbName, roleName, func(doc *types.Document) error {
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

func (h *Handler) MsgRevokeRolesFromRole(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	dbName, roleName, err := roleCommandTarget(document)
	if err != nil {
		return nil, err
	}

	common.Ignored(document, h.L, "writeConcern", "comment")

	revoked, err := requiredArray(document, "revokeRolesFromRole", "roles")
	if err != nil {
		return nil, err
	}

	refs, err := normalizeRoleRefs(revoked, dbName)
	if err != nil {
		return nil, err
	}

	err = h.modifyRole(connCtx, dbName, roleName, func(doc *types.Document) error {
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

func roleCommandTarget(document *types.Document) (db, role string, err error) {
	db, err = common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return "", "", err
	}

	role, err = common.GetRequiredParam[string](document, document.Command())
	if err != nil {
		return "", "", err
	}

	return db, role, nil
}

func unionPrivileges(existing, add *types.Array) *types.Array {
	out := copyArray(existing)

	for i := 0; i < add.Len(); i++ {
		addPriv := must.NotFail(add.Get(i)).(*types.Document)
		addRes := must.NotFail(addPriv.Get("resource")).(*types.Document)
		addKey := resourceKey(addRes)

		merged := false
		for j := 0; j < out.Len(); j++ {
			priv := must.NotFail(out.Get(j)).(*types.Document)
			res := must.NotFail(priv.Get("resource")).(*types.Document)
			if resourceKey(res) != addKey {
				continue
			}

			actions := must.NotFail(priv.Get("actions")).(*types.Array)
			priv.Set("actions", unionStrings(actions, must.NotFail(addPriv.Get("actions")).(*types.Array)))
			merged = true
			break
		}

		if !merged {
			out.Append(addPriv)
		}
	}

	return out
}

func revokePrivileges(existing, remove *types.Array) *types.Array {
	if existing == nil {
		return types.MakeArray(0)
	}

	removeByKey := map[string]*types.Array{}
	for i := 0; i < remove.Len(); i++ {
		rp := must.NotFail(remove.Get(i)).(*types.Document)
		res := must.NotFail(rp.Get("resource")).(*types.Document)
		removeByKey[resourceKey(res)] = must.NotFail(rp.Get("actions")).(*types.Array)
	}

	out := types.MakeArray(0)
	for i := 0; i < existing.Len(); i++ {
		priv := must.NotFail(existing.Get(i)).(*types.Document)
		res := must.NotFail(priv.Get("resource")).(*types.Document)

		toRemove, ok := removeByKey[resourceKey(res)]
		if !ok {
			out.Append(priv)
			continue
		}

		remaining := subtractStrings(must.NotFail(priv.Get("actions")).(*types.Array), toRemove)
		if remaining.Len() == 0 {
			continue
		}

		priv.Set("actions", remaining)
		out.Append(priv)
	}

	return out
}

func unionRoles(existing, add *types.Array) *types.Array {
	out := copyArray(existing)

	for i := 0; i < add.Len(); i++ {
		ref := must.NotFail(add.Get(i)).(*types.Document)
		if !containsRole(out, ref) {
			out.Append(ref)
		}
	}

	return out
}

func removeRoles(existing, remove *types.Array) *types.Array {
	if existing == nil {
		return types.MakeArray(0)
	}

	out := types.MakeArray(0)
	for i := 0; i < existing.Len(); i++ {
		ref := must.NotFail(existing.Get(i)).(*types.Document)
		if !containsRole(remove, ref) {
			out.Append(ref)
		}
	}

	return out
}

func containsRole(arr *types.Array, ref *types.Document) bool {
	role := must.NotFail(ref.Get("role"))
	db := must.NotFail(ref.Get("db"))
	for i := 0; i < arr.Len(); i++ {
		r := must.NotFail(arr.Get(i)).(*types.Document)
		if must.NotFail(r.Get("role")) == role && must.NotFail(r.Get("db")) == db {
			return true
		}
	}
	return false
}

func copyArray(arr *types.Array) *types.Array {
	out := types.MakeArray(0)
	if arr == nil {
		return out
	}
	for i := 0; i < arr.Len(); i++ {
		out.Append(must.NotFail(arr.Get(i)))
	}
	return out
}

func unionStrings(existing, add *types.Array) *types.Array {
	out := copyArray(existing)
	for i := 0; i < add.Len(); i++ {
		v := must.NotFail(add.Get(i))
		found := false
		for j := 0; j < out.Len(); j++ {
			if must.NotFail(out.Get(j)) == v {
				found = true
				break
			}
		}
		if !found {
			out.Append(v)
		}
	}
	return out
}

func subtractStrings(from, remove *types.Array) *types.Array {
	out := types.MakeArray(0)
	for i := 0; i < from.Len(); i++ {
		v := must.NotFail(from.Get(i))
		drop := false
		for j := 0; j < remove.Len(); j++ {
			if must.NotFail(remove.Get(j)) == v {
				drop = true
				break
			}
		}
		if !drop {
			out.Append(v)
		}
	}
	return out
}
