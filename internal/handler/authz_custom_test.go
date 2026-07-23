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

func TestEffectivePrivileges_IncludesCustomRole(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "customreader",
		must.NotFail(types.NewArray(collPrivilege("mydb", "", "find"))), nil))
	createUserWithRole(t, h, "mydb", "carol", "customreader")

	ci := conninfo.New()
	ci.SetAuth("carol", "", nil, "mydb")
	authCtx := conninfo.Ctx(context.Background(), ci)

	privs, err := h.effectivePrivileges(authCtx)
	require.NoError(t, err)
	require.True(t, privs.Authorized(authz.ActionFind, authz.CollectionResource("mydb", "c")))
	require.False(t, privs.Authorized(authz.ActionInsert, authz.CollectionResource("mydb", "c")))
}

func TestEffectivePrivileges_CustomRoleInheritsBuiltin(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "wraps_read", nil,
		must.NotFail(types.NewArray(must.NotFail(types.NewDocument("role", "read", "db", "mydb"))))))
	createUserWithRole(t, h, "mydb", "dan", "wraps_read")

	ci := conninfo.New()
	ci.SetAuth("dan", "", nil, "mydb")
	authCtx := conninfo.Ctx(context.Background(), ci)

	privs, err := h.effectivePrivileges(authCtx)
	require.NoError(t, err)
	require.True(t, privs.Authorized(authz.ActionFind, authz.CollectionResource("mydb", "c")),
		"a custom role inheriting the built-in read must grant find")
}

func TestGrantRolesToRole_RejectsCycle(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "a", nil, nil))
	require.NoError(t, createRole(ctx, h, "mydb", "b", nil,
		must.NotFail(types.NewArray(must.NotFail(types.NewDocument("role", "a", "db", "mydb"))))))

	_, err := h.MsgGrantRolesToRole(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"grantRolesToRole", "a",
		"roles", must.NotFail(types.NewArray("b")),
		"$db", "mydb",
	)))))
	require.True(t, errorHasCode(err, handlererrors.ErrInvalidRoleModification),
		"granting b (which inherits a) to a must be a cycle, got %v", err)
}

func TestDropRole_CascadesToInheritors(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createRole(ctx, h, "mydb", "base", nil, nil))
	require.NoError(t, createRole(ctx, h, "mydb", "derived", nil,
		must.NotFail(types.NewArray(must.NotFail(types.NewDocument("role", "base", "db", "mydb"))))))

	require.NoError(t, dropRole(ctx, h, "mydb", "base"))

	doc, err := h.loadRoleDoc(ctx, "mydb", "derived")
	require.NoError(t, err)
	require.NotNil(t, doc)
	require.Equal(t, 0, must.NotFail(doc.Get("roles")).(*types.Array).Len(),
		"dropRole must remove the dropped role from inheritors")
}
