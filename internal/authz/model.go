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

import "strings"

// Action is a MongoDB privilege action name.
type Action string

// The DumboDB-enforced MongoDB actions. Action names outside this set stay valid
// in custom roles for parity, but never gate a command.
const (
	ActionFind                         Action = "find"
	ActionInsert                       Action = "insert"
	ActionUpdate                       Action = "update"
	ActionRemove                       Action = "remove"
	ActionBypassDocumentValidation     Action = "bypassDocumentValidation"
	ActionCreateCollection             Action = "createCollection"
	ActionCreateIndex                  Action = "createIndex"
	ActionDropCollection               Action = "dropCollection"
	ActionDropIndex                    Action = "dropIndex"
	ActionDropDatabase                 Action = "dropDatabase"
	ActionCollMod                      Action = "collMod"
	ActionCompact                      Action = "compact"
	ActionConvertToCapped              Action = "convertToCapped"
	ActionRenameCollectionSameDB       Action = "renameCollectionSameDB"
	ActionCreateSearchIndexes          Action = "createSearchIndexes"
	ActionDropSearchIndex              Action = "dropSearchIndex"
	ActionListSearchIndexes            Action = "listSearchIndexes"
	ActionUpdateSearchIndex            Action = "updateSearchIndex"
	ActionKillCursors                  Action = "killCursors"
	ActionListCollections              Action = "listCollections"
	ActionListIndexes                  Action = "listIndexes"
	ActionListDatabases                Action = "listDatabases"
	ActionCollStats                    Action = "collStats"
	ActionDBStats                      Action = "dbStats"
	ActionDataSize                     Action = "dataSize"
	ActionValidate                     Action = "validate"
	ActionServerStatus                 Action = "serverStatus"
	ActionGetParameter                 Action = "getParameter"
	ActionSetParameter                 Action = "setParameter"
	ActionHostInfo                     Action = "hostInfo"
	ActionTop                          Action = "top"
	ActionGetLog                       Action = "getLog"
	ActionCreateUser                   Action = "createUser"
	ActionDropUser                     Action = "dropUser"
	ActionChangePassword               Action = "changePassword"
	ActionChangeCustomData             Action = "changeCustomData"
	ActionChangeOwnPassword            Action = "changeOwnPassword"
	ActionChangeOwnCustomData          Action = "changeOwnCustomData"
	ActionGrantRole                    Action = "grantRole"
	ActionRevokeRole                   Action = "revokeRole"
	ActionCreateRole                   Action = "createRole"
	ActionDropRole                     Action = "dropRole"
	ActionViewUser                     Action = "viewUser"
	ActionViewRole                     Action = "viewRole"
	ActionSetAuthenticationRestriction Action = "setAuthenticationRestriction"

	// AnyAction grants every action on the resource it is attached to.
	AnyAction Action = "anyAction"
)

// Resource is a privilege resource pattern. Exactly one form is used:
// {DB,Collection}, {DB,""}, {"",Collection}, {"",""}, {Cluster:true}, or
// {AnyResource:true}.
type Resource struct {
	DB          string
	Collection  string
	Cluster     bool
	AnyResource bool
}

// CollectionResource is the {db, collection} resource for one collection.
func CollectionResource(db, collection string) Resource {
	return Resource{DB: db, Collection: collection}
}

// DatabaseResource is the {db, ""} database-wide resource.
func DatabaseResource(db string) Resource { return Resource{DB: db} }

// ClusterResource is the {cluster:true} server-wide resource.
var ClusterResource = Resource{Cluster: true}

// Covers reports whether this resource pattern grants access to a concrete
// target resource. A collection-wildcard pattern ({db,""} or {"",""}) excludes
// system collections unless {AnyResource:true}.
func (r Resource) Covers(target Resource) bool {
	if r.AnyResource {
		return true
	}
	if target.Cluster {
		return r.Cluster
	}
	if r.Cluster {
		return false
	}
	if r.DB != "" && r.DB != target.DB {
		return false
	}
	if r.Collection != "" {
		return r.Collection == target.Collection
	}
	return !IsSystemCollection(target.Collection)
}

// IsSystemCollection reports whether a collection name is in the reserved
// system.* namespace.
func IsSystemCollection(collection string) bool {
	return strings.HasPrefix(collection, "system.")
}

// Privilege is one resource plus the actions permitted on it.
type Privilege struct {
	Resource Resource
	Actions  []Action
}

// PrivilegeSet is a computed effective privilege set.
type PrivilegeSet []Privilege

// Authorized reports whether the set grants action on the target resource.
func (ps PrivilegeSet) Authorized(action Action, target Resource) bool {
	for _, p := range ps {
		if !p.Resource.Covers(target) {
			continue
		}
		for _, a := range p.Actions {
			if a == action || a == AnyAction {
				return true
			}
		}
	}
	return false
}
