// Copyright 2021 FerretDB Inc.
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
	"fmt"
	"log/slog"
	"regexp"
	"regexp/syntax"

	"github.com/dlclark/regexp2"

	"github.com/dolthub/docudolt/internal/util/lazyerrors"
)

var (
	// ErrOptionNotImplemented indicates unimplemented regex option.
	ErrOptionNotImplemented = fmt.Errorf("regex: option not implemented")

	// ErrMissingParen indicates missing parentheses in regex expression.
	ErrMissingParen = fmt.Errorf("Regular expression is invalid: missing )")

	// ErrMissingBracket indicates missing terminating ] for character class.
	ErrMissingBracket = fmt.Errorf("Regular expression is invalid: missing terminating ] for character class")

	// ErrInvalidEscape indicates invalid escape errors.
	ErrInvalidEscape = fmt.Errorf("Regular expression is invalid: PCRE does not support \\L, \\l, \\N{name}, \\U, or \\u")

	// ErrMissingTerminator indicates syntax error in subpattern name (missing terminator).
	ErrMissingTerminator = fmt.Errorf("Regular expression is invalid: syntax error in subpattern name (missing terminator)")

	// ErrUnmatchedParentheses indicates unmatched parentheses.
	ErrUnmatchedParentheses = fmt.Errorf("Regular expression is invalid: unmatched parentheses")

	// ErrTrailingBackslash indicates \\ at end of the pattern.
	ErrTrailingBackslash = fmt.Errorf("Regular expression is invalid: \\ at end of pattern")

	// ErrNothingToRepeat indicates invalid regex: nothing to repeat.
	ErrNothingToRepeat = fmt.Errorf("Regular expression is invalid: nothing to repeat")

	// ErrInvalidClassRange indicates that range out of order in character class.
	ErrInvalidClassRange = fmt.Errorf("Regular expression is invalid: range out of order in character class")

	// ErrUnsupportedPerlOp indicates unrecognized character after the grouping sequence start.
	ErrUnsupportedPerlOp = fmt.Errorf("Regular expression is invalid: unrecognized character after (? or (?-")

	// ErrInvalidRepeatSize indicates that the regular expression is too large.
	ErrInvalidRepeatSize = fmt.Errorf("Regular expression is invalid: regular expression is too large")
)

// Matcher is implemented by compiled regular expressions returned by Regex.Compile.
type Matcher interface {
	MatchString(s string) bool
	String() string
}

// regexp2Matcher wraps regexp2.Regexp to implement Matcher.
type regexp2Matcher struct {
	re *regexp2.Regexp
}

func (m *regexp2Matcher) MatchString(s string) bool {
	matched, _ := m.re.MatchString(s)
	return matched
}

func (m *regexp2Matcher) String() string {
	return m.re.String()
}

// Regex represents BSON type Regex.
type Regex struct {
	Pattern string
	Options string
}

// Compile returns a Matcher that can evaluate the regex pattern.
// For patterns that require PCRE features (such as lookaheads) that Go's
// regexp engine does not support, it falls back to the regexp2 engine.
func (r Regex) Compile() (Matcher, error) {
	var opts string
	extendedMode := false
	for _, o := range r.Options {
		switch o {
		case 'i', 'm', 's':
			opts += string(o)
		case 'x':
			extendedMode = true
		default:
			continue
		}
	}

	expr := r.Pattern
	if extendedMode {
		expr = stripExtendedWhitespace(expr)
	}
	if opts != "" {
		expr = "(?" + opts + ")" + expr
	}

	re, err := regexp.Compile(expr)
	if err == nil {
		return re, nil
	}

	syntaxErr, ok := err.(*syntax.Error)
	if !ok {
		return nil, lazyerrors.Error(err)
	}

	//nolint:exhaustive // we don't need to handle all possible errors there
	switch syntaxErr.Code {
	case syntax.ErrInvalidCharRange:
		return nil, ErrInvalidClassRange
	case syntax.ErrInvalidEscape:
		return nil, ErrInvalidEscape
	case syntax.ErrInvalidNamedCapture:
		return nil, ErrMissingTerminator
	case syntax.ErrInvalidRepeatOp:
		return nil, ErrNothingToRepeat
	case syntax.ErrInvalidRepeatSize:
		return nil, ErrInvalidRepeatSize
	case syntax.ErrMissingBracket:
		return nil, ErrMissingBracket
	case syntax.ErrMissingParen:
		return nil, ErrMissingParen
	case syntax.ErrMissingRepeatArgument:
		return nil, ErrNothingToRepeat
	case syntax.ErrTrailingBackslash:
		return nil, ErrTrailingBackslash
	case syntax.ErrUnexpectedParen:
		return nil, ErrUnmatchedParentheses

	case syntax.ErrInvalidPerlOp:
		// Go's regexp engine does not support PCRE features like lookaheads.
		// Fall back to regexp2 which provides PCRE-compatible evaluation.
		re2, err2 := regexp2.Compile(expr, 0)
		if err2 != nil {
			return nil, ErrUnsupportedPerlOp
		}
		return &regexp2Matcher{re: re2}, nil

	default:
		return nil, lazyerrors.Error(syntaxErr)
	}
}

// stripExtendedWhitespace preprocesses a regex pattern for the x flag (extended whitespace mode).
// Unescaped whitespace outside of character classes is removed.
// '#' outside of character classes starts a comment until end of line.
func stripExtendedWhitespace(pattern string) string {
	var result []rune
	runes := []rune(pattern)
	i := 0
	inClass := false

	for i < len(runes) {
		ch := runes[i]

		if ch == '\\' && i+1 < len(runes) {
			result = append(result, ch, runes[i+1])
			i += 2
			continue
		}

		if ch == '[' && !inClass {
			inClass = true
			result = append(result, ch)
			i++
			continue
		}

		if ch == ']' && inClass {
			inClass = false
			result = append(result, ch)
			i++
			continue
		}

		if inClass {
			result = append(result, ch)
			i++
			continue
		}

		if ch == '#' {
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			continue
		}

		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\f' || ch == '\v' {
			i++
			continue
		}

		result = append(result, ch)
		i++
	}

	return string(result)
}

// LogValue implements [slog.LogValuer].
func (r Regex) LogValue() slog.Value {
	return slogValue(r, 1)
}

// check interfaces
var (
	_ slog.LogValuer = Regex{}
)
