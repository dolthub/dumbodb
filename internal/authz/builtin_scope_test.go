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

// Before the fix BuiltinRole ignored the database for admin-only roles, so a
// custom role named "root" in any database resolved to cluster root.
func TestAdminOnlyBuiltinRolesDoNotApplyOutsideAdmin(t *testing.T) {
	for role := range builtinRoles {
		if IsBuiltinRoleOnDB(role, "tenant") {
			continue
		}
		t.Run(role, func(t *testing.T) {
			_, ok := BuiltinRole(role, "tenant")
			require.False(t, ok)
			require.Empty(t, Resolve([]Role{{role, "tenant"}}, NoCustomRoles))
		})
	}

	require.True(t, Resolve([]Role{{"root", "admin"}}, NoCustomRoles).Authorized(ActionCreateUser, DatabaseResource("x")))
	require.True(t, Resolve([]Role{{"read", "tenant"}}, NoCustomRoles).Authorized(ActionFind, CollectionResource("tenant", "c")))
}
