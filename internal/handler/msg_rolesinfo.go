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

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/authz"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) MsgRolesInfo(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	showPrivileges, err := common.GetOptionalParam(document, "showPrivileges", false)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	showBuiltin, err := common.GetOptionalParam(document, "showBuiltinRoles", false)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	common.Ignored(document, h.L, "comment")

	rolesInfo, err := document.Get(document.Command())
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	res := types.MakeArray(0)

	var requested []authz.Role
	allOnDB := false

	switch v := rolesInfo.(type) {
	case int32, int64, float64:
		allOnDB = true
	case string:
		requested = []authz.Role{{Role: v, DB: dbName}}
	case *types.Document:
		requested = append(requested, roleRefFromValue(v, dbName))
	case *types.Array:
		for i := 0; i < v.Len(); i++ {
			requested = append(requested, roleRefFromValue(must.NotFail(v.Get(i)), dbName))
		}
	}

	if allOnDB {
		if err = h.appendDatabaseRoles(connCtx, res, dbName, showPrivileges); err != nil {
			return nil, err
		}

		if showBuiltin {
			for _, name := range authz.BuiltinRoleNames(dbName) {
				res.Append(builtinRoleInfo(name, dbName, showPrivileges))
			}
		}
	} else {
		for _, r := range requested {
			if r.Role == "" {
				continue
			}

			if authz.IsBuiltinRole(r.Role) {
				res.Append(builtinRoleInfo(r.Role, r.DB, showPrivileges))
				continue
			}

			doc, err := h.loadRoleDoc(connCtx, r.DB, r.Role)
			if err != nil {
				return nil, err
			}

			if doc != nil {
				res.Append(h.userDefinedRoleInfo(connCtx, doc, showPrivileges))
			}
		}
	}

	return documentOpMsg(must.NotFail(types.NewDocument(
		"roles", res,
		"ok", float64(1),
	)))
}

func roleRefFromValue(v any, defaultDB string) authz.Role {
	switch r := v.(type) {
	case string:
		return authz.Role{Role: r, DB: defaultDB}
	case *types.Document:
		role, _ := r.Get("role")
		db, _ := r.Get("db")
		roleStr, _ := role.(string)
		dbStr, ok := db.(string)
		if !ok {
			dbStr = defaultDB
		}
		return authz.Role{Role: roleStr, DB: dbStr}
	default:
		return authz.Role{}
	}
}

func (h *Handler) appendDatabaseRoles(ctx context.Context, res *types.Array, dbName string, showPrivileges bool) error {
	coll, err := h.systemRolesCollection()
	if err != nil {
		return err
	}

	qr, err := coll.Query(ctx, nil)
	if err != nil {
		return lazyerrors.Error(err)
	}

	defer qr.Iter.Close()

	for {
		_, doc, err := qr.Iter.Next()

		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return lazyerrors.Error(err)
		}

		if db, _ := doc.Get("db"); db == dbName {
			res.Append(h.userDefinedRoleInfo(ctx, doc, showPrivileges))
		}
	}

	return nil
}

func (h *Handler) userDefinedRoleInfo(ctx context.Context, doc *types.Document, showPrivileges bool) *types.Document {
	role, _ := doc.Get("role")
	db, _ := doc.Get("db")
	roleStr, _ := role.(string)
	dbStr, _ := db.(string)

	storedRoles, _ := doc.Get("roles")
	rolesArr, _ := storedRoles.(*types.Array)
	if rolesArr == nil {
		rolesArr = types.MakeArray(0)
	}

	directRefs := rolesFromArray(rolesArr)

	out := must.NotFail(types.NewDocument(
		"_id", dbStr+"."+roleStr,
		"role", roleStr,
		"db", dbStr,
		"isBuiltin", false,
		"roles", rolesArr,
		"inheritedRoles", roleRefsToArray(h.roleRefClosure(ctx, directRefs)),
	))

	if showPrivileges {
		storedPrivs, _ := doc.Get("privileges")
		privArr, _ := storedPrivs.(*types.Array)
		if privArr == nil {
			privArr = types.MakeArray(0)
		}

		effective := append(authz.PrivilegeSet{}, parseStoredPrivileges(privArr)...)
		effective = append(effective, authz.Resolve(directRefs, h.customRoleResolver(ctx))...)

		out.Set("privileges", privArr)
		out.Set("inheritedPrivileges", privilegesToArray(effective))
	}

	return out
}

func builtinRoleInfo(role, db string, showPrivileges bool) *types.Document {
	out := must.NotFail(types.NewDocument(
		"role", role,
		"db", db,
		"isBuiltin", true,
		"roles", types.MakeArray(0),
		"inheritedRoles", types.MakeArray(0),
	))

	if showPrivileges {
		ps, _ := authz.BuiltinRole(role, db)
		out.Set("privileges", privilegesToArray(ps))
		out.Set("inheritedPrivileges", privilegesToArray(ps))
	}

	return out
}

func roleRefsToArray(roles []authz.Role) *types.Array {
	out := types.MakeArray(len(roles))
	for _, r := range roles {
		out.Append(must.NotFail(types.NewDocument("role", r.Role, "db", r.DB)))
	}
	return out
}
