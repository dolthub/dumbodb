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

// Package pcre runs PCRE2 patterns, compiled the way MongoDB compiles them
// (UTF mode without UCP), on the regexp2 engine.
//
// regexp2 follows .NET syntax, so patterns are translated first: \d, \w, \s
// and \b become their ASCII PCRE meanings, possessive quantifiers become
// atomic groups, POSIX classes and \Q...\E are expanded, PCRE's \h, \v, \R
// and \N are spelled out, and named groups become plain groups so captures
// are numbered left to right as in PCRE. Negated shorthands inside a class,
// such as [\D], keep regexp2's Unicode meaning.
package pcre

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
)

// Options are MongoDB's regex flags.
type Options struct {
	CaseInsensitive bool // i
	Multiline       bool // m
	DotAll          bool // s
	Extended        bool // x
}

// MatchTimeout bounds one match; regexp2 backtracks and can take exponential
// time where PCRE2 would hit its match limit.
const MatchTimeout = time.Second

// Compile translates pattern and compiles it.
func Compile(pattern string, opts Options) (*regexp2.Regexp, error) {
	translated, err := Translate(pattern, opts.Extended)
	if err != nil {
		return nil, err
	}

	var flags regexp2.RegexOptions
	if opts.CaseInsensitive {
		flags |= regexp2.IgnoreCase
	}
	if opts.Multiline {
		flags |= regexp2.Multiline
	}
	if opts.DotAll {
		flags |= regexp2.Singleline
	}

	re, err := regexp2.Compile(translated, flags)
	if err != nil {
		return nil, err
	}
	re.MatchTimeout = MatchTimeout
	return re, nil
}

const (
	asciiWord  = `a-zA-Z0-9_`
	asciiSpace = `\t\n\x0B\f\r `
	vertical   = `\n\x0B\f\r\x85\u2028\u2029`
	horizontal = `\t \xA0\u1680\u180E\u2000-\u200A\u202F\u205F\u3000`

	wordBoundary    = `(?:(?<=[` + asciiWord + `])(?![` + asciiWord + `])|(?<![` + asciiWord + `])(?=[` + asciiWord + `]))`
	notWordBoundary = `(?:(?<=[` + asciiWord + `])(?=[` + asciiWord + `])|(?<![` + asciiWord + `])(?![` + asciiWord + `]))`
)

// classEscapes are the shorthand classes as they appear inside brackets.
var classEscapes = map[rune]string{
	'd': `0-9`,
	'w': asciiWord,
	's': asciiSpace,
	'h': horizontal,
	'v': vertical,
}

// posixClasses are PCRE's POSIX bracket expressions without UCP.
var posixClasses = map[string]string{
	"alnum":  `a-zA-Z0-9`,
	"alpha":  `a-zA-Z`,
	"ascii":  `\x00-\x7F`,
	"blank":  `\t `,
	"cntrl":  `\x00-\x1F\x7F`,
	"digit":  `0-9`,
	"graph":  `!-~`,
	"lower":  `a-z`,
	"print":  ` -~`,
	"punct":  `!-/:-@\[-` + "`" + `{-~`,
	"space":  asciiSpace,
	"upper":  `A-Z`,
	"word":   asciiWord,
	"xdigit": `0-9A-Fa-f`,
}

// translator rewrites a PCRE2 pattern into regexp2 syntax.
type translator struct {
	in       []rune
	pos      int
	out      strings.Builder
	extended bool

	groupCount  int
	groupNames  map[string]int
	groupStarts []int

	// atomStart is where the most recent atom begins in out, so a possessive
	// quantifier can wrap it.
	atomStart int
}

// Translate rewrites pattern into regexp2 syntax. In extended mode,
// whitespace and comments outside classes are removed.
func Translate(pattern string, extended bool) (string, error) {
	t := &translator{in: []rune(pattern), extended: extended, groupNames: map[string]int{}}
	t.nameGroups()
	for t.pos < len(t.in) {
		if err := t.next(); err != nil {
			return "", err
		}
	}
	return t.out.String(), nil
}

