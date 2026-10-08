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

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Before the fix explain accepted any string-valued inner command and opened
// its collection, while authorization was derived from that inner name. A
// name with no privilege requirement (ping, listCommands, getMore) therefore
// returned query plans and index metadata for any collection. Only the
// commands MongoDB can explain are accepted, with MongoDB's errors.
func TestExplain_OnlyExplainableCommands(t *testing.T) {
	for _, auth := range []bool{true, false} {
		h := authGateHandler(t, auth)
		createUserWithRole(t, h, "x", "xreader", "read")
		ci := conninfo.New()
		ci.SetAuth("xreader", "", nil, "x")
		ci.SetAuthenticated()
		ctx := conninfo.Ctx(context.Background(), ci)

		for name, code := range map[string]handlererrors.ErrorCode{
			"ping":         handlererrors.ErrIllegalOperation,
			"listCommands": handlererrors.ErrIllegalOperation,
			"getMore":      handlererrors.ErrIllegalOperation,
			"killCursors":  handlererrors.ErrIllegalOperation,
			"nosuch":       handlererrors.ErrCommandNotFound,
		} {
			inner := must.NotFail(types.NewDocument(name, "secrets", "filter", types.MakeDocument(0)))
			_, err := h.commands["explain"].Handler(ctx, explainMsg(t, "victim", inner))
			got, ok := commandErrorCode(err)
			require.True(t, ok, "auth=%v %s: want CommandError, got %v", auth, name, err)
			require.Equal(t, code, got, "auth=%v %s: %v", auth, name, err)
		}

		find := must.NotFail(types.NewDocument("find", "c", "filter", types.MakeDocument(0)))
		_, err := h.commands["explain"].Handler(ctx, explainMsg(t, "x", find))
		require.NoError(t, err, "auth=%v: explaining an authorized find", auth)
	}
}
