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

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func usersInfoAuthzMsg(t *testing.T, db string, arg any) *wire.OpMsg {
	t.Helper()
	return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"usersInfo", arg,
		"showCredentials", true,
		"$db", db,
	))))
}

func userRef(user, db string) *types.Document {
	return must.NotFail(types.NewDocument("user", user, "db", db))
}

// Before the fix usersInfo was authorized against $db only, while the
// handler returned users from whatever databases the argument named, so a
// userAdmin of one database could dump every user's SCRAM credentials.
func TestAuthorize_UsersInfoChecksEachRequestedDatabase(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "admin", "boss", "root")
	createUserWithRole(t, h, "test", "tenantAdmin", "userAdmin")
	createUserWithRole(t, h, "admin", "anyAdmin", "userAdminAnyDatabase")

	authAs := func(user, db string) context.Context {
		ci := conninfo.New()
		ci.SetAuth(user, "", nil, db)
		return conninfo.Ctx(context.Background(), ci)
	}
	tenant := authAs("tenantAdmin", "test")

	forAll := must.NotFail(types.NewDocument("forAllDBs", true))
	require.True(t, isUnauthorized(t, h.authorize(tenant, usersInfoAuthzMsg(t, "test", forAll))))
	require.True(t, isUnauthorized(t, h.authorize(tenant, usersInfoAuthzMsg(t, "test", userRef("boss", "admin")))))
	require.True(t, isUnauthorized(t, h.authorize(tenant, usersInfoAuthzMsg(t, "test",
		must.NotFail(types.NewArray(userRef("tenantAdmin", "test"), userRef("boss", "admin")))))))

	require.NoError(t, h.authorize(tenant, usersInfoAuthzMsg(t, "test", int32(1))))
	require.NoError(t, h.authorize(tenant, usersInfoAuthzMsg(t, "test", "someone")))
	require.NoError(t, h.authorize(tenant, usersInfoAuthzMsg(t, "test", userRef("someone", "test"))))
	require.True(t, isUnauthorized(t, h.authorize(tenant, usersInfoAuthzMsg(t, "other", int32(1)))))

	anyAdmin := authAs("anyAdmin", "admin")
	require.NoError(t, h.authorize(anyAdmin, usersInfoAuthzMsg(t, "test", forAll)))
	require.NoError(t, h.authorize(anyAdmin, usersInfoAuthzMsg(t, "test", userRef("boss", "admin"))))

	boss := authAs("boss", "admin")
	require.NoError(t, h.authorize(boss, usersInfoAuthzMsg(t, "admin", forAll)))
}

func TestAuthorize_UsersInfoSelfServiceInAnyForm(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "test", "plain", "read")

	ci := conninfo.New()
	ci.SetAuth("plain", "", nil, "test")
	ctx := conninfo.Ctx(context.Background(), ci)

	require.NoError(t, h.authorize(ctx, usersInfoAuthzMsg(t, "test", "plain")))
	require.NoError(t, h.authorize(ctx, usersInfoAuthzMsg(t, "other", userRef("plain", "test"))))
	require.True(t, isUnauthorized(t, h.authorize(ctx, usersInfoAuthzMsg(t, "test", "someoneElse"))))
	require.True(t, isUnauthorized(t, h.authorize(ctx, usersInfoAuthzMsg(t, "other", "plain"))))
}
