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

package pcre

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/util/lineartest"
)

func TestTranslateMatches(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		pattern string
		opts    Options
		input   string
		want    []string // every non-overlapping match
	}{
		"DigitIsASCII":          {pattern: `\d+`, input: "12\u0663", want: []string{"12"}},
		"WordIsASCII":           {pattern: `\w+`, input: "caf\u00E9", want: []string{"caf"}},
		"SpaceHasVerticalTab":   {pattern: `\s`, input: "a\vb", want: []string{"\v"}},
		"WordBoundaryASCII":     {pattern: `\bb`, input: "\u00E9b b", want: []string{"b", "b"}},
		"ClassShorthands":       {pattern: `[\d\s]+`, input: "a1 2b", want: []string{"1 2"}},
		"Possessive":            {pattern: `a++b`, input: "aaab", want: []string{"aaab"}},
		"PossessiveFails":       {pattern: `a*+a`, input: "aaa", want: nil},
		"PossessiveGroup":       {pattern: `(?:ab)++c`, input: "ababc", want: []string{"ababc"}},
		"POSIXClass":            {pattern: `[[:upper:][:digit:]]+`, input: "aB1c", want: []string{"B1"}},
		"Quoted":                {pattern: `\Qa.b\E+`, input: "a.bb axb", want: []string{"a.bb"}},
		"HexBrace":              {pattern: `\x{e9}`, input: "caf\u00E9", want: []string{"\u00E9"}},
		"UnicodeProperty":       {pattern: `\p{Lu}`, input: "a\u00C9", want: []string{"\u00C9"}},
		"NamedBackreference":    {pattern: `(?<c>\w)\k<c>`, input: "abbc", want: []string{"bb"}},
		"PythonNamedGroup":      {pattern: `(?P<c>\w)(?P=c)`, input: "abbc", want: []string{"bb"}},
		"RelativeBackreference": {pattern: `(a)(b)\g{-1}`, input: "abb", want: []string{"abb"}},
		"LiteralBrace":          {pattern: `a{x}`, input: "a{x}", want: []string{"a{x}"}},
		"BraceQuantifier":       {pattern: `a{2,}`, input: "aaa", want: []string{"aaa"}},
		"Extended":              {pattern: "a b # comment\n[ ]c", opts: Options{Extended: true}, input: "ab c", want: []string{"ab c"}},
		"InlineExtended":        {pattern: "(?x) a b", input: "ab", want: []string{"ab"}},
		"Comment":               {pattern: `a(?#note)b`, input: "ab", want: []string{"ab"}},
		"DollarBeforeNewline":   {pattern: `a$`, input: "a\n", want: []string{"a"}},
		"CaseInsensitive":       {pattern: "\u00E9", opts: Options{CaseInsensitive: true}, input: "\u00C9", want: []string{"\u00C9"}},
		"DotAll":                {pattern: `a.b`, opts: Options{DotAll: true}, input: "a\nb", want: []string{"a\nb"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			re, err := Compile(tc.pattern, tc.opts)
			require.NoError(t, err)

			var got []string
			m, err := re.FindStringMatch(tc.input)
			for ; m != nil && err == nil; m, err = re.FindNextMatch(m) {
				got = append(got, m.String())
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNamedGroupsNumberLeftToRight(t *testing.T) {
	t.Parallel()

	re, err := Compile(`(a)(?<n>b)(c)`, Options{})
	require.NoError(t, err)

	m, err := re.FindStringMatch("abc")
	require.NoError(t, err)

	var captures []string
	for _, g := range m.Groups()[1:] {
		captures = append(captures, g.String())
	}
	assert.Equal(t, []string{"a", "b", "c"}, captures)
}

func TestTranslateErrors(t *testing.T) {
	t.Parallel()

	for name, pattern := range map[string]string{
		"TrailingBackslash":  `a\`,
		"UnterminatedClass":  `[a`,
		"UnknownPOSIXClass":  `[[:nope:]]`,
		"UnknownGroupName":   `\k<x>`,
		"MissingGroupNumber": `\g{-1}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := Compile(pattern, Options{})
			assert.Error(t, err)
		})
	}
}

func TestTranslateIsLinear(t *testing.T) {
	t.Parallel()

	for name, pattern := range map[string]func(n int) string{
		"NonCapturing": func(n int) string { return strings.Repeat("(?:a)", n) },
		"Braces":       func(n int) string { return strings.Repeat("{", n) },
		"Possessive":   func(n int) string { return strings.Repeat("a++", n) },
		"HexBraces":    func(n int) string { return strings.Repeat(`\x{41}`, n) },
		"NamedGroups":  func(n int) string { return "(?<n>a)" + strings.Repeat(`\k<n>`, n) },
		"Unclosed":     func(n int) string { return strings.Repeat("(?<", n) },
		"POSIX":        func(n int) string { return "[" + strings.Repeat("[:", n) + "]" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			lineartest.Assert(t, 1000, func(n int) { _, _ = Translate(pattern(n), false) })
		})
	}
}
