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

	"github.com/FerretDB/wire"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/authz"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
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

func authzCmd(t *testing.T, name, db, coll string) *wire.OpMsg {
	t.Helper()
	return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(name, coll, "$db", db))))
}

func isUnauthorized(t *testing.T, err error) bool {
	t.Helper()
	var ce *handlererrors.CommandError
	return errors.As(err, &ce) && ce.Code() == handlererrors.ErrUnauthorized
}

func TestAuthorize_EnforcesBuiltinRoles(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "mydb", "reader", "read")
	createUserWithRole(t, h, "mydb", "writer", "readWrite")
	createUserWithRole(t, h, "admin", "boss", "root")

	authAs := func(user, db string) context.Context {
		ci := conninfo.New()
		ci.SetAuth(user, "", nil, db)
		return conninfo.Ctx(context.Background(), ci)
	}

	reader := authAs("reader", "mydb")
	require.NoError(t, h.authorize(reader, authzCmd(t, "find", "mydb", "c")))
	require.NoError(t, h.authorize(reader, authzCmd(t, "collStats", "mydb", "c")))
	require.True(t, isUnauthorized(t, h.authorize(reader, authzCmd(t, "insert", "mydb", "c"))))
	require.True(t, isUnauthorized(t, h.authorize(reader, authzCmd(t, "dropDatabase", "mydb", "c"))))
	require.True(t, isUnauthorized(t, h.authorize(reader, authzCmd(t, "find", "other", "c"))))

	writer := authAs("writer", "mydb")
	require.NoError(t, h.authorize(writer, authzCmd(t, "insert", "mydb", "c")))
	require.NoError(t, h.authorize(writer, authzCmd(t, "find", "mydb", "c")))
	require.True(t, isUnauthorized(t, h.authorize(writer, authzCmd(t, "createUser", "mydb", "c"))))
	require.True(t, isUnauthorized(t, h.authorize(writer, authzCmd(t, "insert", "other", "c"))))

	boss := authAs("boss", "admin")
	require.NoError(t, h.authorize(boss, authzCmd(t, "insert", "anydb", "c")))
	require.NoError(t, h.authorize(boss, authzCmd(t, "createUser", "anydb", "c")))
	require.NoError(t, h.authorize(boss, authzCmd(t, "dropDatabase", "anydb", "c")))
	require.NoError(t, h.authorize(boss, authzCmd(t, "serverStatus", "admin", "c")))
}

func TestAuthorize_UpdateUserRequiresPrivilegePerField(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "mydb", "reader", "read")
	createUserWithRole(t, h, "mydb", "dbUserAdmin", "userAdmin")
	createUserWithRole(t, h, "admin", "anyUserAdmin", "userAdminAnyDatabase")

	authAs := func(user, db string) context.Context {
		ci := conninfo.New()
		ci.SetAuth(user, "", nil, db)
		return conninfo.Ctx(context.Background(), ci)
	}
	updateUser := func(target, db, field string, value any) *wire.OpMsg {
		return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument("updateUser", target, field, value, "$db", db))))
	}
	readRole := must.NotFail(types.NewArray("read"))
	identity := must.NotFail(types.NewDocument("name", "N", "email", "n@example.com"))

	reader := authAs("reader", "mydb")
	require.True(t, isUnauthorized(t, h.authorize(reader, updateUser("reader", "mydb", "pwd", "x"))))
	require.True(t, isUnauthorized(t, h.authorize(reader, updateUser("reader", "mydb", "roles", readRole))))
	require.True(t, isUnauthorized(t, h.authorize(reader, updateUser("reader", "mydb", "commitIdentity", identity))))

	dbUserAdmin := authAs("dbUserAdmin", "mydb")
	require.NoError(t, h.authorize(dbUserAdmin, updateUser("reader", "mydb", "pwd", "x")))
	require.NoError(t, h.authorize(dbUserAdmin, updateUser("reader", "mydb", "customData", must.NotFail(types.NewDocument()))))
	require.NoError(t, h.authorize(dbUserAdmin, updateUser("reader", "mydb", "commitIdentity", identity)))
	require.True(t, isUnauthorized(t, h.authorize(dbUserAdmin, updateUser("reader", "mydb", "roles", readRole))))

	anyUserAdmin := authAs("anyUserAdmin", "admin")
	require.NoError(t, h.authorize(anyUserAdmin, updateUser("reader", "mydb", "roles", readRole)))
}

