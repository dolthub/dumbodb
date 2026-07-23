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
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func roleCtx() context.Context {
	return conninfo.Ctx(context.Background(), conninfo.New())
}

func collPrivilege(db, coll string, actions ...string) *types.Document {
	a := types.MakeArray(len(actions))
	for _, act := range actions {
		a.Append(act)
	}
	return must.NotFail(types.NewDocument(
		"resource", must.NotFail(types.NewDocument("db", db, "collection", coll)),
		"actions", a,
	))
}

func errorHasCode(err error, code handlererrors.ErrorCode) bool {
	var ce *handlererrors.CommandError
	return errors.As(err, &ce) && ce.Code() == code
}

func createRole(ctx context.Context, h *Handler, db, role string, privileges, roles *types.Array) error {
	if privileges == nil {
		privileges = types.MakeArray(0)
	}
	if roles == nil {
		roles = types.MakeArray(0)
	}
	_, err := h.MsgCreateRole(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"createRole", role,
		"privileges", privileges,
		"roles", roles,
		"$db", db,
	)))))
	return err
}

func TestCreateRole_StoresDocument(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "r1",
		must.NotFail(types.NewArray(collPrivilege("mydb", "", "find"))), nil))

	doc, err := h.loadRoleDoc(ctx, "mydb", "r1")
	require.NoError(t, err)
	require.NotNil(t, doc)
	require.Equal(t, "mydb.r1", must.NotFail(doc.Get("_id")))
	require.Equal(t, "r1", must.NotFail(doc.Get("role")))
	require.Equal(t, "mydb", must.NotFail(doc.Get("db")))
	require.Equal(t, 1, must.NotFail(doc.Get("privileges")).(*types.Array).Len())
}

func TestCreateRole_Duplicate(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "dup", nil, nil))
	err := createRole(ctx, h, "mydb", "dup", nil, nil)
	require.True(t, errorHasCode(err, handlererrors.ErrRoleAlreadyExists), "want RoleAlreadyExists(51002), got %v", err)
}

func TestCreateRole_MissingInheritedRole(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	err := createRole(ctx, h, "mydb", "r2", nil,
		must.NotFail(types.NewArray(must.NotFail(types.NewDocument("role", "ghost", "db", "mydb")))))
	require.True(t, errorHasCode(err, handlererrors.ErrRoleNotFound), "want RoleNotFound(31), got %v", err)
}

func TestCreateRole_InheritsBuiltin(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "admin", "r3", nil,
		must.NotFail(types.NewArray(must.NotFail(types.NewDocument("role", "read", "db", "admin"))))))
}

func TestCreateRole_NonAdminClusterResource(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	clusterPriv := must.NotFail(types.NewDocument(
		"resource", must.NotFail(types.NewDocument("cluster", true)),
		"actions", must.NotFail(types.NewArray("shutdown")),
	))
	err := createRole(ctx, h, "mydb", "r4", must.NotFail(types.NewArray(clusterPriv)), nil)
	require.True(t, errorHasCode(err, handlererrors.ErrInvalidRoleModification), "want InvalidRoleModification(49), got %v", err)
}

func TestCreateRole_AdminAnyResource(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	anyPriv := must.NotFail(types.NewDocument(
		"resource", must.NotFail(types.NewDocument("anyResource", true)),
		"actions", must.NotFail(types.NewArray("find")),
	))
	require.NoError(t, createRole(ctx, h, "admin", "r5", must.NotFail(types.NewArray(anyPriv)), nil))
}

func TestUpdateRole_ReplacesPrivileges(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "r6",
		must.NotFail(types.NewArray(collPrivilege("mydb", "", "find"))), nil))

	_, err := h.MsgUpdateRole(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"updateRole", "r6",
		"privileges", must.NotFail(types.NewArray(collPrivilege("mydb", "", "insert", "remove"))),
		"$db", "mydb",
	)))))
	require.NoError(t, err)

	doc, err := h.loadRoleDoc(ctx, "mydb", "r6")
	require.NoError(t, err)
	privs := must.NotFail(doc.Get("privileges")).(*types.Array)
	require.Equal(t, 1, privs.Len())
	p0 := must.NotFail(privs.Get(0)).(*types.Document)
	require.Equal(t, 2, must.NotFail(p0.Get("actions")).(*types.Array).Len())
}

func TestUpdateRole_Missing(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	_, err := h.MsgUpdateRole(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"updateRole", "ghost",
		"roles", types.MakeArray(0),
		"$db", "mydb",
	)))))
	require.True(t, errorHasCode(err, handlererrors.ErrRoleNotFound), "want RoleNotFound(31), got %v", err)
}

func dropRole(ctx context.Context, h *Handler, db, role string) error {
	_, err := h.MsgDropRole(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"dropRole", role,
		"$db", db,
	)))))
	return err
}

func TestDropRole_Existing(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "r7", nil, nil))
	require.NoError(t, dropRole(ctx, h, "mydb", "r7"))

	doc, err := h.loadRoleDoc(ctx, "mydb", "r7")
	require.NoError(t, err)
	require.Nil(t, doc)
}

func TestDropRole_Missing(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	err := dropRole(ctx, h, "mydb", "ghost")
	require.True(t, errorHasCode(err, handlererrors.ErrRoleNotFound), "want RoleNotFound(31), got %v", err)
}

func TestDropRole_Builtin(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	err := dropRole(ctx, h, "mydb", "read")
	require.True(t, errorHasCode(err, handlererrors.ErrBadValue), "want BadValue(2), got %v", err)
}

func TestDropAllRolesFromDatabase(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "a", nil, nil))
	require.NoError(t, createRole(ctx, h, "mydb", "b", nil, nil))
	require.NoError(t, createRole(ctx, h, "otherdb", "c", nil, nil))

	res, err := h.MsgDropAllRolesFromDatabase(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"dropAllRolesFromDatabase", int32(1),
		"$db", "mydb",
	)))))
	require.NoError(t, err)

	resDoc := must.NotFail(opMsgDocument(res))
	require.Equal(t, int32(2), must.NotFail(resDoc.Get("n")))

	other, err := h.loadRoleDoc(ctx, "otherdb", "c")
	require.NoError(t, err)
	require.NotNil(t, other, "dropAllRolesFromDatabase must not touch other databases")
}
