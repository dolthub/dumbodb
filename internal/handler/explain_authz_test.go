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

	"github.com/FerretDB/wire"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func explainMsg(t *testing.T, db string, inner *types.Document) *wire.OpMsg {
	t.Helper()
	return must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"explain", inner,
		"verbosity", "executionStats",
		"$db", db,
	))))
}

// Before the fix explain had no authorization at all, and executionStats
// runs the wrapped query, so its nReturned was an oracle over any collection.
func TestAuthorize_ExplainRequiresWrappedCommandPrivileges(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "x", "xreader", "read")

	x := authCtx("xreader", "x")
	findIn := func(coll string) *types.Document {
		return must.NotFail(types.NewDocument("find", coll, "filter", types.MakeDocument(0)))
	}

	require.NoError(t, h.authorize(x, explainMsg(t, "x", findIn("c"))))
	require.True(t, isUnauthorized(t, h.authorize(x, explainMsg(t, "victim", findIn("c")))))
	require.True(t, isUnauthorized(t, h.authorize(x, explainMsg(t, "admin", findIn("system.users")))))
	require.True(t, isUnauthorized(t, h.authorize(x, explainMsg(t, "x",
		must.NotFail(types.NewDocument("delete", "c", "deletes", types.MakeArray(0)))))))

	out := must.NotFail(types.NewArray(must.NotFail(types.NewDocument("$out", "copy"))))
	require.True(t, isUnauthorized(t, h.authorize(x, explainMsg(t, "x",
		must.NotFail(types.NewDocument("aggregate", "c", "pipeline", out, "cursor", types.MakeDocument(0)))))),
		"stage privileges of the wrapped pipeline must be checked")
}

// Before the fix these commands were missing from the privilege table, and
// a missing entry is allowed for any authenticated user.
func TestAuthorize_PreviouslyUnmappedCommandsRequirePrivileges(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "x", "xreader", "read")
	createUserWithRole(t, h, "admin", "boss", "root")

	x := authCtx("xreader", "x")
	boss := authCtx("boss", "admin")

	cases := []*wire.OpMsg{
		authzCmd(t, "compact", "x", "c"),
		authzCmd(t, "createSearchIndexes", "x", "c"),
		authzCmd(t, "dropSearchIndex", "x", "c"),
		authzCmd(t, "updateSearchIndex", "x", "c"),
		gateCmd(t, "autoCompact"),
		gateCmd(t, "currentOp"),
		gateCmd(t, "getCmdLineOpts"),
	}
	for _, msg := range cases {
		name := wireCommandName(msg)
		require.True(t, isUnauthorized(t, h.authorize(x, msg)), "%s must require a privilege", name)
		require.NoError(t, h.authorize(boss, msg), "%s must be allowed for root", name)
	}

	require.NoError(t, h.authorize(x, authzCmd(t, "listSearchIndexes", "x", "c")))
	require.True(t, isUnauthorized(t, h.authorize(x, authzCmd(t, "listSearchIndexes", "victim", "c"))))
}
