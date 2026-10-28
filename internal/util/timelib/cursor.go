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
	"math"
	"strconv"
	"strings"
)

// cursor walks a token as parse_date.re's helpers walk their char pointer;
// the end of the token, like an embedded NUL, reads as '\0'.
type cursor struct {
	b []byte
	i int
}

func (p *cursor) at(offset int) byte {
	if j := p.i + offset; j < len(p.b) {
		return p.b[j]
	}
	return 0
}

func (p *cursor) c() byte { return p.at(0) }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isAlpha(c byte) bool { return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') }

// strchr reports whether c is in set; like C's strchr, it finds the NUL.
func strchr(set string, c byte) bool {
	return c == 0 || strings.IndexByte(set, c) >= 0
}

func pow10(n int) float64 { return math.Pow(10, float64(n)) }

// getNrEx is timelib_get_nr_ex: skip to a digit, then read up to maxLength.
func (p *cursor) getNrEx(maxLength int) (int64, int) {
	for !isDigit(p.c()) {
		if p.c() == 0 {
			return Unset, 0
		}
		p.i++
	}
	begin := p.i
	for isDigit(p.c()) && p.i-begin < maxLength {
		p.i++
	}
	n, _ := strconv.ParseInt(string(p.b[begin:p.i]), 10, 64)
	return n, p.i - begin
}

func (p *cursor) getNr(maxLength int) int64 {
	n, _ := p.getNrEx(maxLength)
	return n
}

func (p *cursor) skipDaySuffix() {
	if isSpace(p.c()) {
		return
	}
	two := strings.ToLower(string([]byte{p.c(), p.at(1)}))
	if two == "nd" || two == "rd" || two == "st" || two == "th" {
		p.i += 2
	}
}

// getFracNr is timelib_get_frac_nr: the fraction's microseconds, computed
// in double precision and truncated.
func (p *cursor) getFracNr() int64 {
	for p.c() != '.' && p.c() != ':' && !isDigit(p.c()) {
		if p.c() == 0 {
			return Unset
		}
		p.i++
	}
	begin := p.i
	for p.c() == '.' || p.c() == ':' || isDigit(p.c()) {
		p.i++
	}
	return int64(strtod(string(p.b[begin+1:p.i])) * pow10(7-(p.i-begin)))
}

// strtod parses the longest decimal prefix of s, as C's strtod does here.
func strtod(s string) float64 {
	end := 0
	for end < len(s) && isDigit(s[end]) {
		end++
	}
	if end < len(s) && s[end] == '.' {
		end++
		for end < len(s) && isDigit(s[end]) {
			end++
		}
	}
	f, _ := strconv.ParseFloat(strings.TrimSuffix(s[:end], "."), 64)
	return f
}

// getSignedNr is timelib_get_signed_nr.
func (p *cursor) getSignedNr(s *scanner, maxLength int) int64 {
	for !isDigit(p.c()) && p.c() != '+' && p.c() != '-' {
		if p.c() == 0 {
			s.failed = true
			return 0
		}
		p.i++
	}
	var digits strings.Builder
	if p.c() == '+' || p.c() == '-' {
		digits.WriteByte(p.c())
		p.i++
	}
	for !isDigit(p.c()) {
		if p.c() == 0 {
			s.failed = true
			return 0
		}
		p.i++
	}
	for n := 0; isDigit(p.c()) && n < maxLength; n++ {
		digits.WriteByte(p.c())
		p.i++
	}
	n, err := strconv.ParseInt(digits.String(), 10, 64)
	if err != nil {
		s.failed = true
		return 0
	}
	return n
}

func (p *cursor) word(keep func(byte) bool) string {
	begin := p.i
	for p.c() != 0 && keep(p.c()) {
		p.i++
	}
	return string(p.b[begin:p.i])
}

var relativeText = []struct {
	name     string
	behavior int64
	value    int64
}{
	{"first", 0, 1}, {"next", 0, 1}, {"second", 0, 2}, {"third", 0, 3}, {"fourth", 0, 4},
	{"fifth", 0, 5}, {"sixth", 0, 6}, {"seventh", 0, 7}, {"eight", 0, 8}, {"eighth", 0, 8},
	{"ninth", 0, 9}, {"tenth", 0, 10}, {"eleventh", 0, 11}, {"twelfth", 0, 12},
	{"last", 0, -1}, {"previous", 0, -1}, {"this", 1, 0},
}

// getRelativeText is timelib_get_relative_text.
func (p *cursor) getRelativeText() (value, behavior int64) {
	for p.c() == ' ' || p.c() == '\t' || p.c() == '-' || p.c() == '/' {
		p.i++
	}
	w := p.word(isAlpha)
	for _, e := range relativeText {
		if strings.EqualFold(w, e.name) {
			value, behavior = e.value, e.behavior
		}
	}
	return value, behavior
}

var monthNames = map[string]int64{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8,
	"sep": 9, "sept": 9, "oct": 10, "nov": 11, "dec": 12,
	"i": 1, "ii": 2, "iii": 3, "iv": 4, "v": 5, "vi": 6, "vii": 7, "viii": 8, "ix": 9, "x": 10, "xi": 11, "xii": 12,
	"january": 1, "february": 2, "march": 3, "april": 4, "june": 6, "july": 7, "august": 8,
	"september": 9, "october": 10, "november": 11, "december": 12,
}

// lookupMonth is timelib_lookup_month: 0 for an unknown word.
func (p *cursor) lookupMonth() int64 {
	return monthNames[strings.ToLower(p.word(isAlpha))]
}

func (p *cursor) getMonth() int64 {
	for p.c() == ' ' || p.c() == '\t' || p.c() == '-' || p.c() == '.' || p.c() == '/' {
		p.i++
	}
	return p.lookupMonth()
}

func (p *cursor) eatSpaces() {
	for {
		switch {
		case p.c() == ' ' || p.c() == '\t':
			p.i++
		case p.c() == 0xe2 && p.at(1) == 0x80 && p.at(2) == 0xaf:
			p.i += 3
		case p.c() == 0xc2 && p.at(1) == 0xa0:
			p.i += 2
		default:
			return
		}
	}
}

func (p *cursor) eatUntilSeparator() {
	p.i++
	for !strchr(" \t.,:;/-0123456789", p.c()) {
		p.i++
	}
}

type relunit struct {
	name       string
	unit       int
	multiplier int64
}

var relunits = []relunit{
	{"ms", unitMicrosecond, 1000}, {"msec", unitMicrosecond, 1000}, {"msecs", unitMicrosecond, 1000},
	{"millisecond", unitMicrosecond, 1000}, {"milliseconds", unitMicrosecond, 1000},
	{"\xc2\xb5s", unitMicrosecond, 1}, {"usec", unitMicrosecond, 1}, {"usecs", unitMicrosecond, 1},
	{"\xc2\xb5sec", unitMicrosecond, 1}, {"\xc2\xb5secs", unitMicrosecond, 1},
	{"microsecond", unitMicrosecond, 1}, {"microseconds", unitMicrosecond, 1},
	{"sec", unitSecond, 1}, {"secs", unitSecond, 1}, {"second", unitSecond, 1}, {"seconds", unitSecond, 1},
	{"min", unitMinute, 1}, {"mins", unitMinute, 1}, {"minute", unitMinute, 1}, {"minutes", unitMinute, 1},
	{"hour", unitHour, 1}, {"hours", unitHour, 1},
	{"day", unitDay, 1}, {"days", unitDay, 1}, {"week", unitDay, 7}, {"weeks", unitDay, 7},
	{"fortnight", unitDay, 14}, {"fortnights", unitDay, 14}, {"forthnight", unitDay, 14}, {"forthnights", unitDay, 14},
	{"month", unitMonth, 1}, {"months", unitMonth, 1}, {"year", unitYear, 1}, {"years", unitYear, 1},
	{"mondays", unitWeekday, 1}, {"monday", unitWeekday, 1}, {"mon", unitWeekday, 1},
	{"tuesdays", unitWeekday, 2}, {"tuesday", unitWeekday, 2}, {"tue", unitWeekday, 2},
	{"wednesdays", unitWeekday, 3}, {"wednesday", unitWeekday, 3}, {"wed", unitWeekday, 3},
	{"thursdays", unitWeekday, 4}, {"thursday", unitWeekday, 4}, {"thu", unitWeekday, 4},
	{"fridays", unitWeekday, 5}, {"friday", unitWeekday, 5}, {"fri", unitWeekday, 5},
	{"saturdays", unitWeekday, 6}, {"saturday", unitWeekday, 6}, {"sat", unitWeekday, 6},
	{"sundays", unitWeekday, 0}, {"sunday", unitWeekday, 0}, {"sun", unitWeekday, 0},
	{"weekday", unitSpecial, specialWeekday}, {"weekdays", unitSpecial, specialWeekday},
}

// lookupRelunit is timelib_lookup_relunit; nil for an unknown unit.
func (p *cursor) lookupRelunit() *relunit {
	w := p.word(func(c byte) bool { return !strchr(" ,\t;:/.-()", c) })
	for i := range relunits {
		if asciiEqualFold(w, relunits[i].name) {
			return &relunits[i]
		}
	}
	return nil
}

func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 32
		}
		if y >= 'A' && y <= 'Z' {
			y += 32
		}
		if x != y {
			return false
		}
	}
	return true
}

