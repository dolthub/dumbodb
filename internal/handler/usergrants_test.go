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
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func createUser(ctx context.Context, h *Handler, db, user string, roles *types.Array) error {
	if roles == nil {
		roles = types.MakeArray(0)
	}
	_, err := h.MsgCreateUser(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"createUser", user,
		"pwd", "pw",
		"roles", roles,
		"$db", db,
	)))))
	return err
}

func grantRolesToUser(ctx context.Context, h *Handler, db, user string, roles *types.Array) error {
	_, err := h.MsgGrantRolesToUser(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"grantRolesToUser", user,
		"roles", roles,
		"$db", db,
	)))))
	return err
}

func revokeRolesFromUser(ctx context.Context, h *Handler, db, user string, roles *types.Array) error {
	_, err := h.MsgRevokeRolesFromUser(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"revokeRolesFromUser", user,
		"roles", roles,
		"$db", db,
	)))))
	return err
}

func TestCreateUser_MissingRole(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	err := createUser(ctx, h, "mydb", "u", must.NotFail(types.NewArray("nosuchrole")))
	require.True(t, errorHasCode(err, handlererrors.ErrRoleNotFound), "want RoleNotFound(31), got %v", err)
}

func TestGrantRolesToUser_UnionAndEnforcement(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "reader",
		must.NotFail(types.NewArray(collPrivilege("mydb", "", "find"))), nil))
	require.NoError(t, createUser(ctx, h, "mydb", "u", nil))

	require.NoError(t, grantRolesToUser(ctx, h, "mydb", "u", must.NotFail(types.NewArray("reader"))))

	ci := conninfo.New()
	ci.SetAuth("u", "", nil, "mydb")
	authCtx := conninfo.Ctx(context.Background(), ci)
	privs, err := h.effectivePrivileges(authCtx)
	require.NoError(t, err)
	require.True(t, privs.Authorized(authz.ActionFind, authz.CollectionResource("mydb", "c")))

	doc, err := h.loadUserDoc(ctx, "mydb", "u")
	require.NoError(t, err)
	require.Equal(t, 1, must.NotFail(doc.Get("roles")).(*types.Array).Len())
}

func TestGrantRolesToUser_MissingUser(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	err := grantRolesToUser(ctx, h, "mydb", "ghost", must.NotFail(types.NewArray("read")))
	require.True(t, errorHasCode(err, handlererrors.ErrUserNotFound), "want UserNotFound(11), got %v", err)
}

func TestGrantRolesToUser_MissingRole(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createUser(ctx, h, "mydb", "u", nil))
	err := grantRolesToUser(ctx, h, "mydb", "u", must.NotFail(types.NewArray("nosuchrole")))
	require.True(t, errorHasCode(err, handlererrors.ErrRoleNotFound), "want RoleNotFound(31), got %v", err)
}

func TestRevokeRolesFromUser(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createUser(ctx, h, "mydb", "u",
		must.NotFail(types.NewArray("read", "dbAdmin"))))

	require.NoError(t, revokeRolesFromUser(ctx, h, "mydb", "u", must.NotFail(types.NewArray("dbAdmin"))))

	doc, err := h.loadUserDoc(ctx, "mydb", "u")
	require.NoError(t, err)
	require.Equal(t, 1, must.NotFail(doc.Get("roles")).(*types.Array).Len())
}

func TestAuthorize_UsersInfoSelfService(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "mydb", "eve", "read")

	ci := conninfo.New()
	ci.SetAuth("eve", "", nil, "mydb")
	ctx := conninfo.Ctx(context.Background(), ci)

	require.NoError(t, h.authorize(ctx, authzCmd(t, "usersInfo", "mydb", "eve")),
		"a user must be able to view itself via usersInfo")
	require.True(t, isUnauthorized(t, h.authorize(ctx, authzCmd(t, "usersInfo", "mydb", "other"))),
		"a read-only user must not view another user")
}