// nameGroups numbers the named groups up front so backreferences to them can
// become numeric, as regexp2 numbers named groups after unnamed ones.
func (t *translator) nameGroups() {
	n := 0
	inClass := false
	for i := 0; i < len(t.in); i++ {
		switch c := t.in[i]; {
		case c == '\\':
			i++
		case inClass:
			inClass = c != ']'
		case c == '[':
			inClass = true
			if i+1 < len(t.in) && t.in[i+1] == '^' {
				i++
			}
			if i+1 < len(t.in) && t.in[i+1] == ']' {
				i++
			}
		case c == '(':
			if name, _, ok := namedGroupAt(t.in, i); ok {
				n++
				t.groupNames[name] = n
			} else if i+1 >= len(t.in) || t.in[i+1] != '?' && t.in[i+1] != '*' {
				n++
			}
		}
	}
}

// namedGroupAt recognizes (?<name>, (?P<name> and (?'name' at i and returns
// the name and the length of the opener.
func namedGroupAt(in []rune, i int) (string, int, bool) {
	rest := string(in[i:])
	var open, close string
	switch {
	case strings.HasPrefix(rest, "(?P<"):
		open, close = "(?P<", ">"
	case strings.HasPrefix(rest, "(?<") && !strings.HasPrefix(rest, "(?<=") && !strings.HasPrefix(rest, "(?<!"):
		open, close = "(?<", ">"
	case strings.HasPrefix(rest, "(?'"):
		open, close = "(?'", "'"
	default:
		return "", 0, false
	}
	end := strings.Index(rest[len(open):], close)
	if end <= 0 {
		return "", 0, false
	}
	name := rest[len(open) : len(open)+end]
	return name, utf8.RuneCountInString(open + name + close), true
}

func (t *translator) peek(offset int) rune {
	if t.pos+offset < len(t.in) {
		return t.in[t.pos+offset]
	}
	return -1
}

func (t *translator) hasPrefix(s string) bool {
	return strings.HasPrefix(string(t.in[t.pos:]), s)
}

func (t *translator) beginAtom() {
	t.atomStart = t.out.Len()
}

func (t *translator) next() error {
	c := t.in[t.pos]
	switch {
	case t.extended && (c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'):
		t.pos++
	case t.extended && c == '#':
		for t.pos < len(t.in) && t.in[t.pos] != '\n' {
			t.pos++
		}
	case c == '\\':
		t.beginAtom()
		return t.escape()
	case c == '[':
		t.beginAtom()
		return t.class()
	case c == '(':
		return t.openGroup()
	case c == ')':
		t.pos++
		if n := len(t.groupStarts); n > 0 {
			t.atomStart = t.groupStarts[n-1]
			t.groupStarts = t.groupStarts[:n-1]
		}
		t.out.WriteRune(')')
	case c == '*' || c == '+' || c == '?':
		t.pos++
		t.out.WriteRune(c)
		t.quantifierSuffix()
	case c == '{':
		if q, ok := t.braceQuantifier(); ok {
			t.pos += len(q)
			t.out.WriteString(q)
			t.quantifierSuffix()
			return nil
		}
		t.beginAtom()
		t.pos++
		t.out.WriteString(`\{`)
	default:
		t.beginAtom()
		t.pos++
		t.out.WriteRune(c)
	}
	return nil
}

// braceQuantifier returns {n}, {n,} or {n,m} at pos.
func (t *translator) braceQuantifier() (string, bool) {
	rest := string(t.in[t.pos:])
	end := strings.IndexByte(rest, '}')
	if end < 0 {
		return "", false
	}
	body := rest[1:end]
	lo, hi, hasComma := strings.Cut(body, ",")
	if lo == "" || strings.Trim(lo, "0123456789") != "" || strings.Trim(hi, "0123456789") != "" {
		return "", false
	}
	if !hasComma && hi != "" {
		return "", false
	}
	return rest[:end+1], true
}

// quantifierSuffix handles a lazy '?' or turns a possessive '+' into an
// atomic group around the quantified atom.
func (t *translator) quantifierSuffix() {
	switch t.peek(0) {
	case '?':
		t.pos++
		t.out.WriteRune('?')
	case '+':
		t.pos++
		s := t.out.String()
		t.out.Reset()
		t.out.WriteString(s[:t.atomStart])
		t.out.WriteString("(?>")
		t.out.WriteString(s[t.atomStart:])
		t.out.WriteString(")")
	}
}

func (t *translator) openGroup() error {
	start := t.out.Len()
	t.atomStart = start

	if _, n, ok := namedGroupAt(t.in, t.pos); ok {
		t.groupCount++
		t.pos += n
		t.groupStarts = append(t.groupStarts, start)
		t.out.WriteRune('(')
		return nil
	}

	switch {
	case t.hasPrefix("(?#"):
		end := t.pos
		for end < len(t.in) && t.in[end] != ')' {
			end++
		}
		t.pos = min(end+1, len(t.in))
		return nil
	case t.hasPrefix("(?P="):
		rest := string(t.in[t.pos+4:])
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return fmt.Errorf("missing ) after (?P= name")
		}
		name := rest[:end]
		t.pos += 4 + utf8.RuneCountInString(name) + 1
		return t.backreferenceByName(name)
	case t.peek(1) == '?':
		// Inline flags may turn on extended mode for the rest of the pattern.
		j := t.pos + 2
		for j < len(t.in) && strings.ContainsRune("imsxnU-", t.in[j]) {
			if t.in[j] == 'x' && !strings.ContainsRune(string(t.in[t.pos+2:j]), '-') {
				t.extended = true
			}
			j++
		}
		if j < len(t.in) && t.in[j] == ')' && j > t.pos+2 {
			t.out.WriteString(string(t.in[t.pos : j+1]))
			t.pos = j + 1
			return nil
		}
	case t.peek(1) != '*':
		t.groupCount++
	}

	t.groupStarts = append(t.groupStarts, start)
	t.out.WriteRune('(')
	t.pos++
	return nil
}

