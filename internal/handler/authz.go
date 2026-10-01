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
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/users"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func privilegesToArray(ps authz.PrivilegeSet) *types.Array {
	arr := types.MakeArray(len(ps))
	for _, p := range ps {
		actions := types.MakeArray(len(p.Actions))
		for _, a := range p.Actions {
			actions.Append(string(a))
		}
		arr.Append(must.NotFail(types.NewDocument(
			"resource", resourceDoc(p.Resource),
			"actions", actions,
		)))
	}
	return arr
}

func resourceDoc(r authz.Resource) *types.Document {
	switch {
	case r.AnyResource:
		return must.NotFail(types.NewDocument("anyResource", true))
	case r.Cluster:
		return must.NotFail(types.NewDocument("cluster", true))
	default:
		return must.NotFail(types.NewDocument("db", r.DB, "collection", r.Collection))
	}
}

type resourceScope int

const (
	scopeCollection resourceScope = iota
	scopeDatabase
	scopeCluster
)

type commandPrivilege struct {
	action authz.Action
	scope  resourceScope
}

var commandPrivileges = map[string][]commandPrivilege{
	"find":             {{authz.ActionFind, scopeCollection}},
	"count":            {{authz.ActionFind, scopeCollection}},
	"distinct":         {{authz.ActionFind, scopeCollection}},
	"aggregate":        {{authz.ActionFind, scopeCollection}},
	"collStats":        {{authz.ActionCollStats, scopeCollection}},
	"listIndexes":      {{authz.ActionListIndexes, scopeCollection}},
	"dataSize":         {{authz.ActionFind, scopeCollection}},
	"insert":           {{authz.ActionInsert, scopeCollection}},
	"update":           {{authz.ActionUpdate, scopeCollection}},
	"delete":           {{authz.ActionRemove, scopeCollection}},
	"findAndModify":    {{authz.ActionFind, scopeCollection}, {authz.ActionUpdate, scopeCollection}},
	"create":           {{authz.ActionCreateCollection, scopeCollection}},
	"createIndexes":    {{authz.ActionCreateIndex, scopeCollection}},
	"drop":             {{authz.ActionDropCollection, scopeCollection}},
	"dropIndexes":      {{authz.ActionDropIndex, scopeCollection}},
	"collMod":          {{authz.ActionCollMod, scopeCollection}},
	"validate":         {{authz.ActionValidate, scopeCollection}},
	"convertToCapped":  {{authz.ActionConvertToCapped, scopeCollection}},
	"renameCollection": {{authz.ActionRenameCollectionSameDB, scopeCollection}},

	"dbStats":                  {{authz.ActionDBStats, scopeDatabase}},
	"listCollections":          {{authz.ActionListCollections, scopeDatabase}},
	"dropDatabase":             {{authz.ActionDropDatabase, scopeDatabase}},
	"createUser":               {{authz.ActionCreateUser, scopeDatabase}},
	"dropUser":                 {{authz.ActionDropUser, scopeDatabase}},
	"dropAllUsersFromDatabase": {{authz.ActionDropUser, scopeDatabase}},
	"usersInfo":                {{authz.ActionViewUser, scopeDatabase}},
	"createRole":               {{authz.ActionCreateRole, scopeDatabase}},
	"updateRole":               {{authz.ActionGrantRole, scopeDatabase}},
	"dropRole":                 {{authz.ActionDropRole, scopeDatabase}},
	"dropAllRolesFromDatabase": {{authz.ActionDropRole, scopeDatabase}},
	"rolesInfo":                {{authz.ActionViewRole, scopeDatabase}},
	"grantRolesToUser":         {{authz.ActionGrantRole, scopeDatabase}},
	"revokeRolesFromUser":      {{authz.ActionRevokeRole, scopeDatabase}},
	"grantPrivilegesToRole":    {{authz.ActionGrantRole, scopeDatabase}},
	"revokePrivilegesFromRole": {{authz.ActionRevokeRole, scopeDatabase}},
	"grantRolesToRole":         {{authz.ActionGrantRole, scopeDatabase}},
	"revokeRolesFromRole":      {{authz.ActionRevokeRole, scopeDatabase}},

	"serverStatus":  {{authz.ActionServerStatus, scopeCluster}},
	"listDatabases": {{authz.ActionListDatabases, scopeCluster}},
	"getParameter":  {{authz.ActionGetParameter, scopeCluster}},
	"setParameter":  {{authz.ActionSetParameter, scopeCluster}},
	"hostInfo":      {{authz.ActionHostInfo, scopeCluster}},
	"top":           {{authz.ActionTop, scopeCluster}},
	"getLog":        {{authz.ActionGetLog, scopeCluster}},
}

func (h *Handler) authorize(ctx context.Context, msg *wire.OpMsg) error {
	command, db, collection := wireCommandTarget(msg)

	if command == "updateUser" {
		return h.authorizeUpdateUser(ctx, msg, db, collection)
	}

	reqs, ok := commandPrivileges[command]
	if !ok {
		return nil
	}

	privs, err := h.effectivePrivileges(ctx)
	if err != nil {
		return err
	}

	for _, r := range reqs {
		target := targetResource(r.scope, db, collection)
		if privs.Authorized(r.action, target) {
			continue
		}
		if h.selfServiceAllowed(ctx, command, db, collection) {
			continue
		}
		return unauthorizedCommandError(msg, command, db)
	}
	return nil
}

