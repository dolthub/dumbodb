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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/authz"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Before the fix the per-connection privilege and commit-identity caches were
// keyed only by the global auth generation, so after the identity on a
// connection changed (logout then login as someone else) the previous user's
// privileges and commit author were reused.
func TestPrivilegeCache_DoesNotSurviveIdentityChange(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "admin", "boss", "root")
	createUserWithRole(t, h, "mydb", "low", "read")

	ci := conninfo.New()
	ctx := conninfo.Ctx(context.Background(), ci)

	ci.SetAuth("boss", "", nil, "admin")
	ci.SetAuthenticated()
	privs, err := h.effectivePrivileges(ctx)
	require.NoError(t, err)
	require.True(t, privs.Authorized(authz.ActionCreateUser, authz.DatabaseResource("admin")))

	name, _, ok, err := h.resolveCommitIdentity(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "boss", name)

	_, err = h.MsgLogout(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument("logout", int32(1), "$db", "admin")))))
	require.NoError(t, err)

	ci.SetAuth("low", "", nil, "mydb")
	ci.SetAuthenticated()

	privs, err = h.effectivePrivileges(ctx)
	require.NoError(t, err)
	require.False(t, privs.Authorized(authz.ActionCreateUser, authz.DatabaseResource("admin")),
		"the new identity must not inherit the previous user's privileges")
	require.True(t, privs.Authorized(authz.ActionFind, authz.CollectionResource("mydb", "c")))

	name, _, ok, err = h.resolveCommitIdentity(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "low", name, "the new identity must not inherit the previous commit author")
}

// The same reuse happened without logout: the cache was filled for one
// identity and a later SetAuth for another kept it.
func TestPrivilegeCache_ResetBySetAuth(t *testing.T) {
	ci := conninfo.New()
	ci.SetAuth("boss", "", nil, "admin")
	ci.SetPrivilegeCache(0, authz.PrivilegeSet{})
	ci.SetCommitIdentityCache(0, "boss", "boss@admin")

	ci.SetAuth("low", "", nil, "mydb")

	_, _, ok := ci.PrivilegeCache()
	require.False(t, ok)
	_, _, _, ok = ci.CommitIdentityCache()
	require.False(t, ok)
}
