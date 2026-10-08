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
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xdg-go/scram"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// scramProofStep runs saslStart and the proof-bearing saslContinue for user
// on ctx, and returns the client conversation and the server's final message.
func scramProofStep(t *testing.T, h *Handler, ctx context.Context, user, pwd string) (*scram.ClientConversation, string) {
	t.Helper()

	client, err := scram.SHA256.NewClient(user, pwd, "")
	require.NoError(t, err)
	conv := client.NewConversation()

	clientFirst, err := conv.Step("")
	require.NoError(t, err)

	res, err := h.commands["saslStart"].Handler(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"saslStart", int32(1),
		"mechanism", "SCRAM-SHA-256",
		"payload", types.Binary{B: []byte(clientFirst)},
		"$db", "admin",
	)))))
	require.NoError(t, err)
	serverFirst := must.NotFail(must.NotFail(opMsgDocument(res)).Get("payload")).(types.Binary).B

	clientFinal, err := conv.Step(string(serverFirst))
	require.NoError(t, err)

	res, err = h.commands["saslContinue"].Handler(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"saslContinue", int32(1),
		"conversationId", int32(1),
		"payload", types.Binary{B: []byte(clientFinal)},
		"$db", "admin",
	)))))
	if err != nil {
		return conv, ""
	}
	serverFinal := must.NotFail(must.NotFail(opMsgDocument(res)).Get("payload")).(types.Binary).B
	return conv, string(serverFinal)
}

func restrictedPeerCtx(peer string) context.Context {
	ci := conninfo.New()
	ci.Peer = netip.MustParseAddrPort(peer)
	ci.Local = netip.MustParseAddrPort("127.0.0.1:27017")
	return conninfo.Ctx(context.Background(), ci)
}

// Before the fix authenticationRestrictions were checked only on the final,
// empty saslContinue. A client that stopped after the proof step was treated
// as authenticated by the command gate anyway, so the restriction never ran.
func TestSASL_RestrictionCannotBeSkippedByOmittingFinalStep(t *testing.T) {
	h := authGateHandler(t, true)

	bootstrap := peerCtx("127.0.0.1:40000")
	_, err := h.commands["createUser"].Handler(bootstrap, createUserMsg(t, "root", "pw"))
	require.NoError(t, err)

	restrictions := must.NotFail(types.NewArray(restrictionDoc("clientSource", "10.0.0.5")))
	require.NoError(t, createUserWithRestrictions(roleCtx(), h, "admin", "restricted", restrictions))

	ctx := restrictedPeerCtx("10.9.9.9:40000")
	_, _ = scramProofStep(t, h, ctx, "restricted", "pw")

	_, err = h.commands["listCollections"].Handler(ctx, gateCmd(t, "listCollections"))
	require.True(t, isForcedLoginError(err), "a restricted user must not be authenticated from a disallowed address, got %v", err)
	require.False(t, conninfo.Get(ctx).Authenticated())
}

func TestSASL_RestrictionAdmitsAllowedAddressWithoutFinalStep(t *testing.T) {
	h := authGateHandler(t, true)

	bootstrap := peerCtx("127.0.0.1:40000")
	_, err := h.commands["createUser"].Handler(bootstrap, createUserMsg(t, "root", "pw"))
	require.NoError(t, err)

	restrictions := must.NotFail(types.NewArray(restrictionDoc("clientSource", "10.0.0.5")))
	require.NoError(t, createUserWithRestrictions(roleCtx(), h, "admin", "restricted", restrictions))

	ctx := restrictedPeerCtx("10.0.0.5:40000")
	conv, serverFinal := scramProofStep(t, h, ctx, "restricted", "pw")
	_, err = conv.Step(serverFinal)
	require.NoError(t, err, "server signature must verify")
	require.True(t, conninfo.Get(ctx).Authenticated(), "a valid proof from an allowed address authenticates")
}

func TestSASL_WrongPasswordDoesNotAuthenticate(t *testing.T) {
	h := authGateHandler(t, true)

	bootstrap := peerCtx("127.0.0.1:40000")
	_, err := h.commands["createUser"].Handler(bootstrap, createUserMsg(t, "root", "pw"))
	require.NoError(t, err)

	ctx := restrictedPeerCtx("10.0.0.5:40000")
	_, _ = scramProofStep(t, h, ctx, "root", "wrong")

	_, err = h.commands["listCollections"].Handler(ctx, gateCmd(t, "listCollections"))
	require.True(t, isForcedLoginError(err), "got %v", err)
}
