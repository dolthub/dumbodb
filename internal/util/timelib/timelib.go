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

// Package timelib is a port of the date parsing parts of timelib 2022.10,
// the library MongoDB uses for $dateFromString, so that DumboDB accepts and
// rejects exactly the strings MongoDB does. It is derived from timelib's
// parse_date.re, tm2unixtime.c and dow.c, distributed under the MIT License
// reproduced in this directory's LICENSE file.
//
// Errors and warnings are not itemized: MongoDB rejects a string with any.
package timelib

// Unset marks a field the parsed string did not set (TIMELIB_UNSET).
const Unset = -9999999

// Zone types (TIMELIB_ZONETYPE_*).
const (
	ZoneNone = iota
	ZoneOffset
	ZoneAbbr
	ZoneID
)

const (
	unitSecond = iota + 1
	unitMinute
	unitHour
	unitDay
	unitMonth
	unitYear
	unitWeekday
	unitSpecial
	unitMicrosecond
)

const (
	specialWeekday             = 1
	specialDayOfWeekInMonth    = 2
	specialLastDayOfWeekInMont = 3

	firstDayOfMonth = 1
	lastDayOfMonth  = 2
)

// Relative is timelib_rel_time, the relative part of a parsed string.
type Relative struct {
	Y, M, D, H, I, S, US int64

	Weekday             int64
	WeekdayBehavior     int64
	HaveWeekdayRelative bool
	HaveSpecialRelative bool
	SpecialType         int64
	SpecialAmount       int64
	FirstLastDayOf      int64
}

// Time is timelib_time: the fields a string set, Unset where it did not.
type Time struct {
	Y, M, D, H, I, S, US int64

	// Z is the zone offset in seconds east of UTC, without Dst.
	Z, Dst   int64
	ZoneType int
	TzAbbr   string

	HaveTime, HaveDate, HaveZone int
	HaveRelative                 bool
	Relative                     Relative
}

