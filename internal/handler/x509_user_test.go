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

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func createX509UserMsg(t *testing.T, subject string, roles *types.Array) *types.Document {
	t.Helper()

	return must.NotFail(types.NewDocument(
		"createUser", subject,
		"roles", roles,
		"$db", "$external",
	))
}

func usersInfoMsg(t *testing.T, db string) *types.Document {
	t.Helper()

	return must.NotFail(types.NewDocument(
		"usersInfo", int32(1),
		"showCredentials", true,
		"$db", db,
	))
}

func usersFromResponse(t *testing.T, response *types.Document) *types.Array {
	t.Helper()

	return must.NotFail(response.Get("users")).(*types.Array)
}

func TestCreateExternalUserStoresPasswordlessUser(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()
	certificate, subject := testX509Certificate(t)
	roledb := must.NotFail(types.NewDocument("role", "readWrite", "db", "shop"))

	response, err := h.MsgCreateUser(ctx, must.NotFail(documentOpMsg(createX509UserMsg(
		t, subject, must.NotFail(types.NewArray(roledb)),
	))))
	require.NoError(t, err)
	require.NotNil(t, response)

	stored, err := h.loadUserDoc(ctx, "$external", subject)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, "$external."+subject, must.NotFail(stored.Get("_id")))
	require.Equal(t, "$external", must.NotFail(stored.Get("db")))
	require.Equal(t, subject, must.NotFail(stored.Get("user")))
	require.False(t, stored.Has("credentials"))

	externalResponse, err := h.MsgUsersInfo(ctx, must.NotFail(documentOpMsg(usersInfoMsg(t, "$external"))))
	require.NoError(t, err)
	externalDoc, err := opMsgDocument(externalResponse)
	require.NoError(t, err)
	require.Equal(t, 1, usersFromResponse(t, externalDoc).Len())

	adminResponse, err := h.MsgUsersInfo(ctx, must.NotFail(documentOpMsg(usersInfoMsg(t, "admin"))))
	require.NoError(t, err)
	adminDoc, err := opMsgDocument(adminResponse)
	require.NoError(t, err)
	require.Equal(t, 0, usersFromResponse(t, adminDoc).Len())

	authCtx, ci := x509Context(certificate)
	_, err = h.commands["authenticate"].Handler(authCtx, authenticateMsg(t, nil))
	require.NoError(t, err)
	require.True(t, ci.Authenticated())
}

func TestCreateExternalUserRejectsRoleShorthand(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	_, err := h.MsgCreateUser(ctx, must.NotFail(documentOpMsg(createX509UserMsg(
		t, "CN=shorthand", must.NotFail(types.NewArray("readWrite")),
	))))
	requireCommandCode(t, err, handlererrors.ErrRoleNotFound)
	require.Contains(t, err.Error(), "Could not find role: readWrite@$external")
}
