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

package timelib

import (
	"regexp/syntax"
	"unicode"
	"unicode/utf8"
)

// byteSet is a set of bytes.
type byteSet [256]bool

func (s *byteSet) addAll() {
	for i := range s {
		s[i] = true
	}
}

// addRune adds the first byte of r's UTF-8 encoding, and of its other cases
// when the pattern folds case.
func (s *byteSet) addRune(r rune, foldCase bool) {
	s.addFirstByte(r)
	if foldCase {
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			s.addFirstByte(f)
		}
	}
}

func (s *byteSet) addFirstByte(r rune) {
	var buf [utf8.UTFMax]byte
	utf8.EncodeRune(buf[:], r)
	s[buf[0]] = true
}

// firstBytes returns the bytes a non-empty match of pattern can start with,
// so a scanner can skip patterns that cannot match at a position.
func firstBytes(pattern string) byteSet {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		var all byteSet
		all.addAll()
		return all
	}
	var set byteSet
	addFirstBytes(re.Simplify(), &set)
	return set
}

// addFirstBytes adds to set the bytes a match of re can start with and
// reports whether re can match the empty string.
func addFirstBytes(re *syntax.Regexp, set *byteSet) (nullable bool) {
	foldCase := re.Flags&syntax.FoldCase != 0
	switch re.Op {
	case syntax.OpLiteral:
		set.addRune(re.Rune[0], foldCase)
		return false
	case syntax.OpCharClass:
		for i := 0; i+1 < len(re.Rune); i += 2 {
			lo, hi := re.Rune[i], re.Rune[i+1]
			if hi >= utf8.RuneSelf {
				// Any multi-byte first byte; the class is exact for ASCII.
				for b := 0xC0; b < 0x100; b++ {
					set[b] = true
				}
				hi = utf8.RuneSelf - 1
			}
			for r := lo; r <= hi; r++ {
				set.addRune(r, foldCase)
			}
		}
		return false
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		set.addAll()
		return false
	case syntax.OpCapture:
		return addFirstBytes(re.Sub[0], set)
	case syntax.OpStar, syntax.OpQuest:
		addFirstBytes(re.Sub[0], set)
		return true
	case syntax.OpPlus:
		return addFirstBytes(re.Sub[0], set)
	case syntax.OpRepeat:
		nullable := addFirstBytes(re.Sub[0], set)
		return nullable || re.Min == 0
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			if !addFirstBytes(sub, set) {
				return false
			}
		}
		return true
	case syntax.OpAlternate:
		nullable := false
		for _, sub := range re.Sub {
			if addFirstBytes(sub, set) {
				nullable = true
			}
		}
		return nullable
	case syntax.OpNoMatch:
		return false
	default:
		// Empty matches and assertions consume nothing.
		return true
	}
}