func newTime() *Time {
	return &Time{Y: Unset, M: Unset, D: Unset, H: Unset, I: Unset, S: Unset, US: Unset, Z: Unset, Dst: Unset}
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func isLeap(y int64) bool {
	return y%4 == 0 && (y%100 != 0 || y%400 == 0)
}

var (
	monthTableCommon = [13]int64{-1, 0, 3, 3, 6, 1, 4, 6, 2, 5, 0, 3, 5}
	monthTableLeap   = [13]int64{-1, 6, 2, 3, 6, 1, 4, 6, 2, 5, 0, 3, 5}
	daysCommon       = [13]int64{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	daysLeap         = [13]int64{0, 31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
)

func positiveMod(x, y int64) int64 {
	r := x % y
	if r < 0 {
		r += y
	}
	return r
}

// DayOfWeek is timelib_day_of_week: 0 is Sunday.
func DayOfWeek(y, m, d int64) int64 {
	c1 := 6 - positiveMod(positiveMod(y, 400)/100, 4)*2
	y1 := positiveMod(y, 100)
	m1 := monthTableCommon[m]
	if isLeap(y) {
		m1 = monthTableLeap[m]
	}
	return positiveMod(c1+y1+m1+y1/4+d, 7)
}

func daysInMonth(y, m int64) int64 {
	if isLeap(y) {
		return daysLeap[m]
	}
	return daysCommon[m]
}

func validTime(h, i, s int64) bool {
	return h >= 0 && h <= 23 && i >= 0 && i <= 59 && s >= 0 && s <= 59
}

func validDate(y, m, d int64) bool {
	return m >= 1 && m <= 12 && d >= 1 && d <= daysInMonth(y, m)
}

// daynrFromWeeknr is timelib_daynr_from_weeknr.
func daynrFromWeeknr(iy, iw, id int64) int64 {
	dow := DayOfWeek(iy, 1, 1)
	day := -dow
	if dow > 4 {
		day = 7 - dow
	}
	return day + (iw-1)*7 + id
}

// dateFromISODate is timelib_date_from_isodate.
func dateFromISODate(iy, iw, id int64) (y, m, d int64) {
	return civilFromDays(epochDays(iy, 1, 1) + daynrFromWeeknr(iy, iw, id))
}

// epochDays returns days since 1970-01-01 for a month in [1, 12] and any day.
func epochDays(y, m, d int64) int64 {
	if m <= 2 {
		y--
	}
	era := floorDiv(y, 400)
	yoe := y - era*400
	mp := m + 9
	if m > 2 {
		mp = m - 3
	}
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

func civilFromDays(days int64) (y, m, d int64) {
	days += 719468
	era := floorDiv(days, 146097)
	doe := days - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = doy - (153*mp+2)/5 + 1
	m = mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	y = yoe + era*400
	if m <= 2 {
		y++
	}
	return y, m, d
}

// rangeLimit is do_range_limit: it carries a into b so start <= a < end.
func rangeLimit(start, end, adj int64, a, b *int64) {
	if *a < start {
		aPlus1 := *a + 1
		*b -= (start-aPlus1)/adj + 1
		*a += adj * ((start - aPlus1) / adj)
		*a += adj
	}
	if *a >= end {
		*b += *a / adj
		*a -= adj * (*a / adj)
	}
}

// normalize is timelib_do_normalize. Its day loop steps month by month; the
// result is the same calendar date, computed here directly.
func (t *Time) normalize() {
	if t.US != Unset {
		rangeLimit(0, 1000000, 1000000, &t.US, &t.S)
	}
	if t.S != Unset {
		rangeLimit(0, 60, 60, &t.S, &t.I)
		rangeLimit(0, 60, 60, &t.I, &t.H)
		rangeLimit(0, 24, 24, &t.H, &t.D)
	}
	rangeLimit(1, 13, 12, &t.M, &t.Y)
	if t.D < 1 || t.D > daysInMonth(t.Y, t.M) {
		t.Y, t.M, t.D = civilFromDays(epochDays(t.Y, t.M, 1) + t.D - 1)
	}
}

// adjustForWeekday is do_adjust_for_weekday.
func (t *Time) adjustForWeekday() {
	currentDow := DayOfWeek(t.Y, t.M, t.D)
	r := &t.Relative
	if r.WeekdayBehavior == 2 {
		if currentDow == 0 && r.Weekday != 0 {
			r.Weekday -= 7
		}
		if r.Weekday == 0 && currentDow != 0 {
			r.Weekday = 7
		}
		t.D -= currentDow
		t.D += r.Weekday
		return
	}
	difference := r.Weekday - currentDow
	if (r.D < 0 && difference < 0) || (r.D >= 0 && difference <= -r.WeekdayBehavior) {
		difference += 7
	}
	if r.Weekday >= 0 {
		t.D += difference
	} else {
		w := r.Weekday
		if w < 0 {
			w = -w
		}
		t.D -= 7 - (w - currentDow)
	}
	r.HaveWeekdayRelative = false
}

// adjustSpecialWeekday is do_adjust_special_weekday, counting business days.
func (t *Time) adjustSpecialWeekday() {
	count := t.Relative.SpecialAmount
	dow := DayOfWeek(t.Y, t.M, t.D)
	t.D += (count / 5) * 7
	rem := count % 5
	if count > 0 {
		switch {
		case rem == 0:
			if dow == 0 {
				t.D -= 2
			} else if dow == 6 {
				t.D--
			}
		case dow == 6:
			t.D++
		case dow+rem > 5:
			t.D += 2
		}
	} else {
		switch {
		case rem == 0:
			if dow == 6 {
				t.D += 2
			} else if dow == 0 {
				t.D++
			}
		case dow == 0:
			t.D--
		case dow+rem < 1:
			t.D -= 2
		}
	}
	t.D += rem
}

func (t *Time) applyFirstLastDayOf() {
	switch t.Relative.FirstLastDayOf {
	case firstDayOfMonth:
		t.D = 1
	case lastDayOfMonth:
		t.D = 0
		t.M++
	}
}

// Resolve is timelib_update_ts: it applies the relative parts and the zone,
// returning seconds since the epoch and the remaining microseconds. An ID
// zone converts local seconds with idZone. Every date and time field must be
// set.
func (t *Time) Resolve(idZone func(local int64) int64) (sse, us int64) {
	r := &t.Relative

	if r.HaveSpecialRelative {
		switch r.SpecialType {
		case specialDayOfWeekInMonth:
			t.D = 1
			t.M += r.M
			r.M = 0
		case specialLastDayOfWeekInMont:
			t.D = 1
			t.M += r.M + 1
			r.M = 0
		}
	}
	t.applyFirstLastDayOf()
	t.normalize()

	if r.HaveWeekdayRelative {
		t.adjustForWeekday()
	}
	t.normalize()
	if t.HaveRelative {
		t.US += r.US
		t.S += r.S
		t.I += r.I
		t.H += r.H
		t.D += r.D
		t.M += r.M
		t.Y += r.Y
	}
	t.applyFirstLastDayOf()
	t.normalize()

	if r.HaveSpecialRelative && r.SpecialType == specialWeekday {
		t.adjustSpecialWeekday()
	}
	t.normalize()
	r.SpecialType, r.SpecialAmount = 0, 0

	sse = t.H*3600 + t.I*60 + t.S + epochDays(t.Y, t.M, t.D)*86400
	switch t.ZoneType {
	case ZoneOffset:
		sse -= t.Z
	case ZoneAbbr:
		sse -= t.Z + t.Dst*3600
	case ZoneID:
		sse = idZone(sse)
	}
	return sse, t.US
}
