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
	"unicode"
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

// MaxPatternLength is the length in bytes mongod lets PCRE2 compile
// (kMaxPatternLength); maxNesting is PCRE2's PARENS_NEST_LIMIT as mongod
// builds it.
const (
	MaxPatternLength = 32764
	maxNesting       = 250
)

// Compile translates pattern and compiles it.
func Compile(pattern string, opts Options) (*regexp2.Regexp, error) {
	if len(pattern) > MaxPatternLength {
		return nil, fmt.Errorf("pattern string is longer than the limit set by the application")
	}
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

// runeRange is an inclusive range of code points.
type runeRange struct{ lo, hi rune }

// The shorthand and POSIX classes, as PCRE2 defines them without UCP.
var (
	digitSet      = []runeRange{{'0', '9'}}
	wordSet       = []runeRange{{'0', '9'}, {'A', 'Z'}, {'_', '_'}, {'a', 'z'}}
	spaceSet      = []runeRange{{'\t', '\r'}, {' ', ' '}}
	verticalSet   = []runeRange{{'\n', '\r'}, {0x85, 0x85}, {0x2028, 0x2029}}
	horizontalSet = []runeRange{
		{'\t', '\t'}, {' ', ' '}, {0xA0, 0xA0}, {0x1680, 0x1680}, {0x180E, 0x180E},
		{0x2000, 0x200A}, {0x202F, 0x202F}, {0x205F, 0x205F}, {0x3000, 0x3000},
	}

	posixClasses = map[string][]runeRange{
		"alnum":  {{'0', '9'}, {'A', 'Z'}, {'a', 'z'}},
		"alpha":  {{'A', 'Z'}, {'a', 'z'}},
		"ascii":  {{0, 0x7F}},
		"blank":  {{'\t', '\t'}, {' ', ' '}},
		"cntrl":  {{0, 0x1F}, {0x7F, 0x7F}},
		"digit":  digitSet,
		"graph":  {{'!', '~'}},
		"lower":  {{'a', 'z'}},
		"print":  {{' ', '~'}},
		"punct":  {{'!', '/'}, {':', '@'}, {'[', '`'}, {'{', '~'}},
		"space":  spaceSet,
		"upper":  {{'A', 'Z'}},
		"word":   wordSet,
		"xdigit": {{'0', '9'}, {'A', 'F'}, {'a', 'f'}},
	}

	// classEscapes are the shorthand classes; an uppercase letter is the
	// complement of its lowercase one.
	classEscapes = map[rune][]runeRange{
		'd': digitSet, 'w': wordSet, 's': spaceSet, 'h': horizontalSet, 'v': verticalSet,
	}

	word            = classText(wordSet)
	wordBoundary    = `(?:(?<=[` + word + `])(?![` + word + `])|(?<![` + word + `])(?=[` + word + `]))`
	notWordBoundary = `(?:(?<=[` + word + `])(?=[` + word + `])|(?<![` + word + `])(?![` + word + `]))`
)

// classEscapeRanges returns the ranges \c stands for, ok false when c is not
// a shorthand class.
func classEscapeRanges(c rune) ([]runeRange, bool) {
	if set, ok := classEscapes[c]; ok {
		return set, true
	}
	if set, ok := classEscapes[unicode.ToLower(c)]; ok && unicode.IsUpper(c) {
		return complement(set), true
	}
	return nil, false
}

// complement returns the code points not in set, which is sorted and
// non-overlapping.
func complement(set []runeRange) []runeRange {
	var out []runeRange
	next := rune(0)
	for _, r := range set {
		if r.lo > next {
			out = append(out, runeRange{next, r.lo - 1})
		}
		next = r.hi + 1
	}
	if next <= unicode.MaxRune {
		out = append(out, runeRange{next, unicode.MaxRune})
	}
	return out
}

// classText spells set as the inside of a regexp2 character class.
func classText(set []runeRange) string {
	var b strings.Builder
	for _, r := range set {
		b.WriteString(classRune(r.lo))
		if r.hi > r.lo {
			b.WriteByte('-')
			b.WriteString(classRune(r.hi))
		}
	}
	return b.String()
}

// classRune spells r so regexp2 reads it as a literal inside a class.
func classRune(r rune) string {
	switch {
	case r < 0x20 || r == 0x7F:
		return fmt.Sprintf(`\x%02X`, r)
	case strings.ContainsRune(`\]^-[`, r):
		return `\` + string(r)
	case r < 0x80:
		return string(r)
	case r <= 0xFFFF:
		return fmt.Sprintf(`\u%04X`, r)
	default:
		return string(r)
	}
}

// outBuffer collects the translated pattern. Unlike strings.Builder it can
// insert at an earlier position, moving only what follows it.
type outBuffer struct{ b []byte }

func (o *outBuffer) Write(p []byte) (int, error) { o.b = append(o.b, p...); return len(p), nil }
func (o *outBuffer) WriteString(s string)        { o.b = append(o.b, s...) }
func (o *outBuffer) WriteRune(r rune)            { o.b = utf8.AppendRune(o.b, r) }
func (o *outBuffer) WriteByte(c byte) error      { o.b = append(o.b, c); return nil }
func (o *outBuffer) Len() int                    { return len(o.b) }
func (o *outBuffer) String() string              { return string(o.b) }

// insert puts s at byte offset at.
func (o *outBuffer) insert(at int, s string) {
	o.b = append(o.b, s...)
	copy(o.b[at+len(s):], o.b[at:len(o.b)-len(s)])
	copy(o.b[at:], s)
}

// maxPOSIXNameLength bounds the scan for a POSIX class name, the longest
// being "^xdigit".
const maxPOSIXNameLength = 7

// maxNameLength is PCRE2's limit on group names; scans for a name's closing
// delimiter stop there, which keeps translation linear.
const maxNameLength = 32

// hasPrefixAt reports whether in[i:] starts with s.
func hasPrefixAt(in []rune, i int, s string) bool {
	for _, r := range s {
		if i >= len(in) || in[i] != r {
			return false
		}
		i++
	}
	return true
}

// indexWithin returns the index of the first r in in[from:from+limit], or -1.
func indexWithin(in []rune, from int, r rune, limit int) int {
	for i := from; i < len(in) && i < from+limit; i++ {
		if in[i] == r {
			return i
		}
	}
	return -1
}

// indexQuoteEnd returns the index of the \E closing a \Q quote that starts
// at from, or -1.
func indexQuoteEnd(in []rune, from int) int {
	for i := from; i+1 < len(in); i++ {
		if in[i] == '\\' && in[i+1] == 'E' {
			return i
		}
	}
	return -1
}

// translator rewrites a PCRE2 pattern into regexp2 syntax.
type translator struct {
	in       []rune
	pos      int
	out      outBuffer
	extended bool

	groupCount  int
	groupNames  map[string]int
	groupStarts []int

	// atomStart is where the most recent atom begins in out, so a possessive
	// quantifier can wrap it.
	atomStart int

	// assertion is set when the most recent item is a simple assertion,
	// which PCRE2 does not let a quantifier follow.
	assertion bool
}

// Translate rewrites pattern into regexp2 syntax. In extended mode,
// whitespace and comments outside classes are removed.
func Translate(pattern string, extended bool) (string, error) {
	t := &translator{in: []rune(pattern), extended: extended, groupNames: map[string]int{}}
	if err := t.nameGroups(); err != nil {
		return "", err
	}
	for t.pos < len(t.in) {
		if err := t.next(); err != nil {
			return "", err
		}
	}
	return t.out.String(), nil
}

// nameGroups numbers the named groups up front so backreferences to them can
// become numeric, as regexp2 numbers named groups after unnamed ones. It
// skips what is not a group: classes, \Q...\E, (?#...) and, in extended
// mode, # comments.
func (t *translator) nameGroups() error {
	n := 0
	inClass := false
	extended := t.extended
	for i := 0; i < len(t.in); i++ {
		switch c := t.in[i]; {
		case c == '\\' && i+1 < len(t.in) && t.in[i+1] == 'Q':
			end := indexQuoteEnd(t.in, i+2)
			if end < 0 {
				return nil
			}
			i = end + 1
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
		case extended && c == '#':
			for i < len(t.in) && t.in[i] != '\n' {
				i++
			}
		case c == '(' && hasPrefixAt(t.in, i, "(?#"):
			for i < len(t.in) && t.in[i] != ')' {
				i++
			}
		case c == '(':
			if name, _, ok := namedGroupAt(t.in, i); ok {
				if name[0] >= '0' && name[0] <= '9' {
					return fmt.Errorf("subpattern name must start with a non-digit")
				}
				if _, dup := t.groupNames[name]; dup {
					return fmt.Errorf("two named subpatterns have the same name %q", name)
				}
				n++
				t.groupNames[name] = n
			} else if i+1 >= len(t.in) || t.in[i+1] != '?' && t.in[i+1] != '*' {
				n++
			} else if on, _, ok := inlineFlags(t.in, i); ok {
				extended = on
			}
		}
	}
	return nil
}

// inlineFlags parses the flag letters of (?flags) or (?flags: at i and
// reports whether they leave extended mode on.
func inlineFlags(in []rune, i int) (extended bool, length int, ok bool) {
	j := i + 2
	negated := false
	for j < len(in) && strings.ContainsRune("^imsxnU-", in[j]) {
		switch in[j] {
		case '^':
			extended = false
		case '-':
			negated = true
		case 'x':
			extended = !negated
		}
		j++
	}
	if j == i+2 || j >= len(in) || (in[j] != ')' && in[j] != ':') {
		return false, 0, false
	}
	return extended, j - i, true
}

// namedGroupAt recognizes (?<name>, (?P<name> and (?'name' at i and returns
// the name and the length of the opener.
func namedGroupAt(in []rune, i int) (string, int, bool) {
	var open int
	var closer rune
	switch {
	case hasPrefixAt(in, i, "(?P<"):
		open, closer = 4, '>'
	case hasPrefixAt(in, i, "(?<") && !hasPrefixAt(in, i, "(?<=") && !hasPrefixAt(in, i, "(?<!"):
		open, closer = 3, '>'
	case hasPrefixAt(in, i, "(?'"):
		open, closer = 3, '\''
	default:
		return "", 0, false
	}
	end := indexWithin(in, i+open, closer, maxNameLength+1)
	if end <= i+open {
		return "", 0, false
	}
	return string(in[i+open : end]), end + 1 - i, true
}

func (t *translator) peek(offset int) rune {
	if t.pos+offset < len(t.in) {
		return t.in[t.pos+offset]
	}
	return -1
}

func (t *translator) hasPrefix(s string) bool {
	return hasPrefixAt(t.in, t.pos, s)
}

func (t *translator) beginAtom() {
	t.atomStart = t.out.Len()
	t.assertion = false
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
	case c == '^':
		// PCRE2's multiline ^ does not match after a newline that ends the
		// subject; regexp2's does.
		t.beginAtom()
		t.pos++
		t.out.WriteString(`(?:^(?!\z)|\A)`)
		t.assertion = true
	case c == '$':
		t.beginAtom()
		t.pos++
		t.out.WriteRune('$')
		t.assertion = true
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
		if t.assertion {
			return fmt.Errorf("quantifier does not follow a repeatable item")
		}
		t.pos++
		t.out.WriteRune(c)
		t.quantifierSuffix()
	case c == '{':
		if q, ok := t.braceQuantifier(); ok {
			if t.assertion {
				return fmt.Errorf("quantifier does not follow a repeatable item")
			}
			t.pos += utf8.RuneCountInString(q)
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

// skipExtendedSpace skips whitespace and # comments at pos.
func (t *translator) skipExtendedSpace() {
	for t.pos < len(t.in) {
		switch c := t.in[t.pos]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			t.pos++
		case c == '#':
			for t.pos < len(t.in) && t.in[t.pos] != '\n' {
				t.pos++
			}
		default:
			return
		}
	}
}

// braceQuantifier returns {n}, {n,} or {n,m} at pos.
func (t *translator) braceQuantifier() (string, bool) {
	i := t.pos + 1
	for i < len(t.in) && (t.in[i] >= '0' && t.in[i] <= '9' || t.in[i] == ',') {
		i++
	}
	if i >= len(t.in) || t.in[i] != '}' {
		return "", false
	}
	body := string(t.in[t.pos+1 : i])
	lo, hi, hasComma := strings.Cut(body, ",")
	if lo == "" || strings.Contains(hi, ",") || (!hasComma && hi != "") {
		return "", false
	}
	return "{" + body + "}", true
}

// quantifierSuffix handles a lazy '?' or turns a possessive '+' into an
// atomic group around the quantified atom.
func (t *translator) quantifierSuffix() {
	t.assertion = false
	if t.extended {
		t.skipExtendedSpace()
	}
	switch t.peek(0) {
	case '?':
		t.pos++
		t.out.WriteRune('?')
	case '+':
		t.pos++
		t.out.insert(t.atomStart, "(?>")
		t.out.WriteString(")")
	}
}

func (t *translator) openGroup() error {
	start := t.out.Len()
	t.atomStart = start

	if _, n, ok := namedGroupAt(t.in, t.pos); ok {
		t.groupCount++
		return t.pushGroup(start, "(", n)
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
		end := indexWithin(t.in, t.pos+4, ')', maxNameLength+1)
		if end < 0 {
			return fmt.Errorf("missing ) after (?P= name")
		}
		name := string(t.in[t.pos+4 : end])
		t.pos = end + 1
		return t.backreferenceByName(name)
	case t.peek(1) == '?':
		if extended, n, ok := inlineFlags(t.in, t.pos); ok {
			t.extended = extended
			flags := string(t.in[t.pos+2 : t.pos+n])
			terminator := t.in[t.pos+n]
			if terminator == ':' {
				return t.pushGroup(start, dotNetFlags(flags)+":", n+1)
			}
			t.pos += n + 1
			t.out.WriteString(dotNetFlags(flags) + ")")
			return nil
		}
	case t.peek(1) != '*':
		t.groupCount++
	}

	return t.pushGroup(start, "(", 1)
}

// pushGroup opens a group, written as open, that ends at the next ')'.
func (t *translator) pushGroup(start int, open string, consumed int) error {
	if len(t.groupStarts) >= maxNesting {
		return fmt.Errorf("parentheses are too deeply nested")
	}
	t.groupStarts = append(t.groupStarts, start)
	t.out.WriteString(open)
	t.pos += consumed
	return nil
}

// dotNetFlags spells PCRE2 inline flags for regexp2: ^ resets i, m, s and x
// first, and x is applied by the translator, so it is turned off here.
func dotNetFlags(flags string) string {
	on, off := "", "x"
	if rest, reset := strings.CutPrefix(flags, "^"); reset {
		off += "ims"
		flags = rest
	}
	turnOff := false
	for _, f := range flags {
		switch {
		case f == '-':
			turnOff = true
		case f == 'x':
		case turnOff:
			off += string(f)
		default:
			on += string(f)
			off = strings.ReplaceAll(off, string(f), "")
		}
	}
	return "(?" + on + "-" + off
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

	if set, ok := classEscapeRanges(c); ok {
		t.out.WriteString("[" + classText(set) + "]")
		return nil
	}

	switch c {
	case 'R':
		t.out.WriteString(`(?>\r\n|[` + classText(verticalSet) + `])`)
	case 'N':
		if r, ok := t.namedCodePoint(); ok {
			if err := checkCodePoint(r); err != nil {
				return err
			}
			t.out.WriteString(escapeRune(r))
			return nil
		}
		t.out.WriteString(`[^\n]`)
	case 'b':
		t.out.WriteString(wordBoundary)
		t.assertion = true
	case 'B':
		t.out.WriteString(notWordBoundary)
		t.assertion = true
	case 'A', 'z', 'Z', 'G':
		t.out.WriteRune('\\')
		t.out.WriteRune(c)
		t.assertion = true
	case 'Q':
		t.quoted()
	case 'E':
	case 'x':
		r := t.hex()
		if err := checkCodePoint(r); err != nil {
			return err
		}
		t.out.WriteString(escapeRune(r))
	case 'o':
		r, ok := t.octalBrace()
		if !ok {
			return fmt.Errorf(`\o is not followed by {octal digits}`)
		}
		if err := checkCodePoint(r); err != nil {
			return err
		}
		t.out.WriteString(escapeRune(r))
	case 'u', 'U', 'L', 'l':
		return fmt.Errorf(`PCRE2 does not support \%c`, c)
	case 'k':
		open := t.peek(0)
		closer := map[rune]rune{'<': '>', '\'': '\'', '{': '}'}[open]
		if closer == 0 {
			return fmt.Errorf(`\k is not followed by a name`)
		}
		end := indexWithin(t.in, t.pos+1, closer, maxNameLength+1)
		if end < 0 {
			return fmt.Errorf(`\k is not followed by a name`)
		}
		name := string(t.in[t.pos+1 : end])
		t.pos = end + 1
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
		if c != '_' && (unicode.IsLetter(c) || unicode.IsDigit(c)) {
			t.out.WriteRune('\\')
			t.out.WriteRune(c)
		} else {
			// PCRE2 reads an escaped non-alphanumeric character literally.
			t.out.WriteString(escapeRune(c))
		}
	}
	return nil
}

// gReference translates \gN, \g{N}, \g{-N} and \g{name}.
func (t *translator) gReference() error {
	var ref string
	if t.peek(0) == '{' {
		end := indexWithin(t.in, t.pos+1, '}', maxNameLength+1)
		if end < 0 {
			return fmt.Errorf(`\g is not followed by a number or name`)
		}
		ref = string(t.in[t.pos+1 : end])
		t.pos = end + 1
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

// hex reads the hex digits of \x at pos: {hhh}, or up to two digits, none
// meaning NUL.
func (t *translator) hex() rune {
	if r, ok := t.hexBrace(); ok {
		return r
	}
	var r rune
	for i := 0; i < 2; i++ {
		d, err := strconv.ParseUint(string(t.peek(0)), 16, 8)
		if t.peek(0) < 0 || err != nil {
			break
		}
		r = r*16 + rune(d)
		t.pos++
	}
	return r
}

// octalBrace reads the {ooo} of \o{ooo} at pos.
func (t *translator) octalBrace() (rune, bool) {
	return t.bracedCodePoint(0, 8)
}

// namedCodePoint reads the {U+hhhh} of \N{U+hhhh} at pos.
func (t *translator) namedCodePoint() (rune, bool) {
	if !t.hasPrefix("{U+") {
		return 0, false
	}
	return t.bracedCodePoint(2, 16)
}

// hexBrace reads the {hhh} of \x{hhh} at pos.
func (t *translator) hexBrace() (rune, bool) {
	return t.bracedCodePoint(0, 16)
}

// maxCodePointDigits bounds the digits read for a code point; more cannot
// name a valid one, so the scan stays short.
const maxCodePointDigits = 16

// bracedCodePoint reads a code point in base written in braces at pos, its
// digits starting skip runes after the brace. A value outside Unicode is
// returned as is for the caller to reject.
func (t *translator) bracedCodePoint(skip, base int) (rune, bool) {
	if t.peek(0) != '{' {
		return 0, false
	}
	start := t.pos + 1 + skip
	end := indexWithin(t.in, start, '}', maxCodePointDigits+1)
	if end < 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(string(t.in[start:end]), base, 32)
	if err != nil {
		return 0, false
	}
	t.pos = end + 1
	if n > unicode.MaxRune {
		return unicode.MaxRune + 1, true
	}
	return rune(n), true
}

// checkCodePoint returns PCRE2's error for a code point UTF mode does not
// allow: one past U+10FFFF, or a surrogate.
func checkCodePoint(r rune) error {
	switch {
	case r > unicode.MaxRune:
		return fmt.Errorf("character code point value in \\x{} or \\o{} is too large")
	case r >= 0xD800 && r <= 0xDFFF:
		return fmt.Errorf("disallowed Unicode code point (>= 0xd800 && <= 0xdfff)")
	}
	return nil
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
			end := indexWithin(t.in, t.pos+2, ':', maxPOSIXNameLength+1)
			if end < 0 || end+1 >= len(t.in) || t.in[end+1] != ']' {
				t.pos++
				t.out.WriteString(`\[`)
				continue
			}
			name := string(t.in[t.pos+2 : end])
			negate := strings.HasPrefix(name, "^")
			set, ok := posixClasses[strings.TrimPrefix(name, "^")]
			if !ok {
				return fmt.Errorf("unknown POSIX class name %q", name)
			}
			if negate {
				set = complement(set)
			}
			t.pos = end + 2
			t.out.WriteString(classText(set))
		case c == '[' && (t.peek(1) == '.' || t.peek(1) == '='):
			return fmt.Errorf("POSIX collating elements are not supported")
		case c == '[':
			t.pos++
			t.out.WriteString(`\[`)
		case c == '\\':
			e := t.peek(1)
			if e < 0 {
				return fmt.Errorf(`\ at end of pattern`)
			}
			t.pos += 2
			if set, ok := classEscapeRanges(e); ok {
				t.out.WriteString(classText(set))
				continue
			}
			switch e {
			case 'Q':
				t.quoted()
			case 'E':
			case 'x', 'o':
				r, ok := t.hex(), true
				if e == 'o' {
					r, ok = t.octalBrace()
				}
				if !ok {
					return fmt.Errorf(`\o is not followed by {octal digits}`)
				}
				if err := checkCodePoint(r); err != nil {
					return err
				}
				t.out.WriteString(classRune(r))
			case '8', '9', 'g', '_':
				// PCRE2 reads these escapes literally inside a class.
				t.out.WriteString(classRune(e))
			default:
				if unicode.IsLetter(e) || unicode.IsDigit(e) {
					t.out.WriteRune('\\')
					t.out.WriteRune(e)
				} else {
					t.out.WriteString(classRune(e))
				}
			}
		default:
			t.pos++
			t.out.WriteRune(c)
		}
	}
	return fmt.Errorf("missing terminating ] for character class")
}
