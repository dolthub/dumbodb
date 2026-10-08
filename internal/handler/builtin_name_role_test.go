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

// Before the fix a userAdmin of any database could create a custom role
// named "root" there, assign it to a user, and that user resolved to cluster
// root. The role could not be dropped and rolesInfo showed the built-in.
func TestCustomRoleNamedLikeAdminBuiltinIsOrdinary(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "test", "root", nil, nil))
	_, err := h.MsgCreateUser(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"createUser", "x",
		"pwd", "p",
		"roles", must.NotFail(types.NewArray("root")),
		"$db", "test",
	)))))
	require.NoError(t, err)

	ci := conninfo.New()
	ci.SetAuth("x", "", nil, "test")
	privs, err := h.effectivePrivileges(conninfo.Ctx(context.Background(), ci))
	require.NoError(t, err)
	require.False(t, privs.Authorized(authz.ActionCreateUser, authz.DatabaseResource("admin")),
		"root@test must not grant cluster root")
	require.False(t, privs.Authorized(authz.ActionFind, authz.CollectionResource("other", "c")))

	_, err = h.MsgDropRole(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"dropRole", "root", "$db", "test",
	)))))
	require.NoError(t, err, "a custom role named root outside admin is droppable")
}

func TestCreateRoleRejectsBuiltinNameOnItsDatabase(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	for _, c := range []struct{ db, role string }{{"test", "read"}, {"test", "dbOwner"}, {"admin", "root"}, {"admin", "clusterAdmin"}} {
		err := createRole(ctx, h, c.db, c.role, nil, nil)
		require.True(t, errorHasCode(err, handlererrors.ErrBadValue), "%s@%s: %v", c.role, c.db, err)
	}
}
