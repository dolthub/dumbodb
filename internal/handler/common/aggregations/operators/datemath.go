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

package operators

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// Calendar arithmetic ported from timelib, which MongoDB uses for every date
// expression, so that years far outside 1 to 9999 behave the same.

// civilDate is a proleptic Gregorian date and wall clock time.
type civilDate struct {
	year, month, day             int64
	hour, minute, second, millis int64
}

// civilFromMillis splits Unix milliseconds into date and time parts.
func civilFromMillis(ms int64) civilDate {
	return civilFromSeconds(floorDiv(ms, 1000), floorMod(ms, 1000))
}

// civilFromSeconds is civilFromMillis for a time split into whole seconds
// and milliseconds, which can be shifted by a zone offset without
// overflowing at the ends of the date range.
func civilFromSeconds(seconds, millis int64) civilDate {
	days, secondOfDay := floorDiv(seconds, 86400), floorMod(seconds, 86400)
	year, month, day := civilFromDays(days)
	return civilDate{
		year: year, month: month, day: day,
		hour: secondOfDay / 3600, minute: secondOfDay % 3600 / 60, second: secondOfDay % 60,
		millis: millis,
	}
}

// civilFromDays is the inverse of daysFromCivil.
func civilFromDays(days int64) (year, month, day int64) {
	days += 719468
	era := floorDiv(days, 146097)
	dayOfEra := days - era*146097
	yearOfEra := (dayOfEra - dayOfEra/1460 + dayOfEra/36524 - dayOfEra/146096) / 365
	dayOfYear := dayOfEra - (365*yearOfEra + yearOfEra/4 - yearOfEra/100)
	mp := (5*dayOfYear + 2) / 153
	day = dayOfYear - (153*mp+2)/5 + 1
	month = mp + 3
	if mp >= 10 {
		month = mp - 9
	}
	year = yearOfEra + era*400
	if month <= 2 {
		year++
	}
	return year, month, day
}

func timelibIsLeap(y int64) bool {
	return y%4 == 0 && (y%100 != 0 || y%400 == 0)
}

var (
	timelibMonthTableCommon = [13]int64{-1, 0, 3, 3, 6, 1, 4, 6, 2, 5, 0, 3, 5}
	timelibMonthTableLeap   = [13]int64{-1, 6, 2, 3, 6, 1, 4, 6, 2, 5, 0, 3, 5}
	timelibDayTableCommon   = [13]int64{0, 0, 31, 59, 90, 120, 151, 181, 212, 243, 273, 304, 334}
	timelibDayTableLeap     = [13]int64{0, 0, 31, 60, 91, 121, 152, 182, 213, 244, 274, 305, 335}
)

// timelibDayOfWeek is timelib_day_of_week: 0 is Sunday.
func timelibDayOfWeek(y, m, d int64) int64 {
	c1 := 6 - floorMod(floorMod(y, 400)/100, 4)*2
	y1 := floorMod(y, 100)
	m1 := timelibMonthTableCommon[m]
	if timelibIsLeap(y) {
		m1 = timelibMonthTableLeap[m]
	}
	return floorMod(c1+y1+m1+y1/4+d, 7)
}

func timelibDayOfYear(y, m, d int64) int64 {
	if timelibIsLeap(y) {
		return timelibDayTableLeap[m] + d - 1
	}
	return timelibDayTableCommon[m] + d - 1
}

// timelibISODate is timelib_isodate_from_date, including its double count of
// the leap day, which its later checks compensate for.
func timelibISODate(y, m, d int64) (isoYear, isoWeek, isoDay int64) {
	yLeap := timelibIsLeap(y)
	prevLeap := timelibIsLeap(y - 1)
	doy := timelibDayOfYear(y, m, d) + 1
	if yLeap && m > 2 {
		doy++
	}
	jan1 := timelibDayOfWeek(y, 1, 1)
	weekday := timelibDayOfWeek(y, m, d)
	if weekday == 0 {
		weekday = 7
	}
	if jan1 == 0 {
		jan1 = 7
	}
	isoDay = weekday

	if doy <= 8-jan1 && jan1 > 4 {
		isoYear = y - 1
		isoWeek = 52
		if jan1 == 5 || (jan1 == 6 && prevLeap) {
			isoWeek = 53
		}
		return isoYear, isoWeek, isoDay
	}

	isoYear = y
	leapDays := int64(0)
	if yLeap {
		leapDays = 1
	}
	if 365+leapDays-(doy-leapDays) < 4-weekday {
		return y + 1, 1, isoDay
	}
	isoWeek = (doy + (7 - weekday) + (jan1 - 1)) / 7
	if jan1 > 4 {
		isoWeek--
	}
	return isoYear, isoWeek, isoDay
}

// localCivil returns the wall clock parts of Unix milliseconds in tz, as
// timelib_unixtime2local does.
func (tz mongoTimeZone) localCivil(ms int64) civilDate {
	seconds := floorDiv(ms, 1000)
	offset := tz.offset
	if tz.loc != nil {
		offset, _, _ = tz.zoneInfo(seconds)
	}
	return civilFromSeconds(seconds+offset, floorMod(ms, 1000))
}

// coerceToDateMillis is Value::coerceToDate: dates, timestamps (their
// seconds) and ObjectIds (their creation second) convert, anything else is
// error 16006.
func coerceToDateMillis(v any) (int64, error) {
	switch d := v.(type) {
	case time.Time:
		return d.UnixMilli(), nil
	case types.Timestamp:
		return int64(d>>32) * 1000, nil
	case types.ObjectID:
		return int64(binary.BigEndian.Uint32(d[:4])) * 1000, nil
	default:
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCannotConvertToDate,
			fmt.Sprintf("can't convert from BSON type %s to Date", handlerparams.AliasFromType(v)), "date")
	}
}
