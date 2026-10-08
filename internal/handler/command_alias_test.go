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
	"testing"

	"github.com/stretchr/testify/require"
)

// Every alias must resolve to the same canonical name, so a privilege rule
// written for one name covers all of them.
func TestCommandAliasesShareCanonicalName(t *testing.T) {
	h := authGateHandler(t, true)

	byCommand := map[*Command][]string{}
	for name, cmd := range h.commands {
		require.NotEmpty(t, cmd.name, "%s has no canonical name", name)
		byCommand[cmd] = append(byCommand[cmd], name)
	}
	for cmd, names := range byCommand {
		require.Contains(t, names, cmd.name, "canonical name of %v is not one of its registered names", names)
	}
	require.Equal(t, "findAndModify", h.commands["findandmodify"].name)
	require.Equal(t, "dbStats", h.commands["dbstats"].name)
}

func TestAuthorize_AliasUsesCanonicalPrivileges(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "appdb", "reader", "read")
	reader := authCtx("reader", "appdb")

	require.True(t, isUnauthorized(t, h.authorize(reader, authzCmd(t, "findandmodify", "victim", "secrets"))))
	require.True(t, isUnauthorized(t, h.authorize(reader, authzCmd(t, "dbstats", "victim", ""))))
	require.NoError(t, h.authorize(reader, authzCmd(t, "dbstats", "appdb", "")))
}
