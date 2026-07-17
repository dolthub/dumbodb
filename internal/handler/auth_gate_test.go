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
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"

	"github.com/FerretDB/wire"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func peerCtx(addr string) context.Context {
	ci := conninfo.New()
	ci.Peer = netip.MustParseAddrPort(addr)
	return conninfo.Ctx(context.Background(), ci)
}

func createUserMsg(t *testing.T, user, pwd string) *wire.OpMsg {
	t.Helper()

	return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"createUser", user,
		"pwd", pwd,
		"roles", types.MakeArray(0),
		"$db", "admin",
	))))
}

func authGateHandler(t *testing.T, enableNewAuth bool) *Handler {
	t.Helper()

	be, err := dolt.NewBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	h := &Handler{
		NewOpts: &NewOpts{
			Backend:       be,
			L:             slog.New(slog.NewTextHandler(io.Discard, nil)),
			EnableNewAuth: enableNewAuth,
		},
		b: be,
	}
	h.initCommands()

	return h
}

func isForcedLoginError(err error) bool {
	var ce *handlererrors.CommandError
	if !errors.As(err, &ce) {
		return false
	}
	return ce.Code() == handlererrors.ErrUnauthorized && strings.Contains(ce.Error(), "requires authentication")
}

func gateCmd(t *testing.T, name string, extra ...any) *wire.OpMsg {
	t.Helper()

	pairs := append([]any{name, int32(1), "$db", "admin"}, extra...)
	return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(pairs...))))
}

func collCmd(t *testing.T, name, coll string) *wire.OpMsg {
	t.Helper()

	return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(name, coll, "$db", "admin"))))
}

func commandErrorCode(err error) (handlererrors.ErrorCode, bool) {
	var ce *handlererrors.CommandError
	if !errors.As(err, &ce) {
		return 0, false
	}
	return ce.Code(), true
}

func TestGuardAdminMutation(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		code handlererrors.ErrorCode
	}{
		{"insert", handlererrors.ErrUnauthorized},
		{"update", handlererrors.ErrUnauthorized},
		{"delete", handlererrors.ErrUnauthorized},
		{"findAndModify", handlererrors.ErrUnauthorized},
		{"create", handlererrors.ErrUnauthorized},
		{"drop", handlererrors.ErrIllegalOperation},
	} {
		// The whole admin database is reserved: system and non-system collections.
		for _, coll := range []string{"system.users", "widgets"} {
			err := guardAdminMutation(tc.cmd, "admin", coll)
			require.Error(t, err, "%s on admin.%s must be rejected", tc.cmd, coll)
			code, ok := commandErrorCode(err)
			require.True(t, ok, "%s: want CommandError, got %v", tc.cmd, err)
			require.Equal(t, tc.code, code, "%s on admin.%s: wrong error code", tc.cmd, coll)
		}
	}

	// User databases are not guarded, including their own system.* collections.
	require.NoError(t, guardAdminMutation("insert", "mydb", "system.foobar"))
	require.NoError(t, guardAdminMutation("drop", "mydb", "widgets"))

	// Reads of admin collections are not mutations and are not guarded.
	require.NoError(t, guardAdminMutation("find", "admin", "system.users"))
	require.NoError(t, guardAdminMutation("aggregate", "admin", "widgets"))
}

func TestGuardAdminMutation_WiredAfterAuthGate(t *testing.T) {
	// With enforcement off, an unauthenticated connection passes the gate and
	// reaches the guard, proving the guard is wired into dispatch.
	h := authGateHandler(t, false)
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	_, err := h.commands["drop"].Handler(ctx, collCmd(t, "drop", "system.users"))
	code, ok := commandErrorCode(err)
	require.True(t, ok, "want CommandError, got %v", err)
	require.Equal(t, handlererrors.ErrIllegalOperation, code)

	// With enforcement on, the auth gate fires before the guard: an
	// unauthenticated system.* mutation is a forced-login rejection.
	hEnforced := authGateHandler(t, true)
	_, err = hEnforced.commands["insert"].Handler(ctx, collCmd(t, "insert", "system.users"))
	require.True(t, isForcedLoginError(err), "auth gate must precede the system guard, got %v", err)
}

func TestAuthGate_ForcedLoginRejectsUnauthenticated(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	for _, name := range []string{"listDatabases", "find", "insert", "createUser", "usersInfo"} {
		_, err := h.commands[name].Handler(ctx, gateCmd(t, name))
		require.Error(t, err, "command %q must be gated", name)
		require.True(t, isForcedLoginError(err), "command %q: want Unauthorized(13) requires-authentication, got %v", name, err)
	}
}

func TestAuthGate_AnonymousCommandsStayOpen(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	for _, name := range []string{"ping", "hello", "saslStart", "saslContinue", "connectionStatus", "logout", "whatsmyuri", "buildInfo"} {
		_, err := h.commands[name].Handler(ctx, gateCmd(t, name))
		require.False(t, isForcedLoginError(err), "anonymous command %q must not be gated, got %v", name, err)
	}
}

func TestAuthGate_LocalhostExceptionBootstrap(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := peerCtx("127.0.0.1:40000")

	_, err := h.commands["createUser"].Handler(ctx, createUserMsg(t, "root", "pw"))
	require.NoError(t, err, "localhost exception must permit bootstrapping the first user")
	require.True(t, h.bootstrapLatch.Load(), "exception must latch after a successful bootstrap")

	_, err = h.commands["createUser"].Handler(ctx, createUserMsg(t, "second", "pw"))
	require.True(t, isForcedLoginError(err), "second createUser must require auth, got %v", err)
}