func TestAuthorize_GrantsCheckEachRoleAndPrivilegeDB(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "mydb", "dbUserAdmin", "userAdmin")
	createUserWithRole(t, h, "admin", "adminUserAdmin", "userAdmin")
	createUserWithRole(t, h, "admin", "anyUserAdmin", "userAdminAnyDatabase")

	authAs := func(user, db string) context.Context {
		ci := conninfo.New()
		ci.SetAuth(user, "", nil, db)
		return conninfo.Ctx(context.Background(), ci)
	}
	roleRef := func(role, db string) *types.Document { return must.NotFail(types.NewDocument("role", role, "db", db)) }
	command := func(name, target, db string, fields ...any) *wire.OpMsg {
		pairs := append([]any{name, target}, fields...)
		pairs = append(pairs, "$db", db)
		return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(pairs...))))
	}
	rootRoles := must.NotFail(types.NewArray(roleRef("root", "admin")))
	mydbFind := must.NotFail(types.NewArray(must.NotFail(types.NewDocument(
		"resource", must.NotFail(types.NewDocument("db", "mydb", "collection", "")),
		"actions", must.NotFail(types.NewArray("find")),
	))))
	clusterStatus := must.NotFail(types.NewArray(must.NotFail(types.NewDocument(
		"resource", must.NotFail(types.NewDocument("cluster", true)),
		"actions", must.NotFail(types.NewArray("serverStatus")),
	))))

	dbUserAdmin := authAs("dbUserAdmin", "mydb")
	require.NoError(t, h.authorize(dbUserAdmin, command("grantRolesToUser", "u", "mydb", "roles", must.NotFail(types.NewArray("read")))))
	require.True(t, isUnauthorized(t, h.authorize(dbUserAdmin, command("grantRolesToUser", "dbUserAdmin", "mydb", "roles", rootRoles))))
	require.True(t, isUnauthorized(t, h.authorize(dbUserAdmin, command("revokeRolesFromUser", "u", "mydb", "roles", rootRoles))))
	require.True(t, isUnauthorized(t, h.authorize(dbUserAdmin, command("grantRolesToRole", "r", "mydb", "roles", rootRoles))))
	require.True(t, isUnauthorized(t, h.authorize(dbUserAdmin, command("createUser", "u2", "mydb", "pwd", "pw", "roles", rootRoles))))
	require.True(t, isUnauthorized(t, h.authorize(dbUserAdmin, command("createRole", "r2", "mydb", "privileges", types.MakeArray(0), "roles", rootRoles))))
	require.True(t, isUnauthorized(t, h.authorize(dbUserAdmin, command("updateRole", "r", "mydb", "roles", types.MakeArray(0)))))
	require.NoError(t, h.authorize(dbUserAdmin, command("grantPrivilegesToRole", "r", "mydb", "privileges", mydbFind)))

	adminUserAdmin := authAs("adminUserAdmin", "admin")
	require.NoError(t, h.authorize(adminUserAdmin, command("grantRolesToUser", "u", "mydb", "roles", must.NotFail(types.NewArray(roleRef("read", "admin"))))))
	require.NoError(t, h.authorize(adminUserAdmin, command("grantPrivilegesToRole", "r", "admin", "privileges", clusterStatus)))
	require.True(t, isUnauthorized(t, h.authorize(adminUserAdmin, command("grantPrivilegesToRole", "r", "admin", "privileges", mydbFind))))

	anyUserAdmin := authAs("anyUserAdmin", "admin")
	require.NoError(t, h.authorize(anyUserAdmin, command("grantRolesToUser", "u", "mydb", "roles", rootRoles)))
	require.NoError(t, h.authorize(anyUserAdmin, command("updateRole", "r", "mydb", "roles", rootRoles)))
}

func TestAuthorize_UnmappedCommandNeedsNoPrivilege(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "mydb", "reader", "read")
	ci := conninfo.New()
	ci.SetAuth("reader", "", nil, "mydb")
	ctx := conninfo.Ctx(context.Background(), ci)

	require.NoError(t, h.authorize(ctx, authzCmd(t, "someUnmappedCommand", "mydb", "c")))
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
