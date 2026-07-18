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

func TestResourceCovers(t *testing.T) {
	coll := CollectionResource("mydb", "c")
	sys := CollectionResource("mydb", "system.users")
	other := CollectionResource("other", "c")
	cluster := ClusterResource

	for _, tc := range []struct {
		name    string
		pattern Resource
		target  Resource
		want    bool
	}{
		{"exact match", CollectionResource("mydb", "c"), coll, true},
		{"exact wrong coll", CollectionResource("mydb", "d"), coll, false},
		{"exact wrong db", CollectionResource("other", "c"), coll, false},
		{"db-wide covers coll", DatabaseResource("mydb"), coll, true},
		{"db-wide wrong db", DatabaseResource("other"), coll, false},
		{"db-wide excludes system", DatabaseResource("mydb"), sys, false},
		{"named covers system", CollectionResource("mydb", "system.users"), sys, true},
		{"all-db covers any coll", Resource{}, coll, true},
		{"all-db covers other db", Resource{}, other, true},
		{"all-db excludes system", Resource{}, sys, false},
		{"cross-db named coll", Resource{Collection: "c"}, other, true},
		{"cross-db named wrong coll", Resource{Collection: "d"}, coll, false},
		{"anyResource covers system", Resource{AnyResource: true}, sys, true},
		{"anyResource covers cluster", Resource{AnyResource: true}, cluster, true},
		{"cluster covers cluster", cluster, cluster, true},
		{"cluster excludes coll", cluster, coll, false},
		{"coll excludes cluster", CollectionResource("mydb", "c"), cluster, false},
		{"db-wide covers db target", DatabaseResource("mydb"), DatabaseResource("mydb"), true},
		{"named coll excludes db target", CollectionResource("mydb", "c"), DatabaseResource("mydb"), false},
	} {
		require.Equalf(t, tc.want, tc.pattern.Covers(tc.target), "%s", tc.name)
	}
}

func TestPrivilegeSetAuthorized(t *testing.T) {
	ps := PrivilegeSet{
		{Resource: DatabaseResource("mydb"), Actions: []Action{ActionFind, ActionInsert}},
		{Resource: ClusterResource, Actions: []Action{ActionServerStatus}},
		{Resource: CollectionResource("admin", "system.users"), Actions: []Action{ActionViewUser}},
	}

	require.True(t, ps.Authorized(ActionFind, CollectionResource("mydb", "c")))
	require.True(t, ps.Authorized(ActionInsert, CollectionResource("mydb", "c")))
	require.False(t, ps.Authorized(ActionUpdate, CollectionResource("mydb", "c")))
	require.False(t, ps.Authorized(ActionFind, CollectionResource("other", "c")))
	require.False(t, ps.Authorized(ActionFind, CollectionResource("mydb", "system.users")))

	require.True(t, ps.Authorized(ActionServerStatus, ClusterResource))
	require.False(t, ps.Authorized(ActionSetParameter, ClusterResource))

	require.True(t, ps.Authorized(ActionViewUser, CollectionResource("admin", "system.users")))
}

func TestAnyActionGrantsEverything(t *testing.T) {
	ps := PrivilegeSet{{Resource: Resource{AnyResource: true}, Actions: []Action{AnyAction}}}

	require.True(t, ps.Authorized(ActionFind, CollectionResource("any", "c")))
	require.True(t, ps.Authorized(ActionDropDatabase, DatabaseResource("any")))
	require.True(t, ps.Authorized(ActionSetParameter, ClusterResource))
	require.True(t, ps.Authorized(ActionViewUser, CollectionResource("admin", "system.roles")))
}
