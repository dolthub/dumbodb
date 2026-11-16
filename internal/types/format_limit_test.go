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

package types

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestFormatAnyValueLimit(t *testing.T) {
	small := must.NotFail(NewDocument("a", int32(1), "b", must.NotFail(NewArray("x", 2.5))))
	require.Equal(t, FormatAnyValue(small), FormatAnyValueLimit(small, 100))

	big := must.NotFail(NewDocument("pad", strings.Repeat("x", 1<<20), "a", int32(1)))
	cut := FormatAnyValueLimit(big, 64)
	require.Equal(t, `{ pad: "`+strings.Repeat("x", 56)+"...", cut)
}

func TestFormatAnyValueLimitStrings(t *testing.T) {
	doc := must.NotFail(NewDocument("s", strings.Repeat("x", 1000)))
	require.Equal(t, `{ s: "`+strings.Repeat("x", 150)+`..." }`, FormatAnyValueLimit(doc, 1<<20))
	require.Equal(t, `{ s: "`+strings.Repeat("x", 150)+`" }`,
		FormatAnyValueLimit(must.NotFail(NewDocument("s", strings.Repeat("x", 150))), 1<<20))
	odd := "x" + strings.Repeat("\u00e9", 100)
	require.Equal(t, `"x`+strings.Repeat("\u00e9", 74)+`..."`, FormatAnyValueLimit(odd, 1<<20))
}

func TestFormatAnyValueLimitCutsWholeCharacters(t *testing.T) {
	s := strings.Repeat("\u00e9\u4e2d\U0001F600", 20)
	for limit := range 120 {
		cut := FormatAnyValueLimit(s, limit)
		require.True(t, utf8.ValidString(cut), "limit %d: %q", limit, cut)
		require.LessOrEqual(t, len(cut), limit+len("..."))
	}
}

func TestFormatAnyValueFits(t *testing.T) {
	doc := must.NotFail(NewDocument("a", int32(1), "b", must.NotFail(NewArray("x", 2.5))))
	n := len(FormatAnyValue(doc))
	require.True(t, FormatAnyValueFits(doc, n))
	require.False(t, FormatAnyValueFits(doc, n-1))

	big := must.NotFail(NewDocument("pad", strings.Repeat("x", 1<<20)))
	require.True(t, FormatAnyValueFits(big, 1<<10))
	arr := MakeArray(10000)
	for range 10000 {
		arr.Append(strings.Repeat("x", 150))
	}
	require.False(t, FormatAnyValueFits(arr, 1<<20))
}

func TestFormatAnyValueLimitSymbolAndJavaScript(t *testing.T) {
	doc := must.NotFail(NewDocument("s", Symbol("x"), "c", JavaScript("f")))
	require.Equal(t, `{ s: "x", c: f }`, FormatAnyValueLimit(doc, 1<<20))
	require.Equal(t, `"`+strings.Repeat("x", 150)+`..."`, FormatAnyValueLimit(Symbol(strings.Repeat("x", 1000)), 1<<20))
	require.Equal(t, strings.Repeat("x", 70)+"...", FormatAnyValueLimit(JavaScript(strings.Repeat("x", 1000)), 1<<20))
}
