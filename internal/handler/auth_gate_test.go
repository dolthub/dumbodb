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

func TestAuthGate_DisabledAllowsUnauthenticated(t *testing.T) {
	h := authGateHandler(t, false)
	ctx := conninfo.Ctx(context.Background(), conninfo.New())

	// With enforcement off and no authenticated user, the gate is a no-op: a
	// non-anonymous command may fail for unrelated reasons but never with the
	// forced-login error.
	_, err := h.commands["listDatabases"].Handler(ctx, gateCmd(t, "listDatabases"))
	require.False(t, isForcedLoginError(err), "gate must be inactive when EnableNewAuth is false, got %v", err)
}
