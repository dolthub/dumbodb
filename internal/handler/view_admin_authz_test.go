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

func dbCmd(t *testing.T, db string, pairs ...any) *wire.OpMsg {
	t.Helper()
	return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(append(pairs, "$db", db)...))))
}

// Before the fix the admin write guard compared $db to "admin" literally, so
// the same database addressed as "admin@main" accepted direct writes and
// views.
func TestAdminGuardAppliesToBranchQualifiedAdmin(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "admin", "boss", "root")
	ci := conninfo.New()
	ci.SetAuth("boss", "", nil, "admin")
	ci.SetAuthenticated()
	root := conninfo.Ctx(context.Background(), ci)

	docs := must.NotFail(types.NewArray(must.NotFail(types.NewDocument("_id", "admin.evil"))))
	for _, msg := range []*wire.OpMsg{
		dbCmd(t, "admin@main", "insert", "system.users", "documents", docs),
		dbCmd(t, "admin@main", "create", "v", "viewOn", "system.users", "pipeline", types.MakeArray(0)),
		dbCmd(t, "admin@main", "drop", "system.roles"),
	} {
		name := wireCommandName(msg)
		_, err := h.commands[name].Handler(root, msg)
		_, isGuard := commandErrorCode(err)
		require.True(t, isGuard, "%s on admin@main must hit the admin guard, got %v", name, err)
		require.Contains(t, err.Error(), "admin database is reserved", "%s on admin@main", name)
	}
}

// As in MongoDB, creating a view, or repointing one with collMod, needs find
// on the collection it reads, not only createCollection or collMod; otherwise
// a view is a way to read a collection the creator cannot.
func TestViewsRequireFindOnTheirSource(t *testing.T) {
	h := authGateHandler(t, true)
	creator := userWithPrivileges(t, h, "creator", "createCollection", "collMod")
	reader := userWithPrivileges(t, h, "reader", "createCollection", "collMod", "find")

	pipeline := types.MakeArray(0)
	require.True(t, isUnauthorized(t, h.authorize(creator, writeCmd(t, "create", "v", "viewOn", "secret", "pipeline", pipeline))))
	require.True(t, isUnauthorized(t, h.authorize(creator, writeCmd(t, "collMod", "v", "viewOn", "secret", "pipeline", pipeline))))
	require.NoError(t, h.authorize(creator, writeCmd(t, "create", "plain")), "a plain collection needs no find")

	require.NoError(t, h.authorize(reader, writeCmd(t, "create", "v", "viewOn", "secret", "pipeline", pipeline)))
	require.NoError(t, h.authorize(reader, writeCmd(t, "collMod", "v", "viewOn", "secret", "pipeline", pipeline)))

	out := must.NotFail(types.NewArray(must.NotFail(types.NewDocument("$out", "copy"))))
	require.True(t, isUnauthorized(t, h.authorize(reader, writeCmd(t, "create", "v2", "viewOn", "secret", "pipeline", out))),
		"the view pipeline's stage privileges are checked too")
}