// setRelative is timelib_set_relative.
func (p *cursor) setRelative(s *scanner, amount, behavior int64, keepTime bool) {
	unit := p.lookupRelunit()
	if unit == nil {
		return
	}
	r := &s.t.Relative
	add := func(field *int64) {
		sum := *field + amount*unit.multiplier
		if (amount*unit.multiplier > 0 && sum < *field) || (amount*unit.multiplier < 0 && sum > *field) {
			s.failed = true
		}
		*field = sum
	}
	switch unit.unit {
	case unitMicrosecond:
		add(&r.US)
	case unitSecond:
		add(&r.S)
	case unitMinute:
		add(&r.I)
	case unitHour:
		add(&r.H)
	case unitDay:
		add(&r.D)
	case unitMonth:
		add(&r.M)
	case unitYear:
		add(&r.Y)
	case unitWeekday:
		s.haveWeekdayRelative()
		if !keepTime {
			s.unhaveTime()
		}
		weeks := amount
		if amount > 0 {
			weeks = amount - 1
		}
		r.D += weeks * 7
		r.Weekday = unit.multiplier
		r.WeekdayBehavior = behavior
	case unitSpecial:
		s.haveSpecialRelative()
		if !keepTime {
			s.unhaveTime()
		}
		r.SpecialType = unit.multiplier
		r.SpecialAmount = amount
	}
}

