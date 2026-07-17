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

// peerCtx returns a context carrying a conninfo whose peer is the given address.
func peerCtx(addr string) context.Context {
	ci := conninfo.New()
	ci.Peer = netip.MustParseAddrPort(addr)
	return conninfo.Ctx(context.Background(), ci)
}

// createUserMsg builds a createUser command for the admin database.
func createUserMsg(t *testing.T, user, pwd string) *wire.OpMsg {
	t.Helper()

	return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"createUser", user,
		"pwd", pwd,
		"roles", types.MakeArray(0),
		"$db", "admin",
	))))
}

// authGateHandler builds a handler with its command wrappers installed so the
// forced-login gate is exercised. EnableNewAuth toggles enforcement.
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

// isForcedLoginError reports whether err is the gate's Unauthorized(13)
// "requires authentication" rejection.
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

func TestAuthGate_ForcedLoginRejectsUnauthenticated(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	// Non-anonymous commands are rejected before their handler runs.
	for _, name := range []string{"listDatabases", "find", "insert", "createUser", "usersInfo"} {
		_, err := h.commands[name].Handler(ctx, gateCmd(t, name))
		require.Error(t, err, "command %q must be gated", name)
		require.True(t, isForcedLoginError(err), "command %q: want Unauthorized(13) requires-authentication, got %v", name, err)
	}
}

func TestAuthGate_AnonymousCommandsStayOpen(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	// Handshake/diagnostic commands must never hit the gate even with
	// enforcement on and no authenticated user.
	for _, name := range []string{"ping", "hello", "saslStart", "saslContinue", "connectionStatus", "logout", "whatsmyuri", "buildInfo"} {
		_, err := h.commands[name].Handler(ctx, gateCmd(t, name))
		require.False(t, isForcedLoginError(err), "anonymous command %q must not be gated, got %v", name, err)
	}
}

func TestAuthGate_LocalhostExceptionBootstrap(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := peerCtx("127.0.0.1:40000")

	// With enforcement on and zero users, a loopback connection may create the
	// first user.
	_, err := h.commands["createUser"].Handler(ctx, createUserMsg(t, "root", "pw"))
	require.NoError(t, err, "localhost exception must permit bootstrapping the first user")
	require.True(t, h.bootstrapLatch.Load(), "exception must latch after a successful bootstrap")

	// A second createUser is rejected: a user now exists and the latch is set.
	_, err = h.commands["createUser"].Handler(ctx, createUserMsg(t, "second", "pw"))
	require.True(t, isForcedLoginError(err), "second createUser must require auth, got %v", err)
}

func TestAuthGate_LocalhostExceptionRejectsNonLoopback(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := peerCtx("10.0.0.5:40000")

	// A non-loopback connection never gets the exception, even with zero users.
	_, err := h.commands["createUser"].Handler(ctx, createUserMsg(t, "root", "pw"))
	require.True(t, isForcedLoginError(err), "non-loopback createUser must be rejected, got %v", err)
	require.False(t, h.bootstrapLatch.Load())
}

func TestAuthGate_LocalhostExceptionLatchIsPermanent(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := peerCtx("127.0.0.1:40000")

	// Once the latch is tripped, the exception is gone even from loopback with
	// zero users.
	h.bootstrapLatch.Store(true)
	_, err := h.commands["createUser"].Handler(ctx, createUserMsg(t, "root", "pw"))
	require.True(t, isForcedLoginError(err), "latched exception must not be reusable, got %v", err)
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

	// Bootstrap a user with a role via the localhost exception.
	bootstrapCtx := peerCtx("127.0.0.1:40000")
	_, err := h.commands["createUser"].Handler(bootstrapCtx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"createUser", "alice",
		"pwd", "pw",
		"roles", must.NotFail(types.NewArray("readWrite")),
		"$db", "admin",
	)))))
	require.NoError(t, err)

	// Present the connection as authenticated as that user.
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

	// With enforcement off and no authenticated user, the gate is a no-op: a
	// non-anonymous command may fail for unrelated reasons but never with the
	// forced-login error.
	_, err := h.commands["listDatabases"].Handler(ctx, gateCmd(t, "listDatabases"))
	require.False(t, isForcedLoginError(err), "gate must be inactive when EnableNewAuth is false, got %v", err)
}
