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

func writeCmd(t *testing.T, pairs ...any) *wire.OpMsg {
	t.Helper()
	return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(append(pairs, "$db", "x")...))))
}

func userWithPrivileges(t *testing.T, h *Handler, user string, actions ...string) context.Context {
	t.Helper()
	ctx := roleCtx()
	require.NoError(t, createRole(ctx, h, "x", user+"Role", must.NotFail(types.NewArray(collPrivilege("x", "", actions...))), nil))
	_, err := h.MsgCreateUser(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"createUser", user, "pwd", "pw", "roles", must.NotFail(types.NewArray(user+"Role")), "$db", "x",
	)))))
	require.NoError(t, err)

	ci := conninfo.New()
	ci.SetAuth(user, "", nil, "x")
	return conninfo.Ctx(context.Background(), ci)
}

// Before the fix write commands were authorized by name only: findAndModify
// with remove:true needed update rather than remove, upserts did not need
// insert, and bypassDocumentValidation was not checked, so a role could
// delete, insert or skip validation without the matching privilege.
func TestAuthorize_WritePayloadPrivileges(t *testing.T) {
	h := authGateHandler(t, true)
	updater := userWithPrivileges(t, h, "updater", "find", "update")
	writer := userWithPrivileges(t, h, "writer", "find", "insert", "update", "remove")

	stmt := func(upsert any) *types.Array {
		s := must.NotFail(types.NewDocument(
			"q", types.MakeDocument(0),
			"u", must.NotFail(types.NewDocument("$set", must.NotFail(types.NewDocument("a", int32(1))))),
		))
		if upsert != nil {
			s.Set("upsert", upsert)
		}
		return must.NotFail(types.NewArray(s))
	}
	set := must.NotFail(types.NewDocument("$set", must.NotFail(types.NewDocument("a", int32(1)))))
	docs := must.NotFail(types.NewArray(must.NotFail(types.NewDocument("_id", int32(1)))))

	denied := map[string]*wire.OpMsg{
		"findAndModify remove":       writeCmd(t, "findAndModify", "c", "query", types.MakeDocument(0), "remove", true),
		"findAndModify upsert":       writeCmd(t, "findAndModify", "c", "query", types.MakeDocument(0), "update", set, "upsert", true),
		"findandmodify alias remove": writeCmd(t, "findandmodify", "c", "query", types.MakeDocument(0), "remove", true),
		"update upsert":              writeCmd(t, "update", "c", "updates", stmt(true)),
		"update numeric upsert":      writeCmd(t, "update", "c", "updates", stmt(int32(1))),
		"update bypass":              writeCmd(t, "update", "c", "updates", stmt(nil), "bypassDocumentValidation", true),
		"findAndModify bypass":       writeCmd(t, "findAndModify", "c", "query", types.MakeDocument(0), "update", set, "bypassDocumentValidation", true),
	}
	for name, msg := range denied {
		require.True(t, isUnauthorized(t, h.authorize(updater, msg)), "%s must be denied without the matching privilege", name)
	}

	require.NoError(t, h.authorize(updater, writeCmd(t, "findAndModify", "c", "query", types.MakeDocument(0), "update", set)))
	require.NoError(t, h.authorize(updater, writeCmd(t, "update", "c", "updates", stmt(false))))

	require.True(t, isUnauthorized(t, h.authorize(writer, writeCmd(t, "insert", "c", "documents", docs, "bypassDocumentValidation", true))))
	require.NoError(t, h.authorize(writer, writeCmd(t, "insert", "c", "documents", docs)))
	require.NoError(t, h.authorize(writer, writeCmd(t, "findAndModify", "c", "query", types.MakeDocument(0), "remove", true)))
	require.NoError(t, h.authorize(writer, writeCmd(t, "update", "c", "updates", stmt(true))))
}