// authorizeUpdateUser requires a privilege for each field the command changes,
// matching MongoDB 8.0: roles need revokeRole on any normal resource (the
// roles being replaced are unknown) plus grantRole on each granted role's db.
// commitIdentity is DumboDB-only and has no self-service action.
func (h *Handler) authorizeUpdateUser(ctx context.Context, msg *wire.OpMsg, db, targetUser string) error {
	document, err := opMsgDocument(msg)
	if err != nil {
		return err
	}

	privs, err := h.effectivePrivileges(ctx)
	if err != nil {
		return err
	}

	user, _, _, userDB := conninfo.Get(ctx).Auth()
	isSelf := targetUser != "" && targetUser == user && db == userDB
	dbResource := authz.DatabaseResource(db)
	allowedWithSelfService := func(action, ownAction authz.Action) bool {
		return privs.Authorized(action, dbResource) || (isSelf && privs.Authorized(ownAction, dbResource))
	}

	authorized := true
	if document.Has("pwd") || document.Has("mechanisms") {
		authorized = authorized && allowedWithSelfService(authz.ActionChangePassword, authz.ActionChangeOwnPassword)
	}
	if document.Has("customData") {
		authorized = authorized && allowedWithSelfService(authz.ActionChangeCustomData, authz.ActionChangeOwnCustomData)
	}
	if document.Has("commitIdentity") {
		authorized = authorized && privs.Authorized(authz.ActionChangeCustomData, dbResource)
	}
	if document.Has("authenticationRestrictions") {
		authorized = authorized && privs.Authorized(authz.ActionSetAuthenticationRestriction, dbResource)
	}
	if authorized && document.Has("roles") {
		authorized, err = canReplaceUserRoles(document, db, privs)
		if err != nil {
			return err
		}
	}

	if !authorized {
		return unauthorizedCommandError(msg, "updateUser", db)
	}
	return nil
}

func canReplaceUserRoles(document *types.Document, db string, privs authz.PrivilegeSet) (bool, error) {
	if !privs.AuthorizedOnAnyNormalResource(authz.ActionRevokeRole) {
		return false, nil
	}

	roles, err := common.GetRequiredParam[*types.Array](document, "roles")
	if err != nil {
		return false, err
	}

	normalized, err := users.NormalizeRoles(roles, db)
	if err != nil {
		return false, err
	}

	for i := 0; i < normalized.Len(); i++ {
		role := must.NotFail(normalized.Get(i)).(*types.Document)
		roleDB, _ := role.Get("db")
		roleDBName, _ := roleDB.(string)
		if !privs.Authorized(authz.ActionGrantRole, authz.DatabaseResource(roleDBName)) {
			return false, nil
		}
	}
	return true, nil
}

func unauthorizedCommandError(msg *wire.OpMsg, command, db string) error {
	commandEcho := command
	if doc, err := opMsgDocument(msg); err == nil {
		commandEcho = mongoCommandString(doc)
	}
	return handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrUnauthorized,
		fmt.Sprintf("not authorized on %s to execute command %s", db, commandEcho),
		command,
	)
}

func targetResource(scope resourceScope, db, collection string) authz.Resource {
	switch scope {
	case scopeCluster:
		return authz.ClusterResource
	case scopeDatabase:
		return authz.DatabaseResource(db)
	default:
		return authz.CollectionResource(db, collection)
	}
}

func (h *Handler) selfServiceAllowed(ctx context.Context, command, db, targetUser string) bool {
	user, _, _, userDB := conninfo.Get(ctx).Auth()
	if targetUser == "" || targetUser != user || db != userDB {
		return false
	}
	return command == "usersInfo"
}

func (h *Handler) effectivePrivileges(ctx context.Context) (authz.PrivilegeSet, error) {
	ci := conninfo.Get(ctx)
	user, _, _, userDB := ci.Auth()
	if user == "" {
		return nil, nil
	}

	gen := h.authGen.Load()
	if privs, cachedGen, ok := ci.PrivilegeCache(); ok && cachedGen == gen {
		return privs, nil
	}

	roles, err := h.loadUserRoles(ctx, userDB, user)
	if err != nil {
		return nil, err
	}

	privs := authz.Resolve(roles, h.roleResolver(ctx))
	ci.SetPrivilegeCache(gen, privs)
	return privs, nil
}

func (h *Handler) roleResolver(ctx context.Context) authz.RoleResolver {
	return h.customRoleResolver(ctx)
}

func (h *Handler) loadUserRoles(ctx context.Context, db, user string) ([]authz.Role, error) {
	rolesArr, err := h.authenticatedUserRoles(ctx, db, user)
	if err != nil {
		return nil, err
	}
	return rolesFromArray(rolesArr), nil
}

func rolesFromArray(arr *types.Array) []authz.Role {
	if arr == nil {
		return nil
	}

	out := make([]authz.Role, 0, arr.Len())
	for i := 0; i < arr.Len(); i++ {
		v, err := arr.Get(i)
		if err != nil {
			continue
		}
		doc, ok := v.(*types.Document)
		if !ok {
			continue
		}
		roleVal, _ := doc.Get("role")
		dbVal, _ := doc.Get("db")
		roleStr, _ := roleVal.(string)
		dbStr, _ := dbVal.(string)
		if roleStr != "" {
			out = append(out, authz.Role{Role: roleStr, DB: dbStr})
		}
	}
	return out
}
