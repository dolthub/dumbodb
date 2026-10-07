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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Before the fix authorization read dropTarget only as a bool, while the
// handler also treats nonzero numbers as true, so dropTarget:1 overwrote the
// target without the dropCollection privilege.
func TestAuthorize_RenameDropTargetRequiresDropCollectionForEveryTruthyForm(t *testing.T) {
	h := authGateHandler(t, true)
	ctx := roleCtx()
	require.NoError(t, createRole(ctx, h, "x", "renamer",
		must.NotFail(types.NewArray(collPrivilege("x", "", "renameCollectionSameDB"))), nil))
	_, err := h.MsgCreateUser(ctx, must.NotFail(documentOpMsg(must.NotFail(types.NewDocument(
		"createUser", "r", "pwd", "pw", "roles", must.NotFail(types.NewArray("renamer")), "$db", "x",
	)))))
	require.NoError(t, err)

	ci := conninfo.New()
	ci.SetAuth("r", "", nil, "x")
	renamer := conninfo.Ctx(context.Background(), ci)

	for _, v := range []any{true, int32(1), int64(1), float64(1), float64(0.5), int32(-1), "yes"} {
		msg := renameMsg(t, "admin", "x.a", "x.b", "dropTarget", v)
		require.True(t, isUnauthorized(t, h.authorize(renamer, msg)), "dropTarget %T(%v) must need dropCollection", v, v)
	}
	for _, v := range []any{false, int32(0), int64(0), float64(0), types.Null} {
		msg := renameMsg(t, "admin", "x.a", "x.b", "dropTarget", v)
		require.NoError(t, h.authorize(renamer, msg), fmt.Sprintf("dropTarget %T(%v) is not a drop", v, v))
	}
	require.NoError(t, h.authorize(renamer, renameMsg(t, "admin", "x.a", "x.b")))
}
