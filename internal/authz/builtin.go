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

package authz

var (
	readActions = []Action{
		ActionFind, ActionCollStats, ActionDBStats, ActionDataSize,
		ActionKillCursors, ActionListCollections, ActionListIndexes, ActionListSearchIndexes,
	}
	writeActions = []Action{
		ActionInsert, ActionUpdate, ActionRemove, ActionBypassDocumentValidation,
		ActionCreateCollection, ActionCreateIndex, ActionCreateSearchIndexes,
		ActionDropCollection, ActionDropIndex, ActionDropSearchIndex,
		ActionRenameCollectionSameDB, ActionUpdateSearchIndex, ActionConvertToCapped,
	}
	dbAdminActions = []Action{
		ActionCollMod, ActionCompact, ActionCreateCollection, ActionCreateIndex,
		ActionDropCollection, ActionDropDatabase, ActionDropIndex, ActionDBStats,
		ActionCollStats, ActionDataSize, ActionValidate, ActionRenameCollectionSameDB,
		ActionListCollections, ActionListIndexes,
	}
	userAdminActions = []Action{
		ActionChangeCustomData, ActionChangePassword, ActionCreateRole, ActionCreateUser,
		ActionDropRole, ActionDropUser, ActionGrantRole, ActionRevokeRole,
		ActionSetAuthenticationRestriction, ActionViewRole, ActionViewUser,
	}
	clusterMonitorActions = []Action{
		ActionServerStatus, ActionGetParameter, ActionHostInfo,
		ActionListDatabases, ActionTop, ActionGetLog,
	}
	clusterManagerActions   = []Action{ActionCompact, ActionSetParameter}
	clusterMonitorDBActions = []Action{
		ActionDBStats, ActionCollStats, ActionListCollections, ActionListIndexes,
	}
)

// builtinRoles maps each built-in role name to nothing but its presence; used by
// IsBuiltinRole and rolesInfo.
var builtinRoles = map[string]struct{}{
	"read": {}, "readWrite": {}, "dbAdmin": {}, "userAdmin": {}, "dbOwner": {},
	"readAnyDatabase": {}, "readWriteAnyDatabase": {}, "dbAdminAnyDatabase": {}, "userAdminAnyDatabase": {},
	"clusterMonitor": {}, "clusterManager": {}, "hostManager": {}, "clusterAdmin": {},
	"backup": {}, "restore": {}, "root": {},
}

// IsBuiltinRole reports whether role names a MongoDB built-in role.
func IsBuiltinRole(role string) bool {
	_, ok := builtinRoles[role]
	return ok
}

// BuiltinRole synthesizes the privilege set for a built-in role granted on db
// (the role's {role, db}). The all-database, cluster, and backup/restore roles
// are admin-scoped; db is ignored for the resources they span. Returns false if
// role is not a built-in role name.
func BuiltinRole(role, db string) (PrivilegeSet, bool) {
	dbRes := DatabaseResource(db)
	allDB := Resource{}

	switch role {
	case "read":
		return PrivilegeSet{{dbRes, readActions}}, true
	case "readWrite":
		return PrivilegeSet{{dbRes, concat(readActions, writeActions)}}, true
	case "dbAdmin":
		return PrivilegeSet{{dbRes, dbAdminActions}}, true
	case "userAdmin":
		return PrivilegeSet{{dbRes, userAdminActions}}, true
	case "dbOwner":
		return PrivilegeSet{{dbRes, concat(readActions, writeActions, dbAdminActions, userAdminActions)}}, true
	case "readAnyDatabase":
		return PrivilegeSet{{allDB, readActions}, {ClusterResource, []Action{ActionListDatabases}}}, true
	case "readWriteAnyDatabase":
		return PrivilegeSet{{allDB, concat(readActions, writeActions)}, {ClusterResource, []Action{ActionListDatabases}}}, true
	case "dbAdminAnyDatabase":
		return PrivilegeSet{{allDB, dbAdminActions}, {ClusterResource, []Action{ActionListDatabases}}}, true
	case "userAdminAnyDatabase":
		return PrivilegeSet{{allDB, userAdminActions}, {ClusterResource, []Action{ActionListDatabases}}}, true
	case "clusterMonitor":
		return PrivilegeSet{
			{ClusterResource, clusterMonitorActions},
			{allDB, clusterMonitorDBActions},
		}, true
	case "clusterManager", "clusterAdmin":
		return PrivilegeSet{
			{ClusterResource, concat(clusterMonitorActions, clusterManagerActions)},
			{allDB, []Action{ActionDropDatabase}},
		}, true
	case "hostManager":
		return PrivilegeSet{
			{ClusterResource, clusterManagerActions},
			{allDB, []Action{ActionDropDatabase}},
		}, true
	case "backup":
		return PrivilegeSet{
			{allDB, readActions},
			{ClusterResource, []Action{ActionServerStatus, ActionListDatabases}},
			{CollectionResource("admin", "system.users"), []Action{ActionViewUser}},
			{CollectionResource("admin", "system.roles"), []Action{ActionViewRole}},
		}, true
	case "restore":
		return PrivilegeSet{
			{allDB, concat(writeActions, dbAdminActions, userAdminActions)},
		}, true
	case "root":
		return PrivilegeSet{
			{allDB, concat(readActions, writeActions, dbAdminActions, userAdminActions)},
			{ClusterResource, concat(clusterMonitorActions, clusterManagerActions)},
		}, true
	}
	return nil, false
}

func concat(groups ...[]Action) []Action {
	var out []Action
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}
