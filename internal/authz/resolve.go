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

// Role is a role reference {role, db}.
type Role struct {
	Role string
	DB   string
}

// RoleResolver returns a user-defined role's own privileges and the roles it
// inherits. ok is false when the role does not exist. Built-in roles are never
// passed to a resolver; they are synthesized.
type RoleResolver func(r Role) (privs PrivilegeSet, inherits []Role, ok bool)

// NoCustomRoles is a resolver with no user-defined roles.
func NoCustomRoles(Role) (PrivilegeSet, []Role, bool) { return nil, nil, false }

// Resolve computes the effective privilege set for the granted roles, taking the
// transitive closure over role inheritance. Cycles are broken by visiting each
// role once.
func Resolve(roles []Role, resolver RoleResolver) PrivilegeSet {
	var out PrivilegeSet
	seen := map[Role]bool{}

	var walk func(r Role)
	walk = func(r Role) {
		if seen[r] {
			return
		}
		seen[r] = true

		if ps, ok := BuiltinRole(r.Role, r.DB); ok {
			out = append(out, ps...)
			return
		}

		privs, inherits, ok := resolver(r)
		if !ok {
			return
		}
		out = append(out, privs...)
		for _, ir := range inherits {
			walk(ir)
		}
	}

	for _, r := range roles {
		walk(r)
	}
	return out
}
