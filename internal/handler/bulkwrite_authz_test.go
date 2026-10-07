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

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// Malformed bulkWrite commands are left for the handler to reject; the
// authorization pass must not panic on them.
func TestAuthorize_BulkWriteMalformedDoesNotPanic(t *testing.T) {
	h := authGateHandler(t, true)
	createUserWithRole(t, h, "appdb", "writer", "readWrite")
	ctx := authCtx("writer", "appdb")

	for _, doc := range []*types.Document{
		must.NotFail(types.NewDocument("bulkWrite", int32(1), "$db", "admin")),
		must.NotFail(types.NewDocument("bulkWrite", int32(1), "ops", "x", "nsInfo", int32(1), "$db", "admin")),
		must.NotFail(types.NewDocument("bulkWrite", int32(1),
			"ops", must.NotFail(types.NewArray(int32(1))),
			"nsInfo", must.NotFail(types.NewArray(int32(1), types.MakeDocument(0))), "$db", "admin")),
		must.NotFail(types.NewDocument("bulkWrite", int32(1),
			"ops", must.NotFail(types.NewArray(must.NotFail(types.NewDocument("insert", int32(5))))),
			"nsInfo", must.NotFail(types.NewArray(must.NotFail(types.NewDocument("ns", "appdb.c")))), "$db", "admin")),
	} {
		require.NotPanics(t, func() {
			_ = h.authorize(ctx, must.NotFail(documentOpMsg(doc)))
		})
	}
}
