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

	"github.com/FerretDB/wire"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/authz"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func plainSASLStartMsg(t *testing.T, user string) *wire.OpMsg {
	t.Helper()
	return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"saslStart", int32(1),
		"mechanism", "PLAIN",
		"payload", types.Binary{B: []byte("\x00" + user + "\x00wrong-password")},
		"$db", "admin",
	))))
}

func lowPrivilegeCtx(t *testing.T, h *Handler) (context.Context, *conninfo.ConnInfo) {
	t.Helper()
	createUserWithRole(t, h, "admin", "boss", "root")
	createUserWithRole(t, h, "test", "lowpriv", "read")

	ci := conninfo.New()
	ci.SetAuth("lowpriv", "", nil, "test")
	ci.SetAuthenticated()
	return conninfo.Ctx(context.Background(), ci), ci
}

// Before the fix PLAIN saslStart stored whatever username the client sent
// without checking the password, and left the connection authenticated, so
// any logged-in user could become root.
func TestPLAIN_CannotAssumeAnotherIdentity(t *testing.T) {
	h := authGateHandler(t, true)
	ctx, ci := lowPrivilegeCtx(t, h)

	_, err := h.commands["saslStart"].Handler(ctx, plainSASLStartMsg(t, "boss"))
	code, ok := commandErrorCode(err)
	require.True(t, ok, "want CommandError, got %v", err)
	require.Equal(t, handlererrors.ErrMechanismUnavailable, code)

	user, _, _, db := ci.Auth()
	require.Equal(t, "lowpriv", user)
	require.Equal(t, "test", db)

	_, err = h.commands["createUser"].Handler(ctx, createUserMsg(t, "pwn", "pwn"))
	require.True(t, isUnauthorized(t, err), "createUser must not run as root, got %v", err)

	privs, err := h.effectivePrivileges(ctx)
	require.NoError(t, err)
	require.False(t, privs.Authorized(authz.ActionCreateUser, authz.DatabaseResource("admin")))
}

// The same through hello's speculativeAuthenticate, which runs saslStart.
func TestPLAIN_SpeculativeAuthenticateCannotAssumeIdentity(t *testing.T) {
	h := authGateHandler(t, true)
	ctx, ci := lowPrivilegeCtx(t, h)

	res, err := h.commands["hello"].Handler(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"hello", int32(1),
		"speculativeAuthenticate", must.NotFail(types.NewDocument(
			"saslStart", int32(1),
			"mechanism", "PLAIN",
			"payload", types.Binary{B: []byte("\x00boss\x00wrong-password")},
			"db", "admin",
		)),
		"$db", "admin",
	)))))
	require.NoError(t, err)
	require.False(t, must.NotFail(opMsgDocument(res)).Has("speculativeAuthenticate"))

	user, _, _, _ := ci.Auth()
	require.Equal(t, "lowpriv", user)
}

// Before the fix an unauthenticated client could set any identity with PLAIN
// and read that user's roles and privileges through connectionStatus.
func TestPLAIN_UnauthenticatedCannotProbeUsers(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "admin", "boss", "root")

	ci := conninfo.New()
	ctx := conninfo.Ctx(context.Background(), ci)
	_, _ = h.commands["saslStart"].Handler(ctx, plainSASLStartMsg(t, "boss"))

	user, _, _, _ := ci.Auth()
	require.Empty(t, user)

	res, err := h.commands["connectionStatus"].Handler(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"connectionStatus", int32(1), "showPrivileges", true, "$db", "admin",
	)))))
	require.NoError(t, err)
	authInfo := must.NotFail(must.NotFail(opMsgDocument(res)).Get("authInfo")).(*types.Document)
	require.Equal(t, 0, must.NotFail(authInfo.Get("authenticatedUsers")).(*types.Array).Len())
}
