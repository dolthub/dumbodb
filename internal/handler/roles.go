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

	"github.com/dolthub/dumbodb/internal/authz"
	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) systemRolesCollection() (backends.Collection, error) {
	adminDB, err := h.b.Database("admin")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	coll, err := adminDB.Collection("system.roles")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return coll, nil
}

// loadRoleDoc returns the stored role document for {role, db}, or nil when no
// such user-defined role exists.
func (h *Handler) loadRoleDoc(ctx context.Context, db, role string) (*types.Document, error) {
	coll, err := h.systemRolesCollection()
	if err != nil {
		return nil, err
	}

	id := db + "." + role
	filter := must.NotFail(types.NewDocument("_id", id))

	qr, err := coll.Query(ctx, &backends.QueryParams{Filter: filter})
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

// roleExists reports whether {role, db} names a built-in or a stored role.
func (h *Handler) roleExists(ctx context.Context, db, role string) (bool, error) {
	if db == "admin" && authz.IsBuiltinRole(role) {
		return true, nil
	}

	doc, err := h.loadRoleDoc(ctx, db, role)
	if err != nil {
		return false, err
	}

	return doc != nil, nil
}

// normalizeInheritedRoles validates that every inherited role exists and returns
// the {role, db} array to store. Built-in roles are accepted without lookup. A
// missing inherited role is reported as RoleNotFound against the grantee role
// {granteeRole, granteeDB}.
func (h *Handler) normalizeInheritedRoles(ctx context.Context, roles *types.Array, granteeRole, granteeDB string) (*types.Array, error) {
	refs, err := normalizeRoleRefs(roles, granteeDB)
	if err != nil {
		return nil, err
	}

	for i := 0; i < refs.Len(); i++ {
		ref := must.NotFail(refs.Get(i)).(*types.Document)
		role := must.NotFail(ref.Get("role")).(string)
		db := must.NotFail(ref.Get("db")).(string)

		exists, err := h.roleExists(ctx, db, role)
		if err != nil {
			return nil, err
		}

		if !exists {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrRoleNotFound,
				fmt.Sprintf("Cannot grant roles to '%s@%s': Could not find role: %s@%s", granteeRole, granteeDB, role, db),
			)
		}
	}

	return refs, nil
}

// normalizeRoleRefs converts a roles array into {role, db} documents, resolving a
// shorthand string entry's db to defaultDB.
func normalizeRoleRefs(roles *types.Array, defaultDB string) (*types.Array, error) {
	out := types.MakeArray(0)
	if roles == nil {
		return out, nil
	}

	for i := 0; i < roles.Len(); i++ {
		v := must.NotFail(roles.Get(i))

		switch r := v.(type) {
		case string:
			out.Append(must.NotFail(types.NewDocument("role", r, "db", defaultDB)))
		case *types.Document:
			role, _ := r.Get("role")
			db, _ := r.Get("db")

			roleStr, ok := role.(string)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "role name must be a string")
			}

			dbStr, ok := db.(string)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "role db must be a string")
			}

			out.Append(must.NotFail(types.NewDocument("role", roleStr, "db", dbStr)))
		default:
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "role entry must be a string or a document")
		}
	}

	return out, nil
}

// normalizePrivileges validates and rebuilds a privileges array as {resource,
// actions} documents. A role outside the admin database may only grant on
// collection resources within its own database; cluster, anyResource, and
// cross-database resources are BadValue.
func normalizePrivileges(privileges *types.Array, roleDB string) (*types.Array, error) {
	out := types.MakeArray(0)
	if privileges == nil {
		return out, nil
	}

	for i := 0; i < privileges.Len(); i++ {
		v, ok := must.NotFail(privileges.Get(i)).(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "privilege must be a document")
		}

		resource, ok := must.NotFail(v.Get("resource")).(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "privilege resource must be a document")
		}

		if err := validateResourceScope(resource, roleDB); err != nil {
			return nil, err
		}

		actionsVal, err := v.Get("actions")
		if err != nil {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "privilege must specify actions")
		}

		actions, ok := actionsVal.(*types.Array)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "privilege actions must be an array")
		}

		storedActions := types.MakeArray(actions.Len())
		for j := 0; j < actions.Len(); j++ {
			a, ok := must.NotFail(actions.Get(j)).(string)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "action must be a string")
			}
			storedActions.Append(a)
		}

		out.Append(must.NotFail(types.NewDocument("resource", resource, "actions", storedActions)))
	}

	return out, nil
}

// validateResourceScope enforces the design's resource-scoping rule: a non-admin
// role may only name a collection resource within its own database.
func validateResourceScope(resource *types.Document, roleDB string) error {
	if roleDB == "admin" {
		return nil
	}

	offDatabase := false
	if cluster, _ := resource.Get("cluster"); cluster == true {
		offDatabase = true
	}
	if any, _ := resource.Get("anyResource"); any == true {
		offDatabase = true
	}
	if db, _ := resource.Get("db"); !offDatabase {
		dbStr, _ := db.(string)
		offDatabase = dbStr != roleDB
	}

	if offDatabase {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrInvalidRoleModification,
			fmt.Sprintf("Roles on the '%s' database cannot be granted privileges that target other databases or the cluster", roleDB),
		)
	}

	return nil
}

// requiredArray returns document[key] as an array, translating a missing field
// into a MissingField error naming the command.
func requiredArray(document *types.Document, command, key string) (*types.Array, error) {
	v, err := common.GetOptionalParam[*types.Array](document, key, nil)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if v == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrMissingField,
			fmt.Sprintf("BSON field '%s.%s' is missing but a required field", command, key),
		)
	}

	return v, nil
}