func (t *translator) backreferenceByName(name string) error {
	n, ok := t.groupNames[name]
	if !ok {
		return fmt.Errorf("reference to non-existent subpattern %q", name)
	}
	fmt.Fprintf(&t.out, `(?:\%d)`, n)
	return nil
}

// escape translates the escape sequence at pos outside a class.
func (t *translator) escape() error {
	c := t.peek(1)
	if c < 0 {
		return fmt.Errorf(`\ at end of pattern`)
	}
	t.pos += 2

	switch c {
	case 'd':
		t.out.WriteString(`[0-9]`)
	case 'D':
		t.out.WriteString(`[^0-9]`)
	case 'w':
		t.out.WriteString(`[` + asciiWord + `]`)
	case 'W':
		t.out.WriteString(`[^` + asciiWord + `]`)
	case 's':
		t.out.WriteString(`[` + asciiSpace + `]`)
	case 'S':
		t.out.WriteString(`[^` + asciiSpace + `]`)
	case 'h':
		t.out.WriteString(`[` + horizontal + `]`)
	case 'H':
		t.out.WriteString(`[^` + horizontal + `]`)
	case 'v':
		t.out.WriteString(`[` + vertical + `]`)
	case 'V':
		t.out.WriteString(`[^` + vertical + `]`)
	case 'R':
		t.out.WriteString(`(?>\r\n|[` + vertical + `])`)
	case 'N':
		t.out.WriteString(`[^\n]`)
	case 'b':
		t.out.WriteString(wordBoundary)
	case 'B':
		t.out.WriteString(notWordBoundary)
	case 'Q':
		t.quoted()
	case 'E':
	case 'x':
		r, ok := t.hexBrace()
		if !ok {
			t.out.WriteString(`\x`)
			return nil
		}
		t.out.WriteString(escapeRune(r))
	case 'k':
		open := t.peek(0)
		closer := map[rune]rune{'<': '>', '\'': '\'', '{': '}'}[open]
		if closer == 0 {
			return fmt.Errorf(`\k is not followed by a name`)
		}
		rest := string(t.in[t.pos+1:])
		end := strings.IndexRune(rest, closer)
		if end < 0 {
			return fmt.Errorf(`\k is not followed by a name`)
		}
		name := rest[:end]
		t.pos += 1 + utf8.RuneCountInString(name) + 1
		return t.backreferenceByName(name)
	case 'g':
		return t.gReference()
	case 'p', 'P':
		t.out.WriteRune('\\')
		t.out.WriteRune(c)
		if t.peek(0) == '{' {
			for t.pos < len(t.in) {
				r := t.in[t.pos]
				t.pos++
				t.out.WriteRune(r)
				if r == '}' {
					break
				}
			}
		}
	default:
		t.out.WriteRune('\\')
		t.out.WriteRune(c)
	}
	return nil
}

