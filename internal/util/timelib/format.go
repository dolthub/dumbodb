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

// Format is a format specifier's meaning (TIMELIB_FORMAT_*), for the subset
// of timelib's specifiers MongoDB maps.
type Format int

// Format specifiers.
const (
	FormatLiteral Format = iota
	FormatTextualMonth
	FormatDayTwoDigit
	FormatYearISO
	FormatHour24
	FormatDayOfYear
	FormatMillisecond
	FormatMonthTwoDigit
	FormatMinute
	FormatSecond
	FormatDayOfWeekISO
	FormatWeekOfYearISO
	FormatYearFourDigit
	FormatTimezoneOffset
	FormatTimezoneOffsetMinutes
)

// ParseFromFormat parses s as timelib_parse_from_format_with_map does with a
// prefix character; specifiers maps the character after the prefix to its
// meaning. ok is false if timelib reports any error or warning.
func ParseFromFormat(format, s string, prefix byte, specifiers map[byte]Format) (t *Time, ok bool) {
	t = newTime()
	failed := false
	f := &cursor{b: []byte(format)}
	p := &cursor{b: []byte(s)}
	isoYear, isoWeek, isoDay := int64(Unset), int64(Unset), int64(Unset)
	prefixFound := false

	checkNumber := func() {
		if !strchr("0123456789", p.c()) {
			failed = true
		}
	}

	for f.c() != 0 && p.c() != 0 {
		if (!prefixFound && f.c() != prefix) || (prefixFound && f.c() == prefix) {
			if f.c() != p.c() {
				failed = true
			}
			p.i++
			f.i++
			prefixFound = false
			continue
		}
		if f.c() == prefix {
			f.i++
			prefixFound = true
			continue
		}
		prefixFound = false

		switch specifiers[f.c()] {
		case FormatDayTwoDigit:
			checkNumber()
			if t.D = p.getNr(2); t.D == Unset {
				failed = true
				break
			}
			t.HaveDate = 1
		case FormatDayOfYear:
			checkNumber()
			if t.Y == Unset {
				failed = true
			}
			n := p.getNr(3)
			if n == Unset {
				failed = true
				break
			}
			if t.Y != Unset {
				t.HaveDate = 1
				t.M = 1
				t.D = n + 1
				t.normalize()
			}
		case FormatMonthTwoDigit:
			checkNumber()
			if t.M = p.getNr(2); t.M == Unset {
				failed = true
				break
			}
			t.HaveDate = 1
		case FormatTextualMonth:
			n := p.lookupMonth()
			if n == 0 {
				failed = true
				break
			}
			t.HaveDate = 1
			t.M = n
		case FormatYearFourDigit:
			checkNumber()
			if t.Y = p.getNr(4); t.Y == Unset {
				failed = true
				break
			}
			t.HaveDate = 1
		case FormatHour24:
			checkNumber()
			if t.H = p.getNr(2); t.H == Unset {
				failed = true
				break
			}
			t.HaveTime = 1
		case FormatMinute, FormatSecond:
			checkNumber()
			n, length := p.getNrEx(2)
			if n == Unset || length != 2 {
				failed = true
				break
			}
			t.HaveTime = 1
			if specifiers[f.c()] == FormatMinute {
				t.I = n
			} else {
				t.S = n
			}
		case FormatMillisecond:
			checkNumber()
			before := p.i
			n := p.getNr(3)
			if n == Unset || p.i-before < 1 {
				failed = true
				break
			}
			t.US = int64(float64(n) * pow10(3-(p.i-before)) * 1000)
		case FormatYearISO:
			if isoYear = p.getNr(4); isoYear == Unset {
				failed = true
				break
			}
			t.HaveDate = 1
		case FormatWeekOfYearISO:
			if isoWeek = p.getNr(2); isoWeek == Unset {
				failed = true
				break
			}
			if isoWeek < 1 || isoWeek > 53 {
				failed = true
				break
			}
			t.HaveDate = 1
		case FormatDayOfWeekISO:
			if isoDay = p.getNr(1); isoDay == Unset {
				failed = true
				break
			}
			if isoDay < 1 || isoDay > 7 {
				failed = true
				break
			}
			t.HaveDate = 1
		case FormatTimezoneOffset:
			z, notFound := p.parseZone(t)
			t.Z = z
			if notFound {
				failed = true
				break
			}
			t.HaveZone = 1
		case FormatTimezoneOffsetMinutes:
			z, ok := p.parseTzMinutes(t)
			if !ok {
				t.Z = Unset
				failed = true
				break
			}
			t.Z = z
			t.HaveZone = 1
		default:
			if f.c() != p.c() {
				failed = true
			}
			p.i++
		}
		f.i++
	}

	if p.c() != 0 || f.c() != 0 {
		failed = true
	}

	if t.H != Unset || t.I != Unset || t.S != Unset || t.US != Unset {
		for _, field := range []*int64{&t.H, &t.I, &t.S, &t.US} {
			if *field == Unset {
				*field = 0
			}
		}
	}

	if t.Y != Unset && (isoWeek != Unset || isoYear != Unset || isoDay != Unset) {
		failed = true
	}
	if isoYear != Unset && (t.Y != Unset || t.M != Unset || t.D != Unset) {
		failed = true
	}
	if isoYear != Unset {
		if isoWeek == Unset {
			isoWeek = 1
		}
		if isoDay == Unset {
			isoDay = 1
		}
		t.Y, t.M, t.D = dateFromISODate(isoYear, isoWeek, isoDay)
	} else if isoWeek != Unset || isoDay != Unset {
		failed = true
	}

	if t.H != Unset && t.I != Unset && t.S != Unset && !validTime(t.H, t.I, t.S) {
		failed = true
	}
	if t.Y != Unset && t.M != Unset && t.D != Unset && !validDate(t.Y, t.M, t.D) {
		failed = true
	}
	return t, !failed
}
