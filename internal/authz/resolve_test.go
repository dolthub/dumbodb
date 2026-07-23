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

func TestResolveBuiltinRoles(t *testing.T) {
	privs := Resolve([]Role{{"read", "mydb"}, {"readWrite", "other"}}, NoCustomRoles)

	require.True(t, privs.Authorized(ActionFind, CollectionResource("mydb", "c")))
	require.True(t, privs.Authorized(ActionInsert, CollectionResource("other", "c")))
	require.False(t, privs.Authorized(ActionInsert, CollectionResource("mydb", "c")))
}

func TestResolveCustomRoleWithInheritance(t *testing.T) {
	resolver := func(r Role) (PrivilegeSet, []Role, bool) {
		if r.Role == "auditor" && r.DB == "mydb" {
			return PrivilegeSet{{CollectionResource("mydb", "audit"), []Action{ActionInsert}}},
				[]Role{{"read", "mydb"}}, true
		}
		return nil, nil, false
	}

	privs := Resolve([]Role{{"auditor", "mydb"}}, resolver)

	require.True(t, privs.Authorized(ActionInsert, CollectionResource("mydb", "audit")))
	require.True(t, privs.Authorized(ActionFind, CollectionResource("mydb", "c")))
	require.False(t, privs.Authorized(ActionInsert, CollectionResource("mydb", "c")))
	require.False(t, privs.Authorized(ActionFind, CollectionResource("other", "c")))
}

func TestResolveCycleSafe(t *testing.T) {
	resolver := func(r Role) (PrivilegeSet, []Role, bool) {
		switch r.Role {
		case "a":
			return PrivilegeSet{{CollectionResource("db", "a"), []Action{ActionFind}}}, []Role{{"b", "db"}}, true
		case "b":
			return PrivilegeSet{{CollectionResource("db", "b"), []Action{ActionFind}}}, []Role{{"a", "db"}}, true
		}
		return nil, nil, false
	}

	privs := Resolve([]Role{{"a", "db"}}, resolver)
	require.True(t, privs.Authorized(ActionFind, CollectionResource("db", "a")))
	require.True(t, privs.Authorized(ActionFind, CollectionResource("db", "b")))
}

func TestResolveUnknownRoleContributesNothing(t *testing.T) {
	privs := Resolve([]Role{{"ghost", "mydb"}}, NoCustomRoles)
	require.False(t, privs.Authorized(ActionFind, CollectionResource("mydb", "c")))
}
