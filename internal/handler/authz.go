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

	"serverStatus":       {{authz.ActionServerStatus, scopeCluster}},
	"listDatabases":      {{authz.ActionListDatabases, scopeCluster}},
	"getParameter":       {{authz.ActionGetParameter, scopeCluster}},
	"setParameter":       {{authz.ActionSetParameter, scopeCluster}},
	"hostInfo":           {{authz.ActionHostInfo, scopeCluster}},
	"top":                {{authz.ActionTop, scopeCluster}},
	"getLog":             {{authz.ActionGetLog, scopeCluster}},
	"rotateCertificates": {{authz.ActionRotateCertificates, scopeCluster}},
}

func (h *Handler) authorize(ctx context.Context, msg *wire.OpMsg) error {
	if h.internalMemberAuthenticated(ctx) {
		return nil
	}
	command, db, collection := wireCommandTarget(msg)

	if command == "updateUser" {
		return h.authorizeUpdateUser(ctx, msg, db, collection)
	}
	if checks, ok := grantCommandChecks[command]; ok {
		return h.authorizeGrantCommand(ctx, msg, command, db, checks)
	}
	if command == "aggregate" {
		if pipeline, stage := listSessionsPipeline(msg); stage != nil {
			return h.authorizeListSessions(ctx, msg, db, collection, pipeline, stage)
		}
		if err := h.authorizeByCommandPrivileges(ctx, msg, command, db, collection); err != nil {
			return err
		}
		return h.authorizeAggregationStages(ctx, msg, db)
	}

	return h.authorizeByCommandPrivileges(ctx, msg, command, db, collection)
}

func (h *Handler) authorizeByCommandPrivileges(ctx context.Context, msg *wire.OpMsg, command, db, collection string) error {
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

// listSessionsPipeline returns the pipeline and its first stage when the
// pipeline starts with $listLocalSessions or $listSessions.
func listSessionsPipeline(msg *wire.OpMsg) (*types.Array, *types.Document) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, nil
	}
	pipeline, _ := common.GetOptionalParam[*types.Array](document, "pipeline", nil)
	if pipeline == nil || pipeline.Len() == 0 {
		return nil, nil
	}
	stage, _ := must.NotFail(pipeline.Get(0)).(*types.Document)
	if stage == nil || stage.Len() == 0 {
		return nil, nil
	}
	switch stage.Command() {
	case "$listLocalSessions", "$listSessions":
		return pipeline, stage
	}
	return nil, nil
}

// readOnlyFollowUpStages may follow a session listing without further
// privileges; any other stage also needs the ordinary aggregate privileges.
var readOnlyFollowUpStages = map[string]struct{}{
	"$match": {}, "$project": {}, "$addFields": {}, "$set": {}, "$unset": {},
	"$sort": {}, "$limit": {}, "$skip": {}, "$count": {}, "$group": {},
	"$sortByCount": {}, "$replaceRoot": {}, "$replaceWith": {}, "$unwind": {},
}

// authorizeListSessions follows MongoDB 8.0: listing one's own sessions needs
// no privilege; allUsers or another user's sessions needs listSessions on the
// cluster.
func (h *Handler) authorizeListSessions(ctx context.Context, msg *wire.OpMsg, db, collection string, pipeline *types.Array, stage *types.Document) error {
	caller := conninfo.Get(ctx).SessionPrincipal()
	spec, err := parseListSessionsSpec(stage, caller)
	if err != nil {
		return err
	}

	if spec.requiresListSessionsPrivilege(caller) {
		privs, err := h.effectivePrivileges(ctx)
		if err != nil {
			return err
		}
		if !privs.Authorized(authz.ActionListSessions, authz.ClusterResource) {
			return unauthorizedCommandError(msg, "aggregate", db)
		}
	}

	for i := 1; i < pipeline.Len(); i++ {
		next, _ := must.NotFail(pipeline.Get(i)).(*types.Document)
		if next == nil || next.Len() == 0 {
			continue
		}
		if _, ok := readOnlyFollowUpStages[next.Command()]; !ok {
			if err := h.authorizeByCommandPrivileges(ctx, msg, "aggregate", db, collection); err != nil {
				return err
			}
			return h.authorizeAggregationStages(ctx, msg, db)
		}
	}
	return nil
}

// stagePrivilege is one privilege an aggregation stage requires.
type stagePrivilege struct {
	resource authz.Resource
	action   authz.Action
}

