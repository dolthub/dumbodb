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

func TestMongoCommandString(t *testing.T) {
	// Mirrors the command document MongoDB echoes in an "not authorized ... to
	// execute command <doc>" error, so DumboDB's message carries the same shape.
	uuid := types.Binary{
		Subtype: types.BinaryUUID,
		B: []byte{
			0xc6, 0xe7, 0x21, 0xb5, 0x8d, 0x3c, 0x48, 0xf9,
			0x94, 0x91, 0x51, 0x85, 0x6e, 0x52, 0x2a, 0x26,
		},
	}
	doc := must.NotFail(types.NewDocument(
		"insert", "c",
		"documents", must.NotFail(types.NewArray(must.NotFail(types.NewDocument("x", int32(1))))),
		"lsid", must.NotFail(types.NewDocument("id", uuid)),
		"$db", "cmp_err03",
	))

	want := `{ insert: "c", documents: [ { x: 1 } ], lsid: { id: UUID("c6e721b5-8d3c-48f9-9491-51856e522a26") }, $db: "cmp_err03" }`
	require.Equal(t, want, mongoCommandString(doc))
}

func TestMongoCommandString_Scalars(t *testing.T) {
	require.Equal(t, "{}", mongoCommandString(must.NotFail(types.NewDocument())))
	require.Equal(t, "{}", mongoCommandString(nil))
	require.Equal(t, `{ a: true, b: null, n: 1.5, w: 2.0 }`, mongoCommandString(must.NotFail(types.NewDocument(
		"a", true,
		"b", types.Null,
		"n", 1.5,
		"w", float64(2),
	))))
	require.Equal(t, `{ empty: [] }`, mongoCommandString(must.NotFail(types.NewDocument("empty", must.NotFail(types.NewArray())))))
}
