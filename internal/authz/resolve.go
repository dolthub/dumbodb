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

type Role struct {
	Role string
	DB   string
}

type RoleResolver func(r Role) (privs PrivilegeSet, inherits []Role, ok bool)

func NoCustomRoles(Role) (PrivilegeSet, []Role, bool) { return nil, nil, false }

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