// meridian is timelib_meridian: the hour adjustment for an am/pm marker.
func (p *cursor) meridian(h int64) int64 {
	for !strchr("AaPp", p.c()) {
		p.i++
	}
	var adjust int64
	if p.c() == 'a' || p.c() == 'A' {
		if h == 12 {
			adjust = -12
		}
	} else if h != 12 {
		adjust = 12
	}
	p.i++
	if p.c() == '.' {
		p.i++
	}
	if p.c() == 'M' || p.c() == 'm' {
		p.i++
	}
	if p.c() == '.' {
		p.i++
	}
	return adjust
}

// meridianWithCheck is timelib_meridian_with_check; ok is false when no
// well formed marker follows.
func (p *cursor) meridianWithCheck(h int64) (int64, bool) {
	for p.c() != 0 && !strchr("AaPp", p.c()) {
		p.i++
	}
	if p.c() == 0 {
		return 0, false
	}
	var adjust int64
	if p.c() == 'a' || p.c() == 'A' {
		if h == 12 {
			adjust = -12
		}
	} else if h != 12 {
		adjust = 12
	}
	p.i++
	switch {
	case p.c() == '.':
		p.i++
		if p.c() != 'm' && p.c() != 'M' {
			return 0, false
		}
		p.i++
		if p.c() != '.' {
			return 0, false
		}
		p.i++
	case p.c() == 'm' || p.c() == 'M':
		p.i++
	default:
		return 0, false
	}
	return adjust, true
}