// gReference translates \gN, \g{N}, \g{-N} and \g{name}.
func (t *translator) gReference() error {
	var ref string
	if t.peek(0) == '{' {
		rest := string(t.in[t.pos+1:])
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			return fmt.Errorf(`\g is not followed by a number or name`)
		}
		ref = rest[:end]
		t.pos += 1 + utf8.RuneCountInString(ref) + 1
	} else {
		start := t.pos
		if t.peek(0) == '-' {
			t.pos++
		}
		for t.pos < len(t.in) && t.in[t.pos] >= '0' && t.in[t.pos] <= '9' {
			t.pos++
		}
		ref = string(t.in[start:t.pos])
	}

	n, err := strconv.Atoi(ref)
	if err != nil {
		return t.backreferenceByName(ref)
	}
	if n < 0 {
		n = t.groupCount + 1 + n
	}
	if n <= 0 {
		return fmt.Errorf("reference to non-existent subpattern")
	}
	fmt.Fprintf(&t.out, `(?:\%d)`, n)
	return nil
}

// hexBrace reads the {hhh} of \x{hhh} at pos.
func (t *translator) hexBrace() (rune, bool) {
	if t.peek(0) != '{' {
		return 0, false
	}
	rest := string(t.in[t.pos+1:])
	end := strings.IndexByte(rest, '}')
	if end < 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(rest[:end], 16, 32)
	if err != nil {
		return 0, false
	}
	t.pos += 1 + end + 1
	return rune(n), true
}

// quoted copies \Q...\E as escaped literals.
func (t *translator) quoted() {
	for t.pos < len(t.in) {
		if t.hasPrefix(`\E`) {
			t.pos += 2
			return
		}
		t.out.WriteString(escapeRune(t.in[t.pos]))
		t.pos++
	}
}

// escapeRune spells r so regexp2 reads it as a literal.
func escapeRune(r rune) string {
	switch {
	case r < 0x20 || r == 0x7F:
		return fmt.Sprintf(`\x%02X`, r)
	case strings.ContainsRune(`\.+*?()|[]{}^$#- `, r):
		return `\` + string(r)
	default:
		return string(r)
	}
}

// class translates a bracketed character class.
func (t *translator) class() error {
	t.pos++
	t.out.WriteRune('[')
	if t.peek(0) == '^' {
		t.pos++
		t.out.WriteRune('^')
	}
	if t.peek(0) == ']' {
		t.pos++
		t.out.WriteString(`\]`)
	}

	for t.pos < len(t.in) {
		c := t.in[t.pos]
		switch {
		case c == ']':
			t.pos++
			t.out.WriteRune(']')
			return nil
		case c == '[' && t.peek(1) == ':':
			rest := string(t.in[t.pos+2:])
			end := strings.Index(rest, ":]")
			if end < 0 {
				t.pos++
				t.out.WriteString(`\[`)
				continue
			}
			name := rest[:end]
			ranges, ok := posixClasses[name]
			if !ok {
				return fmt.Errorf("unknown POSIX class name %q", name)
			}
			t.pos += 2 + len(name) + 2
			t.out.WriteString(ranges)
		case c == '[':
			t.pos++
			t.out.WriteString(`\[`)
		case c == '\\':
			e := t.peek(1)
			if e < 0 {
				return fmt.Errorf(`\ at end of pattern`)
			}
			t.pos += 2
			if ranges, ok := classEscapes[e]; ok {
				t.out.WriteString(ranges)
				continue
			}
			switch e {
			case 'Q':
				t.quoted()
			case 'E':
			case 'x':
				if r, ok := t.hexBrace(); ok {
					t.out.WriteString(escapeRune(r))
				} else {
					t.out.WriteString(`\x`)
				}
			default:
				t.out.WriteRune('\\')
				t.out.WriteRune(e)
			}
		default:
			t.pos++
			t.out.WriteRune(c)
		}
	}
	return fmt.Errorf("missing terminating ] for character class")
}
