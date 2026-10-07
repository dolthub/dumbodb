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
	"strings"

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
	"compact":          {{authz.ActionCompact, scopeCollection}},

	"createSearchIndexes": {{authz.ActionCreateSearchIndexes, scopeCollection}},
	"dropSearchIndex":     {{authz.ActionDropSearchIndex, scopeCollection}},
	"updateSearchIndex":   {{authz.ActionUpdateSearchIndex, scopeCollection}},
	"listSearchIndexes":   {{authz.ActionListSearchIndexes, scopeCollection}},

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
	"autoCompact":        {{authz.ActionCompact, scopeCluster}},
	"currentOp":          {{authz.ActionInprog, scopeCluster}},
	"getCmdLineOpts":     {{authz.ActionGetCmdLineOpts, scopeCluster}},

	"dumboRemote": {{authz.ActionDumboRemote, scopeCluster}},
	"doltRemote":  {{authz.ActionDumboRemote, scopeCluster}},
	"dumboPush":   {{authz.ActionDumboRemote, scopeCluster}},
	"doltPush":    {{authz.ActionDumboRemote, scopeCluster}},
	"dumboFetch":  {{authz.ActionDumboRemote, scopeCluster}},
	"doltFetch":   {{authz.ActionDumboRemote, scopeCluster}},
	"dumboPull":   {{authz.ActionDumboRemote, scopeCluster}},
	"doltPull":    {{authz.ActionDumboRemote, scopeCluster}},
	"dumboClone":  {{authz.ActionDumboRemote, scopeCluster}},
	"doltClone":   {{authz.ActionDumboRemote, scopeCluster}},
}

func (h *Handler) authorize(ctx context.Context, msg *wire.OpMsg) error {
	if h.internalMemberAuthenticated(ctx) {
		return nil
	}
	command, db, collection := wireCommandTarget(msg)

	if command == "updateUser" {
		return h.authorizeUpdateUser(ctx, msg, db, collection)
	}
	if command == "usersInfo" {
		return h.authorizeUsersInfo(ctx, msg, db)
	}
	if command == "renameCollection" {
		return h.authorizeRenameCollection(ctx, msg, db, collection)
	}
	if command == "dataSize" {
		return h.authorizeNamespaceAction(ctx, msg, db, collection, authz.ActionFind)
	}
	if command == "explain" {
		return h.authorizeExplain(ctx, msg, db)
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

// authorizeExplain authorizes the wrapped command as if it were run directly:
// executionStats executes it, so explain must not reach anything the command
// itself could not. A malformed wrapper is left for the handler to reject.
func (h *Handler) authorizeExplain(ctx context.Context, msg *wire.OpMsg, db string) error {
	document, err := opMsgDocument(msg)
	if err != nil {
		return err
	}

	inner, ok := must.NotFail(document.Get("explain")).(*types.Document)
	if !ok || inner.Len() == 0 {
		return nil
	}

	if inner.Command() == "explain" {
		return unauthorizedCommandError(msg, "explain", db)
	}

	wrapped := inner.DeepCopy()
	wrapped.Set("$db", db)

	wrappedMsg, err := documentOpMsg(wrapped)
	if err != nil {
		return err
	}

	return h.authorize(ctx, wrappedMsg)
}

// authorizeRenameCollection authorizes the source and target namespaces named
// in the command rather than $db. Malformed namespaces are left for the
// handler to reject.
func (h *Handler) authorizeRenameCollection(ctx context.Context, msg *wire.OpMsg, db, from string) error {
	document, err := opMsgDocument(msg)
	if err != nil {
		return err
	}

	to, _ := common.GetRequiredParam[string](document, "to")
	fromDB, fromColl, fromOK := strings.Cut(from, ".")
	toDB, toColl, toOK := strings.Cut(to, ".")
	if !fromOK || !toOK {
		return nil
	}

	privs, err := h.effectivePrivileges(ctx)
	if err != nil {
		return err
	}

	required := []struct {
		action authz.Action
		target authz.Resource
	}{
		{authz.ActionRenameCollectionSameDB, authz.CollectionResource(fromDB, fromColl)},
		{authz.ActionRenameCollectionSameDB, authz.CollectionResource(toDB, toColl)},
	}
	if dropTarget, _ := common.GetOptionalParam(document, "dropTarget", false); dropTarget {
		required = append(required, struct {
			action authz.Action
			target authz.Resource
		}{authz.ActionDropCollection, authz.CollectionResource(toDB, toColl)})
	}

	for _, r := range required {
		if !privs.Authorized(r.action, r.target) {
			return unauthorizedCommandError(msg, "renameCollection", db)
		}
	}

	return nil
}

// authorizeNamespaceAction authorizes action on the "db.collection"
// namespace given as the command's value rather than on $db.
func (h *Handler) authorizeNamespaceAction(ctx context.Context, msg *wire.OpMsg, db, ns string, action authz.Action) error {
	nsDB, nsColl, ok := strings.Cut(ns, ".")
	if !ok {
		return nil
	}

	privs, err := h.effectivePrivileges(ctx)
	if err != nil {
		return err
	}

	if !privs.Authorized(action, authz.CollectionResource(nsDB, nsColl)) {
		return unauthorizedCommandError(msg, wireCommandName(msg), db)
	}

	return nil
}

// authorizeUsersInfo requires viewUser on the database of every user the
// command reads, not just on $db: forAllDBs needs it on every database, and
// a {user, db} reference names a database of its own. Viewing oneself is
// always allowed. Malformed arguments are left for the handler to reject.
func (h *Handler) authorizeUsersInfo(ctx context.Context, msg *wire.OpMsg, db string) error {
	document, err := opMsgDocument(msg)
	if err != nil {
		return err
	}

	privs, err := h.effectivePrivileges(ctx)
	if err != nil {
		return err
	}

	canView := func(target authz.Resource) bool {
		return privs.Authorized(authz.ActionViewUser, target)
	}

	var targets []usersInfoPair

	switch arg := must.NotFail(document.Get("usersInfo")).(type) {
	case *types.Document:
		if arg.Has("forAllDBs") {
			if canView(authz.Resource{}) {
				return nil
			}
			return unauthorizedCommandError(msg, "usersInfo", db)
		}

		var p usersInfoPair
		if p.extract(arg, db) != nil {
			return nil
		}
		targets = append(targets, p)
	case *types.Array:
		for i := range arg.Len() {
			v := must.NotFail(arg.Get(i))
			if v == nil {
				continue
			}

			var p usersInfoPair
			if p.extract(v, db) != nil {
				return nil
			}
			targets = append(targets, p)
		}
	case string:
		targets = append(targets, usersInfoPair{username: arg, db: db})
	default:
		if canView(authz.DatabaseResource(db)) {
			return nil
		}
		return unauthorizedCommandError(msg, "usersInfo", db)
	}

	user, _, _, userDB := conninfo.Get(ctx).Auth()
	for _, p := range targets {
		if p.username == user && p.db == userDB {
			continue
		}
		if !canView(authz.DatabaseResource(p.db)) {
			return unauthorizedCommandError(msg, "usersInfo", db)
		}
	}

	return nil
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