// parseTzCor is timelib_parse_tz_cor: an offset after its sign, in seconds.
func (p *cursor) parseTzCor() (int64, bool) {
	begin := p.i
	for isDigit(p.c()) || p.c() == ':' {
		p.i++
	}
	text := string(p.b[begin:p.i])
	atoi := func(s string) int64 {
		end := 0
		for end < len(s) && isDigit(s[end]) {
			end++
		}
		n, _ := strconv.ParseInt(s[:end], 10, 64)
		return n
	}
	switch len(text) {
	case 1, 2:
		return atoi(text) * 3600, true
	case 3, 4:
		switch {
		case text[1] == ':':
			return atoi(text)*3600 + atoi(text[2:])*60, true
		case text[2] == ':':
			return atoi(text)*3600 + atoi(text[3:])*60, true
		default:
			n := atoi(text)
			return n/100*3600 + n%100*60, true
		}
	case 5:
		if text[2] != ':' {
			return 0, false
		}
		return atoi(text)*3600 + atoi(text[3:])*60, true
	case 6:
		n := atoi(text)
		return n/10000*3600 + n/100%100*60 + n%100, true
	case 8:
		if text[2] != ':' || text[5] != ':' {
			return 0, false
		}
		return atoi(text)*3600 + atoi(text[3:])*60 + atoi(text[6:]), true
	}
	return 0, false
}

// parseTzMinutes is timelib_parse_tz_minutes: a signed offset in minutes.
func (p *cursor) parseTzMinutes(t *Time) (int64, bool) {
	sign := p.c()
	if sign != '+' && sign != '-' {
		return 0, false
	}
	p.i++
	begin := p.i
	for isDigit(p.c()) {
		p.i++
	}
	n, _ := strconv.ParseInt(string(p.b[begin:p.i]), 10, 64)
	t.ZoneType, t.Dst = ZoneOffset, 0
	if sign == '-' {
		n = -n
	}
	return n * 60, true
}

// maxAbbrLen is glibc's _POSIX_TZNAME_MAX; longer words are not looked up.
const maxAbbrLen = 6

// parseZone is timelib_parse_zone with MongoDB's time zone wrapper, which
// never resolves a tz database identifier.
func (p *cursor) parseZone(t *Time) (offset int64, notFound bool) {
	for p.c() == ' ' || p.c() == '\t' || p.c() == '(' {
		p.i++
	}
	if p.c() == 'G' && p.at(1) == 'M' && p.at(2) == 'T' && (p.at(3) == '+' || p.at(3) == '-') {
		p.i += 3
	}
	switch p.c() {
	case '+', '-':
		sign := int64(1)
		if p.c() == '-' {
			sign = -1
		}
		p.i++
		t.ZoneType, t.Dst = ZoneOffset, 0
		cor, ok := p.parseTzCor()
		offset, notFound = sign*cor, !ok
	default:
		w := p.word(func(c byte) bool {
			return isAlpha(c) || isDigit(c) || c == '/' || c == '_' || c == '-' || c == '+'
		})
		if len(w) < maxAbbrLen {
			if entry, ok := lookupAbbreviation(w); ok {
				t.ZoneType = ZoneAbbr
				t.Dst = entry.dst
				t.TzAbbr = strings.ToUpper(w)
				offset = entry.offset - entry.dst*3600
				break
			}
		}
		notFound = true
	}
	for p.c() == ')' {
		p.i++
	}
	return offset, notFound
}
