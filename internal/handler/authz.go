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
	"dropUser":                 {{authz.ActionDropUser, scopeDatabase}},
	"dropAllUsersFromDatabase": {{authz.ActionDropUser, scopeDatabase}},
	"usersInfo":                {{authz.ActionViewUser, scopeDatabase}},
	"dropRole":                 {{authz.ActionDropRole, scopeDatabase}},
	"dropAllRolesFromDatabase": {{authz.ActionDropRole, scopeDatabase}},
	"rolesInfo":                {{authz.ActionViewRole, scopeDatabase}},

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
	if checks, ok := grantCommandChecks[command]; ok {
		return h.authorizeGrantCommand(ctx, msg, command, db, checks)
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
		authorized, err = allGrantChecksPass(privs, document, db, []grantCheck{
			actionOnAnyNormalResource(authz.ActionRevokeRole),
			actionOnEachRoleDB(authz.ActionGrantRole),
		})
		if err != nil {
			return err
		}
	}

	if !authorized {
		return unauthorizedCommandError(msg, "updateUser", db)
	}
	return nil
}

// grantCheck reports whether privs allow one part of a user- or
// role-management command run on db.
type grantCheck func(privs authz.PrivilegeSet, document *types.Document, db string) (bool, error)

// grantCommandChecks follows MongoDB 8.0 (user_management_commands_common.cpp):
// a granted or revoked role or privilege is authorized against its own
// database, not the database the command runs on.
var grantCommandChecks = map[string][]grantCheck{
	"createUser": {
		actionOnCommandDB(authz.ActionCreateUser),
		actionOnEachRoleDB(authz.ActionGrantRole),
		authenticationRestrictionsAllowed,
	},
	"createRole": {
		actionOnCommandDB(authz.ActionCreateRole),
		actionOnEachRoleDB(authz.ActionGrantRole),
		actionOnEachPrivilegeDB(authz.ActionGrantRole),
		authenticationRestrictionsAllowed,
	},
	"updateRole": {
		actionOnAnyNormalResource(authz.ActionRevokeRole),
		actionOnEachRoleDB(authz.ActionGrantRole),
		actionOnEachPrivilegeDB(authz.ActionGrantRole),
		authenticationRestrictionsAllowed,
	},
	"grantRolesToUser":         {actionOnEachRoleDB(authz.ActionGrantRole)},
	"grantRolesToRole":         {actionOnEachRoleDB(authz.ActionGrantRole)},
	"revokeRolesFromUser":      {actionOnEachRoleDB(authz.ActionRevokeRole)},
	"revokeRolesFromRole":      {actionOnEachRoleDB(authz.ActionRevokeRole)},
	"grantPrivilegesToRole":    {actionOnEachPrivilegeDB(authz.ActionGrantRole)},
	"revokePrivilegesFromRole": {actionOnEachPrivilegeDB(authz.ActionRevokeRole)},
}

func (h *Handler) authorizeGrantCommand(ctx context.Context, msg *wire.OpMsg, command, db string, checks []grantCheck) error {
	document, err := opMsgDocument(msg)
	if err != nil {
		return err
	}

	privs, err := h.effectivePrivileges(ctx)
	if err != nil {
		return err
	}

	authorized, err := allGrantChecksPass(privs, document, db, checks)
	if err != nil {
		return err
	}
	if !authorized {
		return unauthorizedCommandError(msg, command, db)
	}
	return nil
}

func allGrantChecksPass(privs authz.PrivilegeSet, document *types.Document, db string, checks []grantCheck) (bool, error) {
	for _, check := range checks {
		authorized, err := check(privs, document, db)
		if err != nil || !authorized {
			return false, err
		}
	}
	return true, nil
}

func actionOnCommandDB(action authz.Action) grantCheck {
	return func(privs authz.PrivilegeSet, _ *types.Document, db string) (bool, error) {
		return privs.Authorized(action, authz.DatabaseResource(db)), nil
	}
}

func actionOnAnyNormalResource(action authz.Action) grantCheck {
	return func(privs authz.PrivilegeSet, _ *types.Document, _ string) (bool, error) {
		return privs.AuthorizedOnAnyNormalResource(action), nil
	}
}

// actionOnEachRoleDB checks action on the database of every role named in the
// command's roles field; bare role names resolve to db.
func actionOnEachRoleDB(action authz.Action) grantCheck {
	return func(privs authz.PrivilegeSet, document *types.Document, db string) (bool, error) {
		roles, err := common.GetOptionalParam[*types.Array](document, "roles", nil)
		if err != nil {
			return false, err
		}

		normalized, err := normalizeRoleRefs(roles, db)
		if err != nil {
			return false, err
		}

		for i := 0; i < normalized.Len(); i++ {
			role := must.NotFail(normalized.Get(i)).(*types.Document)
			roleDB := must.NotFail(role.Get("db")).(string)
			if !privs.Authorized(action, authz.DatabaseResource(roleDB)) {
				return false, nil
			}
		}
		return true, nil
	}
}

// actionOnEachPrivilegeDB checks action on the database of every privilege's
// resource. Cluster, any-resource, and any-database resources are checked
// against admin.
func actionOnEachPrivilegeDB(action authz.Action) grantCheck {
	return func(privs authz.PrivilegeSet, document *types.Document, _ string) (bool, error) {
		privileges, err := common.GetOptionalParam[*types.Array](document, "privileges", nil)
		if err != nil {
			return false, err
		}
		if privileges == nil {
			return true, nil
		}

		for i := 0; i < privileges.Len(); i++ {
			privilege, ok := must.NotFail(privileges.Get(i)).(*types.Document)
			if !ok {
				return false, handlererrors.NewCommandErrorMsg(handlererrors.ErrBadValue, "privilege entry must be a document")
			}

			resourceValue, _ := privilege.Get("resource")
			resourceDoc, _ := resourceValue.(*types.Document)
			resource := parseResourceDoc(resourceDoc)
			authDB := "admin"
			if !resource.Cluster && !resource.AnyResource && resource.DB != "" {
				authDB = resource.DB
			}
			if !privs.Authorized(action, authz.DatabaseResource(authDB)) {
				return false, nil
			}
		}
		return true, nil
	}
}

func authenticationRestrictionsAllowed(privs authz.PrivilegeSet, document *types.Document, db string) (bool, error) {
	if !document.Has("authenticationRestrictions") {
		return true, nil
	}
	return privs.Authorized(authz.ActionSetAuthenticationRestriction, authz.DatabaseResource(db)), nil
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
