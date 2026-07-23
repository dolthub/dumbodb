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

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func mustRole(t *testing.T, role, db string) PrivilegeSet {
	t.Helper()
	ps, ok := BuiltinRole(role, db)
	require.Truef(t, ok, "role %q should be built-in", role)
	return ps
}

func TestBuiltinRoleScoped(t *testing.T) {
	read := mustRole(t, "read", "mydb")
	require.True(t, read.Authorized(ActionFind, CollectionResource("mydb", "c")))
	require.False(t, read.Authorized(ActionInsert, CollectionResource("mydb", "c")))
	require.False(t, read.Authorized(ActionFind, CollectionResource("other", "c")))

	rw := mustRole(t, "readWrite", "mydb")
	require.True(t, rw.Authorized(ActionInsert, CollectionResource("mydb", "c")))
	require.True(t, rw.Authorized(ActionFind, CollectionResource("mydb", "c")))
	require.False(t, rw.Authorized(ActionCreateUser, DatabaseResource("mydb")))

	dbAdmin := mustRole(t, "dbAdmin", "mydb")
	require.True(t, dbAdmin.Authorized(ActionDropDatabase, DatabaseResource("mydb")))
	require.False(t, dbAdmin.Authorized(ActionFind, CollectionResource("mydb", "c")))
	require.False(t, dbAdmin.Authorized(ActionCreateUser, DatabaseResource("mydb")))

	userAdmin := mustRole(t, "userAdmin", "mydb")
	require.True(t, userAdmin.Authorized(ActionCreateUser, DatabaseResource("mydb")))
	require.False(t, userAdmin.Authorized(ActionFind, CollectionResource("mydb", "c")))

	dbOwner := mustRole(t, "dbOwner", "mydb")
	require.True(t, dbOwner.Authorized(ActionInsert, CollectionResource("mydb", "c")))
	require.True(t, dbOwner.Authorized(ActionDropDatabase, DatabaseResource("mydb")))
	require.True(t, dbOwner.Authorized(ActionCreateUser, DatabaseResource("mydb")))
}

func TestBuiltinRoleAnyDatabase(t *testing.T) {
	readAny := mustRole(t, "readAnyDatabase", "admin")
	require.True(t, readAny.Authorized(ActionFind, CollectionResource("anydb", "c")))
	require.True(t, readAny.Authorized(ActionFind, CollectionResource("other", "c")))
	require.False(t, readAny.Authorized(ActionInsert, CollectionResource("anydb", "c")))
	require.False(t, readAny.Authorized(ActionFind, CollectionResource("anydb", "system.users")))

	rwAny := mustRole(t, "readWriteAnyDatabase", "admin")
	require.True(t, rwAny.Authorized(ActionInsert, CollectionResource("anydb", "c")))
}

func TestBuiltinRoleClusterAndRoot(t *testing.T) {
	mon := mustRole(t, "clusterMonitor", "admin")
	require.True(t, mon.Authorized(ActionServerStatus, ClusterResource))
	require.False(t, mon.Authorized(ActionSetParameter, ClusterResource))

	clusterAdmin := mustRole(t, "clusterAdmin", "admin")
	require.True(t, clusterAdmin.Authorized(ActionSetParameter, ClusterResource))
	require.True(t, clusterAdmin.Authorized(ActionDropDatabase, DatabaseResource("anydb")))

	root := mustRole(t, "root", "admin")
	require.True(t, root.Authorized(ActionFind, CollectionResource("anydb", "c")))
	require.True(t, root.Authorized(ActionInsert, CollectionResource("anydb", "c")))
	require.True(t, root.Authorized(ActionDropDatabase, DatabaseResource("anydb")))
	require.True(t, root.Authorized(ActionCreateUser, DatabaseResource("admin")))
	require.True(t, root.Authorized(ActionServerStatus, ClusterResource))
}

func TestBuiltinRoleUnknown(t *testing.T) {
	_, ok := BuiltinRole("notARole", "mydb")
	require.False(t, ok)
	require.False(t, IsBuiltinRole("notARole"))
	require.True(t, IsBuiltinRole("readWrite"))
	require.True(t, IsBuiltinRole("root"))
}
