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

package index

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestKeyStringSymbolSharesStringKey(t *testing.T) {
	t.Parallel()

	require.Equal(t, EncodeValue("a\x00b"), EncodeValue(types.Symbol("a\x00b")))
	require.NotEqual(t, EncodeValue("abc"), EncodeValue(types.JavaScript("abc")))
	require.False(t, ValueLossy(must.NotFail(types.NewArray(types.Symbol("x"), types.JavaScript("y")))))
	require.Equal(t, EncodeID(types.Symbol("abc")), EncodeID("abc"))
}

func TestKeyStringSymbolJavaScriptBrackets(t *testing.T) {
	t.Parallel()

	for _, v := range []any{"", "zzz", types.Symbol(""), types.Symbol("zzz")} {
		start, stop, ok := BracketRange(types.Symbol("m"))
		require.True(t, ok)
		key := EncodeValue(v)
		require.True(t, string(key) >= string(start) && string(key) < string(stop), "%v", v)
	}
	start, stop, ok := BracketRange(types.JavaScript("m"))
	require.True(t, ok)
	for _, v := range []any{types.JavaScript(""), types.JavaScript("\xff\xff")} {
		key := EncodeValue(v)
		require.True(t, string(key) >= string(start) && string(key) < string(stop), "%v", v)
	}
	for _, v := range []any{"zzz", types.Regex{Pattern: "\xff"}, types.MaxKey} {
		key := EncodeValue(v)
		require.False(t, string(key) >= string(start) && string(key) < string(stop), "%v", v)
	}
}

func TestKeyStringSymbolJavaScriptDecode(t *testing.T) {
	t.Parallel()

	for _, v := range []any{types.Symbol("a\x00b"), types.JavaScript("function () {}"), types.JavaScript("")} {
		values := []any{v, int32(1)}
		split, ok := SplitKeyValues(append(EncodeValue(v), EncodeValue(int32(1))...), 2)
		require.True(t, ok)
		tags := TypeTags(values)
		got, ok := DecodeValue(split[0], tags[0])
		require.True(t, ok)
		require.Equal(t, v, got)
	}
	_, ok := DecodeValue(EncodeValue(int32(1)), TagSymbol)
	require.False(t, ok)
}

func TestCheckCollatable(t *testing.T) {
	t.Parallel()

	require.NoError(t, CheckCollatable("v", "abc"))
	require.NoError(t, CheckCollatable("v", types.JavaScript("abc")))

	err := CheckCollatable("v", types.Symbol("ABC"))
	require.True(t, errors.Is(err, ErrSymbolWithCollation))
	require.Equal(t, `KeyString format error: Cannot index type Symbol with a collation. Failed to index element: v: "ABC".`, err.Error())

	nested := must.NotFail(types.NewArray(must.NotFail(types.NewDocument("s", types.Symbol("a")))))
	err = CheckCollatable("v", nested)
	require.ErrorContains(t, err, `Failed to index element: s: "a".`)
}