func TestAuthGate_LocalhostExceptionRejectsNonLoopback(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := peerCtx("10.0.0.5:40000")

	_, err := h.commands["createUser"].Handler(ctx, createUserMsg(t, "root", "pw"))
	require.True(t, isForcedLoginError(err), "non-loopback createUser must be rejected, got %v", err)
	require.False(t, h.bootstrapLatch.Load())
}

func TestAuthGate_LocalhostExceptionLatchIsPermanent(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := peerCtx("127.0.0.1:40000")

	h.bootstrapLatch.Store(true)
	_, err := h.commands["createUser"].Handler(ctx, createUserMsg(t, "root", "pw"))
	require.True(t, isForcedLoginError(err), "latched exception must not be reusable, got %v", err)
}

func saslStartMsg(t *testing.T) *wire.OpMsg {
	t.Helper()

	return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"saslStart", int32(1),
		"mechanism", "SCRAM-SHA-256",
		"payload", types.Binary{B: []byte("n,,n=u,r=abcdefgh")},
		"$db", "admin",
	))))
}

func saslContinueMsg(t *testing.T) *wire.OpMsg {
	t.Helper()

	return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"saslContinue", int32(1),
		"conversationId", int32(1),
		"payload", types.Binary{B: []byte("c=biws,r=abcdefgh,p=proof")},
		"$db", "admin",
	))))
}

func TestSASLStart_SecondAuthBeginsButDoesNotAdoptIdentity(t *testing.T) {
	h := authGateHandler(t, true)

	ci := conninfo.New()
	ci.SetAuth("alice", "", nil, "admin")
	ci.SetSCRAMAuthenticated()
	ctx := conninfo.Ctx(context.Background(), ci)

	_, err := h.commands["saslStart"].Handler(ctx, saslStartMsg(t))
	require.NoError(t, err)
	require.True(t, ci.ReauthPending())

	user, _, _, _ := ci.Auth()
	require.Equal(t, "alice", user)
}

func TestSASLContinue_RejectsPendingReauthAndKeepsFirstUser(t *testing.T) {
	h := authGateHandler(t, true)

	ci := conninfo.New()
	ci.SetAuth("alice", "", nil, "admin")
	ci.SetSCRAMAuthenticated()
	ci.SetReauthPending(true)
	ctx := conninfo.Ctx(context.Background(), ci)

	_, err := h.commands["saslContinue"].Handler(ctx, saslContinueMsg(t))
	code, ok := commandErrorCode(err)
	require.True(t, ok, "want CommandError, got %v", err)
	require.Equal(t, handlererrors.ErrAuthenticationFailed, code)

	require.False(t, ci.ReauthPending())
	require.True(t, ci.SCRAMAuthenticated())
	user, _, _, _ := ci.Auth()
	require.Equal(t, "alice", user)
}

func TestSASLStart_AllowedOnUnauthenticatedConnection(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	_, err := h.commands["saslStart"].Handler(ctx, saslStartMsg(t))
	require.NoError(t, err)
}

func TestLogout_ClearsAuthAndSCRAMLatch(t *testing.T) {
	h := authGateHandler(t, true)

	ci := conninfo.New()
	ci.SetAuth("alice", "", nil, "admin")
	ci.SetSCRAMAuthenticated()
	ctx := conninfo.Ctx(context.Background(), ci)

	_, err := h.commands["logout"].Handler(ctx, gateCmd(t, "logout"))
	require.NoError(t, err)

	user, _, _, _ := ci.Auth()
	require.Equal(t, "", user, "logout must clear the authenticated user")
	require.False(t, ci.SCRAMAuthenticated(), "logout must clear the scramAuthenticated latch")
}

func TestConnectionStatus_ReportsUserAndStoredRoles(t *testing.T) {
	h := authGateHandler(t, true)

	bootstrapCtx := peerCtx("127.0.0.1:40000")
	_, err := h.commands["createUser"].Handler(bootstrapCtx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"createUser", "alice",
		"pwd", "pw",
		"roles", must.NotFail(types.NewArray("readWrite")),
		"$db", "admin",
	)))))
	require.NoError(t, err)

	ci := conninfo.New()
	ci.SetAuth("alice", "", nil, "admin")
	ctx := conninfo.Ctx(context.Background(), ci)

	res, err := h.commands["connectionStatus"].Handler(ctx, gateCmd(t, "connectionStatus"))
	require.NoError(t, err)

	doc, err := opMsgDocument(res)
	require.NoError(t, err)
	authInfo := must.NotFail(doc.Get("authInfo")).(*types.Document)
	authUsers := must.NotFail(authInfo.Get("authenticatedUsers")).(*types.Array)
	authRoles := must.NotFail(authInfo.Get("authenticatedUserRoles")).(*types.Array)

	require.Equal(t, 1, authUsers.Len(), "authenticatedUsers must list the user")
	require.Equal(t, 1, authRoles.Len(), "authenticatedUserRoles must reflect the stored roles")

	role0 := must.NotFail(authRoles.Get(0)).(*types.Document)
	require.Equal(t, "readWrite", must.NotFail(role0.Get("role")))
	require.Equal(t, "admin", must.NotFail(role0.Get("db")))
}

func TestAuthGate_DisabledAllowsUnauthenticated(t *testing.T) {
	h := authGateHandler(t, false)
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	_, err := h.commands["listDatabases"].Handler(ctx, gateCmd(t, "listDatabases"))
	require.False(t, isForcedLoginError(err), "gate must be inactive when EnableNewAuth is false, got %v", err)
}
