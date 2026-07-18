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

func createUserWithRole(t *testing.T, h *Handler, db, user, role string) {
	t.Helper()

	ctx := conninfo.Ctx(context.Background(), conninfo.New())
	_, err := h.MsgCreateUser(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"createUser", user,
		"pwd", "pw",
		"roles", must.NotFail(types.NewArray(role)),
		"$db", db,
	)))))
	require.NoError(t, err)
}

func TestEffectivePrivileges_FromStoredRoles(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "mydb", "alice", "readWrite")

	ci := conninfo.New()
	ci.SetAuth("alice", "", nil, "mydb")
	ctx := conninfo.Ctx(context.Background(), ci)

	privs, err := h.effectivePrivileges(ctx)
	require.NoError(t, err)
	require.True(t, privs.Authorized(authz.ActionInsert, authz.CollectionResource("mydb", "c")))
	require.True(t, privs.Authorized(authz.ActionFind, authz.CollectionResource("mydb", "c")))
	require.False(t, privs.Authorized(authz.ActionInsert, authz.CollectionResource("other", "c")))
	require.False(t, privs.Authorized(authz.ActionCreateUser, authz.DatabaseResource("mydb")))

	_, _, ok := ci.PrivilegeCache()
	require.True(t, ok, "effectivePrivileges must populate the connection cache")
}

func TestEffectivePrivileges_Unauthenticated(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	privs, err := h.effectivePrivileges(ctx)
	require.NoError(t, err)
	require.Nil(t, privs)
}

func TestEffectivePrivileges_CacheInvalidatesOnBump(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "mydb", "alice", "read")

	ci := conninfo.New()
	ci.SetAuth("alice", "", nil, "mydb")
	ctx := conninfo.Ctx(context.Background(), ci)

	privs, err := h.effectivePrivileges(ctx)
	require.NoError(t, err)
	require.False(t, privs.Authorized(authz.ActionInsert, authz.CollectionResource("mydb", "c")))

	// Change alice's role and bump the generation; the recomputed set reflects it.
	_, err = h.MsgUpdateUser(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"updateUser", "alice",
		"roles", must.NotFail(types.NewArray("readWrite")),
		"$db", "mydb",
	)))))
	require.NoError(t, err)

	privs, err = h.effectivePrivileges(ctx)
	require.NoError(t, err)
	require.True(t, privs.Authorized(authz.ActionInsert, authz.CollectionResource("mydb", "c")))
}