// authorizeAggregationStages checks the privileges an aggregation's stages
// require beyond reading its source collection.
func (h *Handler) authorizeAggregationStages(ctx context.Context, msg *wire.OpMsg, db string) error {
	document, err := opMsgDocument(msg)
	if err != nil {
		return err
	}
	pipeline, _ := common.GetOptionalParam[*types.Array](document, "pipeline", nil)
	bypass, _ := document.Get("bypassDocumentValidation")
	required := pipelineStagePrivileges(pipeline, db, bypass == true)
	if len(required) == 0 {
		return nil
	}

	privs, err := h.effectivePrivileges(ctx)
	if err != nil {
		return err
	}
	for _, r := range required {
		if !privs.Authorized(r.action, r.resource) {
			return unauthorizedCommandError(msg, "aggregate", db)
		}
	}
	return nil
}

// pipelineStagePrivileges follows MongoDB 8.0: $out needs insert and remove on
// its target; $merge needs insert when unmatched documents are inserted and
// update unless a match fails; $lookup, $graphLookup, and $unionWith need find
// on the foreign collection unless their sub-pipeline supplies its own source
// ($documents). Sub-pipelines, including $facet's, add their own stages'
// requirements.
func pipelineStagePrivileges(pipeline *types.Array, db string, bypassValidation bool) []stagePrivilege {
	if pipeline == nil {
		return nil
	}

	var required []stagePrivilege
	requireOn := func(targetDB, coll string, actions ...authz.Action) {
		if bypassValidation {
			actions = append(actions, authz.ActionBypassDocumentValidation)
		}
		for _, a := range actions {
			required = append(required, stagePrivilege{authz.CollectionResource(targetDB, coll), a})
		}
	}
	readFrom := func(coll string, sub *types.Array) {
		if !startsWithOwnSource(sub) {
			required = append(required, stagePrivilege{authz.CollectionResource(db, coll), authz.ActionFind})
		}
		required = append(required, pipelineStagePrivileges(sub, db, bypassValidation)...)
	}

	for i := 0; i < pipeline.Len(); i++ {
		stage, _ := must.NotFail(pipeline.Get(i)).(*types.Document)
		if stage == nil || stage.Len() == 0 {
			continue
		}
		spec := must.NotFail(stage.Get(stage.Command()))

		switch stage.Command() {
		case "$out":
			targetDB, coll := stageTarget(spec, "db", "coll", db)
			requireOn(targetDB, coll, authz.ActionInsert, authz.ActionRemove)
		case "$merge":
			specDoc, _ := spec.(*types.Document)
			into := spec
			whenMatched, whenNotMatched := any("merge"), any("insert")
			if specDoc != nil {
				into, _ = specDoc.Get("into")
				if v, err := specDoc.Get("whenMatched"); err == nil {
					whenMatched = v
				}
				if v, err := specDoc.Get("whenNotMatched"); err == nil {
					whenNotMatched = v
				}
			}
			targetDB, coll := stageTarget(into, "db", "coll", db)
			var actions []authz.Action
			if whenNotMatched == "insert" {
				actions = append(actions, authz.ActionInsert)
			}
			if whenMatched != "fail" {
				actions = append(actions, authz.ActionUpdate)
			}
			requireOn(targetDB, coll, actions...)
		case "$lookup", "$graphLookup":
			specDoc, _ := spec.(*types.Document)
			if specDoc == nil {
				continue
			}
			from, _ := specDoc.Get("from")
			fromName, _ := from.(string)
			sub, _ := specDoc.Get("pipeline")
			subArr, _ := sub.(*types.Array)
			readFrom(fromName, subArr)
		case "$unionWith":
			_, coll := stageTarget(spec, "", "coll", db)
			var subArr *types.Array
			if specDoc, ok := spec.(*types.Document); ok {
				sub, _ := specDoc.Get("pipeline")
				subArr, _ = sub.(*types.Array)
			}
			readFrom(coll, subArr)
		case "$facet":
			facets, _ := spec.(*types.Document)
			if facets == nil {
				continue
			}
			for _, key := range facets.Keys() {
				sub, _ := must.NotFail(facets.Get(key)).(*types.Array)
				required = append(required, pipelineStagePrivileges(sub, db, bypassValidation)...)
			}
		}
	}
	return required
}

// stageTarget reads a stage target given as a collection name or as a document
// with collection (and, when dbField is set, database) fields.
func stageTarget(spec any, dbField, collField, defaultDB string) (string, string) {
	switch v := spec.(type) {
	case string:
		return defaultDB, v
	case *types.Document:
		targetDB := defaultDB
		if dbField != "" {
			if d, _ := v.Get(dbField); d != nil {
				if name, ok := d.(string); ok {
					targetDB = name
				}
			}
		}
		coll, _ := v.Get(collField)
		collName, _ := coll.(string)
		return targetDB, collName
	}
	return defaultDB, ""
}

func startsWithOwnSource(pipeline *types.Array) bool {
	if pipeline == nil || pipeline.Len() == 0 {
		return false
	}
	first, _ := must.NotFail(pipeline.Get(0)).(*types.Document)
	return first != nil && first.Len() > 0 && first.Command() == "$documents"
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
