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
	"regexp"
	"strings"
)

// The scanner below is timelib_strtotime: parse_date.re's re2c grammar as
// regular expressions, matched longest first with ties going to the earlier
// rule, as the generated scanner does. Its input is padded with NUL bytes,
// which some rules (meridian, datenoyear) match.

const maxFill = 33

// ci matches s case-insensitively in ASCII, as a re2c '...' string does.
func ci(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteString("[" + string(c) + string(c-32) + "]")
		case c >= 'A' && c <= 'Z':
			b.WriteString("[" + string(c+32) + string(c) + "]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

func alt(parts ...string) string { return "(?:" + strings.Join(parts, "|") + ")" }

func ciAlt(words ...string) string {
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = ci(w)
	}
	return alt(parts...)
}

var (
	reSpace    = `(?:[ \t]+|\x{a0}+|\x{202f}+)`
	reFrac     = `\.[0-9]+`
	reHour24   = `(?:[01]?[0-9]|2[0-4])`
	reHour24lz = `(?:[01][0-9]|2[0-4])`
	reHour12   = `(?:0?[1-9]|1[0-2])`
	reMinute   = `(?:[0-5]?[0-9])`
	reMinuteLz = `(?:[0-5][0-9])`
	reSecond   = alt(reMinute, "60")
	reSecondLz = alt(reMinuteLz, "60")
	reMeridian = `(?:[AaPp]\.?[Mm]\.?)[\x00\t ]`
	reTz       = `(?:\(?[A-Za-z]{1,6}\)?|[A-Z][a-z]+(?:[_/-][A-Za-z]+)+)`
	reTzCorr   = `(?:GMT)?[+-]` + alt(reHour24+`(?::?`+reMinute+`)?`, reHour24lz+reMinuteLz+reSecondLz, reHour24lz+`:`+reMinuteLz+`:`+reSecondLz)

	reDaySuf    = `(?:st|nd|rd|th)`
	reMonth     = `(?:0?[0-9]|1[0-2])`
	reDay       = `(?:(?:[0-2]?[0-9]|3[01])` + reDaySuf + `?)`
	reYear      = `[0-9]{1,4}`
	reYear2     = `[0-9]{2}`
	reYear4     = `[0-9]{4}`
	reYear4Sign = `[+-]?[0-9]{4}`
	reYearX     = `[+-][0-9]{5,19}`
	reDayOfYear = `(?:00[1-9]|0[1-9][0-9]|[1-2][0-9][0-9]|3[0-5][0-9]|36[0-6])`
	reWeekOfYr  = `(?:0[1-9]|[1-4][0-9]|5[0-3])`
	reMonthLz   = `(?:0[0-9]|1[0-2])`
	reDayLz     = `(?:0[0-9]|[1-2][0-9]|3[01])`

	reDayFulls  = ciAlt("sundays", "mondays", "tuesdays", "wednesdays", "thursdays", "fridays", "saturdays")
	reDayFull   = ciAlt("sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday")
	reDayAbbr   = ciAlt("sun", "mon", "tue", "wed", "thu", "fri", "sat", "sun")
	reDayText   = alt(reDayFulls, reDayFull, reDayAbbr, ciAlt("weekday", "weekdays"))
	reMonthFull = ciAlt("january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december")
	reMonthAbbr = ciAlt("jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "sept", "oct", "nov", "dec")
	reMonthText = alt(reMonthFull, reMonthAbbr, `(?:I|II|III|IV|V|VI|VII|VIII|IX|X|XI|XII)`)

	reT           = `[tT]`
	reTimeTiny12  = reHour12 + reSpace + `?` + reMeridian
	reTimeShort12 = reHour12 + `[:.]` + reMinuteLz + reSpace + `?` + reMeridian
	reTimeLong12  = reHour12 + `[:.]` + reMinute + `[:.]` + reSecondLz + reSpace + `?` + reMeridian
	reTimeTiny24  = reT + reHour24
	reTimeShort24 = reT + `?` + reHour24 + `[:.]` + reMinute
	reTimeLong24  = reT + `?` + reHour24 + `[:.]` + reMinute + `[:.]` + reSecond
	reISOLong     = reT + `?` + reHour24 + `[:.]` + reMinute + `[:.]` + reSecond + reFrac
	reISONormTz   = reT + `?` + reHour24 + `[:.]` + reMinute + `[:.]` + reSecondLz + reSpace + `?` + alt(reTzCorr, reTz)
	reGNUNoColon  = reT + `?` + reHour24lz + reMinuteLz
	reISONoColon  = reT + `?` + reHour24lz + reMinuteLz + reSecondLz

	reDateNoYear = reMonthText + `[ .\t-]*` + reDay + `(?:[,.stndrh\t ]+|\x00)`

	reRelTextNumber = ciAlt("first", "second", "third", "fourth", "fifth", "sixth", "seventh", "eight", "eighth", "ninth", "tenth", "eleventh", "twelfth")
	reRelTextText   = ciAlt("next", "last", "previous", "this")
	reRelTextUnit   = alt(ci("ms"), `\x{b5}[sS]`,
		alt(ci("msec"), ci("millisecond"), `\x{b5}`+ci("sec"), ci("microsecond"), ci("usec"), ci("sec"), ci("second"), ci("min"), ci("minute"),
			ci("hour"), ci("day"), ci("fortnight"), ci("forthnight"), ci("month"), ci("year"))+`[sS]?`,
		ci("weeks"), reDayText)
	reRelNumber = `[+-]*[ \t]*[0-9]{1,13}`
)

// rulesByFirstByte lists, in rule order, the rules whose matches can start
// with each byte; a rule that cannot is not tried there.
var rulesByFirstByte = func() (byFirst [256][]int) {
	for i, r := range rules {
		for b := range byFirst {
			if r.first[b] {
				byFirst[b] = append(byFirst[b], i)
			}
		}
	}
	return byFirst
}()

// rule is one re2c rule: a pattern and its action. An action returns false
// when it stops early on an error.
type rule struct {
	re     *regexp.Regexp
	action func(s *scanner, tok *cursor)
	first  byteSet
}

var rules = func() []rule {
	def := func(pattern string, action func(s *scanner, tok *cursor)) rule {
		re := regexp.MustCompile(`^(?:` + pattern + `)`)
		re.Longest()
		return rule{re: re, action: action, first: firstBytes(pattern)}
	}
	skip := func(*scanner, *cursor) {}

	return []rule{
		def(ci("yesterday"), func(s *scanner, _ *cursor) {
			s.t.HaveRelative = true
			s.unhaveTime()
			s.t.Relative.D = -1
		}),
		def(ci("now"), skip),
		def(ci("noon"), func(s *scanner, _ *cursor) {
			s.unhaveTime()
			if s.haveTime() {
				s.t.H = 12
			}
		}),
		def(ciAlt("midnight", "today"), func(s *scanner, _ *cursor) { s.unhaveTime() }),
		def(ci("tomorrow"), func(s *scanner, _ *cursor) {
			s.t.HaveRelative = true
			s.unhaveTime()
			s.t.Relative.D = 1
		}),
		def(`@-?[0-9]+`, actionTimestamp),
		def(`@-?[0-9]+\.[0-9]{0,6}`, actionTimestampMillis),
		def(ciAlt("first day of", "last day of"), func(s *scanner, p *cursor) {
			s.t.HaveRelative = true
			s.unhaveTime()
			if p.c() == 'l' || p.c() == 'L' {
				s.t.Relative.FirstLastDayOf = lastDayOfMonth
			} else {
				s.t.Relative.FirstLastDayOf = firstDayOfMonth
			}
		}),
		def(alt(ci("back of ")+reHour24+`(?:`+reSpace+`?`+reMeridian+`)?`, ci("front of ")+reHour24+`(?:`+reSpace+`?`+reMeridian+`)?`), actionBackFrontOf),
		def(alt(reRelTextNumber, reRelTextText)+reSpace+alt(reDayFulls, reDayFull, reDayAbbr)+reSpace+ci("of"), actionWeekdayOf),
		def(alt(reTimeTiny12, reTimeShort12, reTimeLong12), actionTime12),
		def(reHour12+`:`+reMinuteLz+`:`+reSecondLz+`[:.][0-9]+`+reMeridian, actionMSSQLTime),
		def(alt(reTimeTiny24, reTimeShort24, reTimeLong24, reISOLong), actionTime24),
		def(reGNUNoColon, actionGNUNoColon),
		def(reISONoColon, actionISONoColon),
		def(alt(reMonth+`/`+reDay, reMonth+`/`+reDay+`/`+reYear), actionAmerican),
		def(alt(reYear4Sign+`-`+reMonthLz+`-`+reDayLz, reYear4+`/`+reMonthLz+`/`+reDayLz+`/?`, reYear4+`/`+reMonth+`/`+reDay), actionISODate4),
		def(reYear2+`-`+reMonthLz+`-`+reDayLz, actionISODate2),
		def(reYearX+`-`+reMonthLz+`-`+reDayLz, actionISODateX),
		def(reYear4+`-`+reMonth, actionGNUDateShorter),
		def(reYear+`-`+reMonth+`-`+reDay, actionGNUDateShort),
		def(reDay+`[ \t.-]*`+reMonthText+`[ \t.-]*`+reYear, actionDateFull),
		def(reDay+`[.\t-]`+reMonth+`[.-]`+reYear4, actionPointedDate4),
		def(reDay+`[.\t]`+reMonth+`\.`+reYear2, actionPointedDate2),
		def(reMonthText+`[ .\t-]*`+reYear4, actionDateNoDay),
		def(reYear4+`[ .\t-]*`+reMonthText, actionDateNoDayRev),
		def(alt(reMonthText+`[ .\t-]*`+reDay+`[,.stndrh\t ]+`+reYear, reDateNoYear), actionDateTextual),
		def(reDay+`[ .\t-]*`+reMonthText, actionDateNoYearRev),
		def(reYear4+reMonthLz+reDayLz, actionDateNoColon),
		def(alt(
			reYear4+`-`+reMonthLz+`-`+reDayLz+`T`+reHour24lz+`:`+reMinuteLz+`:`+reSecondLz+reFrac+`(?:`+reTzCorr+`)?`,
			reYear4+reMonthLz+reDayLz+`T`+reHour24+`:`+reMinuteLz+`:`+reSecondLz,
			reYear4+reMonthLz+reDayLz+reT+reHour24+reMinuteLz+reSecondLz,
			reYear4+`-`+reMonth+`-`+reDay+`T`+reHour24+`:`+reMinute+`:`+reSecond,
			reYear4+`:`+reMonthLz+`:`+reDayLz+` `+reHour24lz+`:`+reMinuteLz+`:`+reSecondLz,
		), actionXMLRPC),
		def(reYear4+`[.-]?`+reDayOfYear, actionPgYearDay),
		def(reYear4+`-?W`+reWeekOfYr+`-?[0-7]`, actionISOWeekDay),
		def(reYear4+`-?W`+reWeekOfYr, actionISOWeek),
		def(reMonthAbbr+`-`+reDayLz+`-`+reYear, actionPgTextShort),
		def(reYear+`-`+reMonthAbbr+`-`+reDayLz, actionPgTextReverse),
		def(reDay+`/`+reMonthAbbr+`/`+reYear4+`:`+reHour24lz+`:`+reMinuteLz+`:`+reSecondLz+reSpace+reTzCorr, actionCLF),
		def(reYear4, func(s *scanner, p *cursor) { s.t.Y = p.getNr(4) }),
		def(ci("ago"), actionAgo),
		def(reDayText, actionDayText),
		def(reRelTextText+reSpace+ci("week"), actionRelativeTextWeek),
		def(alt(reRelTextNumber, reRelTextText)+reSpace+reRelTextUnit, actionRelativeText),
		def(alt(reMonthFull, reMonthAbbr), func(s *scanner, p *cursor) {
			if s.haveDate() {
				s.t.M = p.lookupMonth()
			}
		}),
		def(alt(reTzCorr, reTz), actionTimeZone),
		def(alt(reDateNoYear+reTimeShort12, reDateNoYear+reTimeLong12), actionDateShortWithTime12),
		def(alt(reDateNoYear+reTimeShort24, reDateNoYear+reTimeLong24, reDateNoYear+reISONormTz), actionDateShortWithTime24),
		def(reRelNumber+reSpace+`?`+alt(reRelTextUnit, ci("week")), actionRelative),
		def(`[.,]`, skip),
		def(reSpace, skip),
		def(`[\x00\n]`, skip),
		def(`(?s:.)`, func(s *scanner, _ *cursor) { s.failed = true }),
	}
}()

type scanner struct {
	t      *Time
	failed bool
}

// StrToTime parses s as timelib_strtotime does; ok is false if timelib
// reports any error or warning.
func StrToTime(s string) (t *Time, ok bool) {
	return strToTime(s, func(c byte) []int { return rulesByFirstByte[c] })
}

// strToTime is StrToTime trying, at each position, the rules candidates
// returns for the byte there.
func strToTime(s string, candidates func(byte) []int) (t *Time, ok bool) {
	b := []byte(s)
	start, end := 0, len(b)-1
	if len(b) > 0 {
		for isSpace(b[start]) && start < end {
			start++
		}
		for isSpace(b[end]) && end > start {
			end--
		}
	}
	if end < start {
		return newTime(), false
	}
	input := b[start : end+1]
	buf := make([]byte, len(input)+maxFill)
	copy(buf, input)

	sc := &scanner{t: newTime()}
	// Once the scan has failed the result is an error whatever follows, so
	// the rest of the input is not scanned.
	for pos := 0; pos < len(input) && !sc.failed; {
		best, bestLen := -1, 0
		for _, i := range candidates(buf[pos]) {
			if loc := rules[i].re.FindIndex(buf[pos:]); loc != nil && loc[1] > bestLen {
				best, bestLen = i, loc[1]
			}
		}
		rules[best].action(sc, &cursor{b: buf[pos : pos+bestLen]})
		pos += bestLen
	}

	t = sc.t
	if t.HaveTime != 0 && !validTime(t.H, t.I, t.S) {
		sc.failed = true
	}
	if t.HaveDate != 0 && !validDate(t.Y, t.M, t.D) {
		sc.failed = true
	}
	return t, !sc.failed
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

// The TIMELIB_HAVE_* macros; each reports whether the action may continue.

func (s *scanner) haveTime() bool {
	if s.t.HaveTime != 0 {
		s.failed = true
		return false
	}
	s.t.HaveTime = 1
	s.t.H, s.t.I, s.t.S, s.t.US = 0, 0, 0, 0
	return true
}

func (s *scanner) unhaveTime() {
	s.t.HaveTime = 0
	s.t.H, s.t.I, s.t.S, s.t.US = 0, 0, 0, 0
}

func (s *scanner) haveDate() bool {
	if s.t.HaveDate != 0 {
		s.failed = true
		return false
	}
	s.t.HaveDate = 1
	return true
}

func (s *scanner) unhaveDate() {
	s.t.HaveDate = 0
	s.t.D, s.t.M, s.t.Y = 0, 0, 0
}

func (s *scanner) haveTZ() bool {
	if s.t.HaveZone != 0 {
		s.failed = true
		s.t.HaveZone++
		return false
	}
	s.t.HaveZone++
	return true
}

func (s *scanner) haveWeekdayRelative() {
	s.t.HaveRelative = true
	s.t.Relative.HaveWeekdayRelative = true
}

func (s *scanner) haveSpecialRelative() {
	s.t.HaveRelative = true
	s.t.Relative.HaveSpecialRelative = true
}

// processYear is TIMELIB_PROCESS_YEAR: one and two digit years are 1970-2069.
func processYear(y int64, length int) int64 {
	if y == Unset || length >= 4 || y >= 100 {
		return y
	}
	if y < 70 {
		return y + 2000
	}
	return y + 1900
}

func (s *scanner) parseZoneInto(p *cursor) {
	z, notFound := p.parseZone(s.t)
	s.t.Z = z
	if notFound {
		s.failed = true
	}
}

func actionTimestamp(s *scanner, p *cursor) {
	s.t.HaveRelative = true
	s.unhaveDate()
	s.unhaveTime()
	if !s.haveTZ() {
		return
	}
	i := p.getSignedNr(s, 24)
	s.setEpochOrigin()
	s.t.Relative.S += i
}

func actionTimestampMillis(s *scanner, p *cursor) {
	s.t.HaveRelative = true
	s.unhaveDate()
	s.unhaveTime()
	if !s.haveTZ() {
		return
	}
	negative := p.at(1) == '-'
	i := p.getSignedNr(s, 24)
	before := p.i
	us := p.getSignedNr(s, 6)
	us = int64(float64(us) * pow10(7-(p.i-before)))
	if negative {
		us = -us
	}
	s.setEpochOrigin()
	s.t.Relative.S += i
	s.t.Relative.US = us
}

func (s *scanner) setEpochOrigin() {
	s.t.Y, s.t.M, s.t.D = 1970, 1, 1
	s.t.H, s.t.I, s.t.S, s.t.US = 0, 0, 0, 0
	s.t.ZoneType = ZoneOffset
	s.t.Z, s.t.Dst = 0, 0
}

func actionBackFrontOf(s *scanner, p *cursor) {
	s.unhaveTime()
	if !s.haveTime() {
		return
	}
	if p.c() == 'b' {
		s.t.H = p.getNr(2)
		s.t.I = 15
	} else {
		s.t.H = p.getNr(2) - 1
		s.t.I = 45
	}
	if p.c() != 0 {
		p.eatSpaces()
		s.t.H += p.meridian(s.t.H)
	}
}

func actionWeekdayOf(s *scanner, p *cursor) {
	s.t.HaveRelative = true
	s.haveSpecialRelative()
	i, behavior := p.getRelativeText()
	p.eatSpaces()
	if i > 0 {
		s.t.Relative.SpecialType = specialDayOfWeekInMonth
		p.setRelative(s, i, 1, false)
	} else {
		s.t.Relative.SpecialType = specialLastDayOfWeekInMont
		p.setRelative(s, i, behavior, false)
	}
}

func actionTime12(s *scanner, p *cursor) {
	if !s.haveTime() {
		return
	}
	s.t.H = p.getNr(2)
	if p.c() == ':' || p.c() == '.' {
		s.t.I = p.getNr(2)
		if p.c() == ':' || p.c() == '.' {
			s.t.S = p.getNr(2)
		}
	}
	p.eatSpaces()
	s.t.H += p.meridian(s.t.H)
}

func actionMSSQLTime(s *scanner, p *cursor) {
	if !s.haveTime() {
		return
	}
	s.t.H = p.getNr(2)
	s.t.I = p.getNr(2)
	if p.c() == ':' || p.c() == '.' {
		s.t.S = p.getNr(2)
		if p.c() == ':' || p.c() == '.' {
			s.t.US = p.getFracNr()
		}
	}
	p.eatSpaces()
	s.t.H += p.meridian(s.t.H)
}

func actionTime24(s *scanner, p *cursor) {
	if !s.haveTime() {
		return
	}
	s.t.H = p.getNr(2)
	if p.c() == ':' || p.c() == '.' {
		s.t.I = p.getNr(2)
		if p.c() == ':' || p.c() == '.' {
			s.t.S = p.getNr(2)
			if p.c() == '.' {
				s.t.US = p.getFracNr()
			}
		}
	}
	if p.c() != 0 {
		s.parseZoneInto(p)
	}
}

func actionGNUNoColon(s *scanner, p *cursor) {
	switch s.t.HaveTime {
	case 0:
		s.t.H = p.getNr(2)
		s.t.I = p.getNr(2)
		s.t.S = 0
	case 1:
		s.t.Y = p.getNr(4)
	default:
		s.failed = true
		return
	}
	s.t.HaveTime++
}

func actionISONoColon(s *scanner, p *cursor) {
	if !s.haveTime() {
		return
	}
	s.t.H = p.getNr(2)
	s.t.I = p.getNr(2)
	s.t.S = p.getNr(2)
	if p.c() != 0 {
		s.parseZoneInto(p)
	}
}

func actionAmerican(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.M = p.getNr(2)
	s.t.D = p.getNr(2)
	if p.c() == '/' {
		y, length := p.getNrEx(4)
		s.t.Y = processYear(y, length)
	}
}

func actionISODate4(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.Y = p.getSignedNr(s, 4)
	s.t.M = p.getNr(2)
	s.t.D = p.getNr(2)
}

func actionISODate2(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	y, length := p.getNrEx(4)
	s.t.M = p.getNr(2)
	s.t.D = p.getNr(2)
	s.t.Y = processYear(y, length)
}

func actionISODateX(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.Y = p.getSignedNr(s, 19)
	s.t.M = p.getNr(2)
	s.t.D = p.getNr(2)
}

func actionGNUDateShorter(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	y, length := p.getNrEx(4)
	s.t.M = p.getNr(2)
	s.t.D = 1
	s.t.Y = processYear(y, length)
}

func actionGNUDateShort(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	y, length := p.getNrEx(4)
	s.t.M = p.getNr(2)
	s.t.D = p.getNr(2)
	s.t.Y = processYear(y, length)
}

func actionDateFull(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.D = p.getNr(2)
	p.skipDaySuffix()
	s.t.M = p.getMonth()
	y, length := p.getNrEx(4)
	s.t.Y = processYear(y, length)
}

func actionPointedDate4(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.D = p.getNr(2)
	s.t.M = p.getNr(2)
	s.t.Y = p.getNr(4)
}

func actionPointedDate2(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.D = p.getNr(2)
	s.t.M = p.getNr(2)
	y, length := p.getNrEx(2)
	s.t.Y = processYear(y, length)
}

func actionDateNoDay(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.M = p.getMonth()
	y, length := p.getNrEx(4)
	s.t.D = 1
	s.t.Y = processYear(y, length)
}

func actionDateNoDayRev(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	y, length := p.getNrEx(4)
	s.t.M = p.getMonth()
	s.t.D = 1
	s.t.Y = processYear(y, length)
}

func actionDateTextual(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.M = p.getMonth()
	s.t.D = p.getNr(2)
	y, length := p.getNrEx(4)
	s.t.Y = processYear(y, length)
}

func actionDateNoYearRev(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.D = p.getNr(2)
	p.skipDaySuffix()
	s.t.M = p.getMonth()
}

func actionDateNoColon(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.Y = p.getNr(4)
	s.t.M = p.getNr(2)
	s.t.D = p.getNr(2)
}

func actionXMLRPC(s *scanner, p *cursor) {
	if !s.haveTime() || !s.haveDate() {
		return
	}
	s.t.Y = p.getNr(4)
	s.t.M = p.getNr(2)
	s.t.D = p.getNr(2)
	s.t.H = p.getNr(2)
	s.t.I = p.getNr(2)
	s.t.S = p.getNr(2)
	if p.c() == '.' {
		s.t.US = p.getFracNr()
		if p.c() != 0 {
			s.parseZoneInto(p)
		}
	}
}

func actionPgYearDay(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	y, length := p.getNrEx(4)
	s.t.D = p.getNr(3)
	s.t.M = 1
	s.t.Y = processYear(y, length)
}

func actionISOWeekDay(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.HaveRelative = true
	s.t.Y = p.getNr(4)
	w := p.getNr(2)
	d := p.getNr(1)
	s.t.M, s.t.D = 1, 1
	s.t.Relative.D = daynrFromWeeknr(s.t.Y, w, d)
}

func actionISOWeek(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.HaveRelative = true
	s.t.Y = p.getNr(4)
	w := p.getNr(2)
	s.t.M, s.t.D = 1, 1
	s.t.Relative.D = daynrFromWeeknr(s.t.Y, w, 1)
}

func actionPgTextShort(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.M = p.getMonth()
	s.t.D = p.getNr(2)
	y, length := p.getNrEx(4)
	s.t.Y = processYear(y, length)
}

func actionPgTextReverse(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	y, length := p.getNrEx(4)
	s.t.M = p.getMonth()
	s.t.D = p.getNr(2)
	s.t.Y = processYear(y, length)
}

func actionCLF(s *scanner, p *cursor) {
	if !s.haveTime() || !s.haveDate() {
		return
	}
	s.t.D = p.getNr(2)
	s.t.M = p.getMonth()
	s.t.Y = p.getNr(4)
	s.t.H = p.getNr(2)
	s.t.I = p.getNr(2)
	s.t.S = p.getNr(2)
	p.eatSpaces()
	s.parseZoneInto(p)
}

func actionAgo(s *scanner, _ *cursor) {
	r := &s.t.Relative
	r.Y, r.M, r.D, r.H, r.I, r.S = -r.Y, -r.M, -r.D, -r.H, -r.I, -r.S
	r.Weekday = -r.Weekday
	if r.Weekday == 0 {
		r.Weekday = -7
	}
	if r.HaveSpecialRelative && r.SpecialType == specialWeekday {
		r.SpecialAmount = -r.SpecialAmount
	}
}

func actionDayText(s *scanner, p *cursor) {
	s.t.HaveRelative = true
	s.haveWeekdayRelative()
	s.unhaveTime()
	if unit := p.lookupRelunit(); unit != nil {
		s.t.Relative.Weekday = unit.multiplier
	}
	if s.t.Relative.WeekdayBehavior != 2 {
		s.t.Relative.WeekdayBehavior = 1
	}
}

func actionRelativeTextWeek(s *scanner, p *cursor) {
	s.t.HaveRelative = true
	for p.c() != 0 {
		start := p.i
		i, behavior := p.getRelativeText()
		p.eatSpaces()
		p.setRelative(s, i, behavior, false)
		s.t.Relative.WeekdayBehavior = 2
		if !s.t.Relative.HaveWeekdayRelative {
			s.haveWeekdayRelative()
			s.t.Relative.Weekday = 1
		}
		if p.i == start {
			p.i++
		}
	}
}

func actionRelativeText(s *scanner, p *cursor) {
	s.t.HaveRelative = true
	for p.c() != 0 {
		start := p.i
		i, behavior := p.getRelativeText()
		p.eatSpaces()
		p.setRelative(s, i, behavior, false)
		if p.i == start {
			p.i++
		}
	}
}

func actionTimeZone(s *scanner, p *cursor) {
	if !s.haveTZ() {
		return
	}
	p.eatSpaces()
	s.parseZoneInto(p)
}

func actionDateShortWithTime12(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.M = p.getMonth()
	s.t.D = p.getNr(2)
	if !s.haveTime() {
		return
	}
	s.t.H = p.getNr(2)
	s.t.I = p.getNr(2)
	if p.c() == ':' || p.c() == '.' {
		s.t.S = p.getNr(2)
		if p.c() == '.' {
			s.t.US = p.getFracNr()
		}
	}
	s.t.H += p.meridian(s.t.H)
}

func actionDateShortWithTime24(s *scanner, p *cursor) {
	if !s.haveDate() {
		return
	}
	s.t.M = p.getMonth()
	s.t.D = p.getNr(2)
	if !s.haveTime() {
		return
	}
	s.t.H = p.getNr(2)
	s.t.I = p.getNr(2)
	if p.c() == ':' {
		s.t.S = p.getNr(2)
		if p.c() == '.' {
			s.t.US = p.getFracNr()
		}
	}
	if p.c() != 0 {
		s.parseZoneInto(p)
	}
}

func actionRelative(s *scanner, p *cursor) {
	s.t.HaveRelative = true
	for p.c() != 0 {
		start := p.i
		i := p.getSignedNr(s, 24)
		p.eatSpaces()
		p.setRelative(s, i, 1, true)
		if p.i == start {
			p.i++
		}
	}
}
