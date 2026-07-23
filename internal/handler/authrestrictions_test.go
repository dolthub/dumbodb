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

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestAddrMatches(t *testing.T) {
	addr := netip.MustParseAddr("127.0.0.1")
	require.True(t, addrMatches(addr, must.NotFail(types.NewArray("127.0.0.1"))))
	require.True(t, addrMatches(addr, must.NotFail(types.NewArray("10.0.0.1", "127.0.0.1"))))
	require.True(t, addrMatches(addr, must.NotFail(types.NewArray("127.0.0.0/8"))))
	require.False(t, addrMatches(addr, must.NotFail(types.NewArray("10.0.0.1"))))
	require.False(t, addrMatches(netip.Addr{}, must.NotFail(types.NewArray("127.0.0.1"))))
}

func restrictionDoc(key string, values ...string) *types.Document {
	a := types.MakeArray(len(values))
	for _, v := range values {
		a.Append(v)
	}
	return must.NotFail(types.NewDocument(key, a))
}

func TestRestrictionSatisfied(t *testing.T) {
	client := netip.MustParseAddr("127.0.0.1")
	server := netip.MustParseAddr("127.0.0.1")

	require.True(t, restrictionSatisfied(restrictionDoc("clientSource", "127.0.0.1"), client, server))
	require.False(t, restrictionSatisfied(restrictionDoc("clientSource", "10.0.0.1"), client, server))
	require.True(t, restrictionSatisfied(restrictionDoc("serverAddress", "127.0.0.1"), client, server))
	require.False(t, restrictionSatisfied(restrictionDoc("serverAddress", "10.0.0.1"), client, server))
	require.True(t, restrictionSatisfied(must.NotFail(types.NewDocument()), client, server), "empty restriction admits all")

	both := must.NotFail(types.NewDocument(
		"clientSource", must.NotFail(types.NewArray("127.0.0.1")),
		"serverAddress", must.NotFail(types.NewArray("10.0.0.1")),
	))
	require.False(t, restrictionSatisfied(both, client, server), "all present fields must match")
}

func createUserWithRestrictions(ctx context.Context, h *Handler, db, user string, restrictions *types.Array) error {
	_, err := h.MsgCreateUser(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"createUser", user,
		"pwd", "pw",
		"roles", types.MakeArray(0),
		"authenticationRestrictions", restrictions,
		"$db", db,
	)))))
	return err
}

func TestCheckAuthRestrictions(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	restrictions := must.NotFail(types.NewArray(restrictionDoc("clientSource", "127.0.0.1")))
	require.NoError(t, createUserWithRestrictions(ctx, h, "mydb", "restricted", restrictions))

	fromLoopback := conninfo.New()
	fromLoopback.Peer = netip.MustParseAddrPort("127.0.0.1:5000")
	fromLoopback.Local = netip.MustParseAddrPort("127.0.0.1:27017")
	require.NoError(t, h.checkAuthRestrictions(conninfo.Ctx(context.Background(), fromLoopback), "mydb", "restricted"))

	fromElsewhere := conninfo.New()
	fromElsewhere.Peer = netip.MustParseAddrPort("10.0.0.1:5000")
	fromElsewhere.Local = netip.MustParseAddrPort("127.0.0.1:27017")
	err := h.checkAuthRestrictions(conninfo.Ctx(context.Background(), fromElsewhere), "mydb", "restricted")
	require.True(t, errorHasCode(err, handlererrors.ErrAuthenticationFailed), "mismatched clientSource must fail auth, got %v", err)
}

func TestCheckAuthRestrictions_NoneStored(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()

	require.NoError(t, createUser(ctx, h, "mydb", "open", nil))

	ci := conninfo.New()
	ci.Peer = netip.MustParseAddrPort("10.0.0.1:5000")
	require.NoError(t, h.checkAuthRestrictions(conninfo.Ctx(context.Background(), ci), "mydb", "open"),
		"a user with no restrictions authenticates from anywhere")
}
