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

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func grantPrivileges(ctx context.Context, h *Handler, db, role string, privileges *types.Array) error {
	_, err := h.MsgGrantPrivilegesToRole(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"grantPrivilegesToRole", role,
		"privileges", privileges,
		"$db", db,
	)))))
	return err
}

func revokePrivilegesFrom(ctx context.Context, h *Handler, db, role string, privileges *types.Array) error {
	_, err := h.MsgRevokePrivilegesFromRole(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"revokePrivilegesFromRole", role,
		"privileges", privileges,
		"$db", db,
	)))))
	return err
}

func rolePrivileges(t *testing.T, h *Handler, ctx context.Context, db, role string) *types.Array {
	t.Helper()
	doc, err := h.loadRoleDoc(ctx, db, role)
	require.NoError(t, err)
	require.NotNil(t, doc)
	return must.NotFail(doc.Get("privileges")).(*types.Array)
}

func TestGrantPrivilegesToRole_AppendAndUnion(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "r",
		must.NotFail(types.NewArray(collPrivilege("mydb", "c1", "find"))), nil))

	require.NoError(t, grantPrivileges(ctx, h, "mydb", "r",
		must.NotFail(types.NewArray(collPrivilege("mydb", "c2", "insert")))))
	require.Equal(t, 2, rolePrivileges(t, h, ctx, "mydb", "r").Len(), "new resource must append")

	require.NoError(t, grantPrivileges(ctx, h, "mydb", "r",
		must.NotFail(types.NewArray(collPrivilege("mydb", "c1", "insert")))))
	privs := rolePrivileges(t, h, ctx, "mydb", "r")
	require.Equal(t, 2, privs.Len(), "existing resource must not add a new entry")

	p0 := must.NotFail(privs.Get(0)).(*types.Document)
	require.Equal(t, 2, must.NotFail(p0.Get("actions")).(*types.Array).Len(), "existing resource must union actions")
}

func TestRevokePrivilegesFromRole_Exact(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "r",
		must.NotFail(types.NewArray(collPrivilege("mydb", "", "find", "insert"))), nil))

	require.NoError(t, revokePrivilegesFrom(ctx, h, "mydb", "r",
		must.NotFail(types.NewArray(collPrivilege("mydb", "", "insert")))))

	privs := rolePrivileges(t, h, ctx, "mydb", "r")
	require.Equal(t, 1, privs.Len())
	p0 := must.NotFail(privs.Get(0)).(*types.Document)
	require.Equal(t, 1, must.NotFail(p0.Get("actions")).(*types.Array).Len())
}

func TestRevokePrivilegesFromRole_Empties(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "r",
		must.NotFail(types.NewArray(collPrivilege("mydb", "", "find"))), nil))

	require.NoError(t, revokePrivilegesFrom(ctx, h, "mydb", "r",
		must.NotFail(types.NewArray(collPrivilege("mydb", "", "find")))))

	require.Equal(t, 0, rolePrivileges(t, h, ctx, "mydb", "r").Len())
}

func TestRevokePrivilegesFromRole_NonMatchNoop(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "r",
		must.NotFail(types.NewArray(collPrivilege("mydb", "", "find", "insert"))), nil))

	require.NoError(t, revokePrivilegesFrom(ctx, h, "mydb", "r",
		must.NotFail(types.NewArray(collPrivilege("mydb", "c1", "find")))))

	privs := rolePrivileges(t, h, ctx, "mydb", "r")
	require.Equal(t, 1, privs.Len())
	p0 := must.NotFail(privs.Get(0)).(*types.Document)
	require.Equal(t, 2, must.NotFail(p0.Get("actions")).(*types.Array).Len())
}

func TestGrantPrivilegesToRole_Missing(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	err := grantPrivileges(ctx, h, "mydb", "ghost",
		must.NotFail(types.NewArray(collPrivilege("mydb", "", "find"))))
	require.True(t, errorHasCode(err, handlererrors.ErrRoleNotFound), "want RoleNotFound(31), got %v", err)
}

func TestGrantRevokeRolesToRole(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "base", nil, nil))
	require.NoError(t, createRole(ctx, h, "mydb", "derived", nil, nil))

	_, err := h.MsgGrantRolesToRole(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"grantRolesToRole", "derived",
		"roles", must.NotFail(types.NewArray("base")),
		"$db", "mydb",
	)))))
	require.NoError(t, err)

	doc, err := h.loadRoleDoc(ctx, "mydb", "derived")
	require.NoError(t, err)
	require.Equal(t, 1, must.NotFail(doc.Get("roles")).(*types.Array).Len())

	_, err = h.MsgRevokeRolesFromRole(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"revokeRolesFromRole", "derived",
		"roles", must.NotFail(types.NewArray("base")),
		"$db", "mydb",
	)))))
	require.NoError(t, err)

	doc, err = h.loadRoleDoc(ctx, "mydb", "derived")
	require.NoError(t, err)
	require.Equal(t, 0, must.NotFail(doc.Get("roles")).(*types.Array).Len())
}

func rolesInfo(t *testing.T, h *Handler, ctx context.Context, db string, value any, showPrivileges bool) *types.Array {
	t.Helper()
	fields := []any{"rolesInfo", value, "$db", db}
	if showPrivileges {
		fields = append(fields, "showPrivileges", true)
	}
	res, err := h.MsgRolesInfo(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(fields...)))))
	require.NoError(t, err)
	resDoc := must.NotFail(opMsgDocument(res))
	return must.NotFail(resDoc.Get("roles")).(*types.Array)
}

func TestRolesInfo_SingleAndMissing(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "r", nil, nil))

	roles := rolesInfo(t, h, ctx, "mydb", "r", false)
	require.Equal(t, 1, roles.Len())
	r0 := must.NotFail(roles.Get(0)).(*types.Document)
	require.Equal(t, false, must.NotFail(r0.Get("isBuiltin")))

	require.Equal(t, 0, rolesInfo(t, h, ctx, "mydb", "ghost", false).Len())
}

func TestRolesInfo_ShowPrivilegesAndInherited(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "base",
		must.NotFail(types.NewArray(collPrivilege("mydb", "", "insert"))), nil))
	require.NoError(t, createRole(ctx, h, "mydb", "r",
		must.NotFail(types.NewArray(collPrivilege("mydb", "", "find"))),
		must.NotFail(types.NewArray(must.NotFail(types.NewDocument("role", "base", "db", "mydb"))))))

	roles := rolesInfo(t, h, ctx, "mydb", "r", true)
	require.Equal(t, 1, roles.Len())
	r0 := must.NotFail(roles.Get(0)).(*types.Document)

	require.True(t, r0.Has("privileges"))
	require.True(t, r0.Has("inheritedPrivileges"))
	require.Equal(t, 1, must.NotFail(r0.Get("inheritedRoles")).(*types.Array).Len())
	require.Equal(t, 2, must.NotFail(r0.Get("inheritedPrivileges")).(*types.Array).Len(),
		"inheritedPrivileges must union own and inherited role privileges")
}

func TestRolesInfo_All(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "a", nil, nil))
	require.NoError(t, createRole(ctx, h, "mydb", "b", nil, nil))
	require.NoError(t, createRole(ctx, h, "otherdb", "c", nil, nil))

	require.Equal(t, 2, rolesInfo(t, h, ctx, "mydb", int32(1), false).Len())
}
