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
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // MongoDB embeds its time zone database; so does DumboDB.

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// mongoTimeZone is a time zone as MongoDB's TimeZoneDatabase resolves an
// identifier: a tz database zone, UTC, or a fixed UTC offset.
type mongoTimeZone struct {
	loc *time.Location

	// offset is the fixed offset in seconds when loc is nil.
	offset int64

	// epochIsDST is the zone's DST flag at the Unix epoch, which timelib
	// carries into its local time resolution.
	epochIsDST bool
}

var mongoTimeZoneCache sync.Map

// lookupMongoTimeZone resolves name as TimeZoneDatabase::getTimeZone does:
// an exact zone name, else a "+HH", "+HHMM" or "+HH:MM" offset.
func lookupMongoTimeZone(name string) (mongoTimeZone, bool) {
	if cached, ok := mongoTimeZoneCache.Load(name); ok {
		return cached.(mongoTimeZone), true
	}

	var tz mongoTimeZone
	switch {
	case name == "UTC":
	case name != "" && name != "Local" && !strings.HasPrefix(name, "+") && !strings.HasPrefix(name, "-"):
		loc, err := time.LoadLocation(name)
		if err != nil {
			return mongoTimeZone{}, false
		}
		tz = mongoTimeZone{loc: loc, epochIsDST: time.Unix(0, 0).In(loc).IsDST()}
	default:
		offset, ok := parseUTCOffset(name)
		if !ok {
			return mongoTimeZone{}, false
		}
		tz = mongoTimeZone{offset: offset}
	}

	mongoTimeZoneCache.Store(name, tz)
	return tz, true
}

func parseUTCOffset(spec string) (int64, bool) {
	if spec == "" || (spec[0] != '+' && spec[0] != '-') {
		return 0, false
	}
	sign := int64(1)
	if spec[0] == '-' {
		sign = -1
	}
	digits := func(s string) (int64, bool) {
		var n int64
		for _, c := range s {
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int64(c-'0')
		}
		return n, true
	}

	var hours, minutes int64
	var ok bool
	switch {
	case len(spec) == 3:
		hours, ok = digits(spec[1:3])
	case len(spec) == 5:
		var hhmm int64
		hhmm, ok = digits(spec[1:5])
		hours, minutes = hhmm/100, hhmm%100
	case len(spec) == 6 && spec[3] == ':':
		hours, ok = digits(spec[1:3])
		if ok {
			minutes, ok = digits(spec[4:6])
		}
	}
	if !ok {
		return 0, false
	}
	return sign * (hours*3600 + minutes*60), true
}

// timeZoneArg is a timezone operand, resolved when the operator is built if it
// is a constant, as MongoDB parses constant time zones up front.
type timeZoneArg struct {
	expr     any
	resolved *mongoTimeZone
}

func newTimeZoneArg(expr any, present bool) (*timeZoneArg, error) {
	if !present {
		return &timeZoneArg{resolved: &mongoTimeZone{}}, nil
	}

	arg := &timeZoneArg{expr: expr}
	if isConstantTimeZone(expr) {
		tz, isNull, err := resolveTimeZone(expr)
		if err != nil {
			return nil, err
		}
		if !isNull {
			arg.resolved = &tz
		}
	}
	return arg, nil
}

func isConstantTimeZone(expr any) bool {
	switch v := expr.(type) {
	case *types.Document, *types.Array:
		return false
	case string:
		return !strings.HasPrefix(v, "$")
	default:
		return !isNullish(v)
	}
}

// evaluate returns the time zone for doc; isNull reports a nullish operand.
func (arg *timeZoneArg) evaluate(doc *types.Document) (tz mongoTimeZone, isNull bool, err error) {
	if arg.resolved != nil {
		return *arg.resolved, false, nil
	}
	v, err := evalArgValue(arg.expr, doc)
	if err != nil {
		return mongoTimeZone{}, false, err
	}
	return resolveTimeZone(v)
}

func resolveTimeZone(v any) (mongoTimeZone, bool, error) {
	if isNullish(v) {
		return mongoTimeZone{}, true, nil
	}
	name, ok := v.(string)
	if !ok {
		return mongoTimeZone{}, false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTimeZoneNotString,
			fmt.Sprintf("timezone must evaluate to a string, found %s", handlerparams.AliasFromType(v)), "timezone")
	}
	tz, ok := lookupMongoTimeZone(name)
	if !ok {
		return mongoTimeZone{}, false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrUnrecognizedTimeZone,
			fmt.Sprintf("unrecognized time zone identifier: \"%s\"", name), "timezone")
	}
	return tz, false, nil
}

// zoneInfo is timelib_get_time_zone_offset_info: the offset in effect at
// unix, when that offset took effect (math.MinInt64 before the first
// transition), and whether it is daylight saving time.
func (tz mongoTimeZone) zoneInfo(unix int64) (offset, transition int64, isDST bool) {
	t := time.Unix(unix, 0).In(tz.loc)
	_, off := t.Zone()
	start, _ := t.ZoneBounds()
	transition = math.MinInt64
	if !start.IsZero() {
		transition = start.Unix()
	}
	return int64(off), transition, t.IsDST()
}

// utcSeconds converts local wall clock seconds to Unix seconds, resolving
// skipped and repeated local times as timelib's do_adjust_timezone does.
func (tz mongoTimeZone) utcSeconds(local int64) int64 {
	if tz.loc == nil {
		return local - tz.offset
	}

	current, _, currentIsDST := tz.zoneInfo(local)
	after, afterTransition, _ := tz.zoneInfo(local - current)
	actual, actualTransition := after, afterTransition
	if current == after {
		switch {
		case current >= 0 && tz.epochIsDST && !currentIsDST:
			earlier, earlierTransition, _ := tz.zoneInfo(local - current - 7200)
			if earlier != after && local-earlier < afterTransition {
				actual, actualTransition = earlier, earlierTransition
			}
		case current <= 0 && currentIsDST && !tz.epochIsDST:
			later, laterTransition, _ := tz.zoneInfo(local - current + 7200)
			if later != after && local-later >= laterTransition {
				actual, actualTransition = later, laterTransition
			}
		}
	}

	inTransition := actualTransition != math.MinInt64 &&
		local-actual >= actualTransition+(current-actual) &&
		local-actual < actualTransition
	if current != actual && !inTransition {
		return local - actual
	}
	return local - current
}

// utcMillis converts local wall clock milliseconds to Unix milliseconds.
func (tz mongoTimeZone) utcMillis(localMillis int64) int64 {
	seconds, millis := floorDiv(localMillis, 1000), floorMod(localMillis, 1000)
	return tz.utcSeconds(seconds)*1000 + millis
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func floorMod(a, b int64) int64 {
	return a - floorDiv(a, b)*b
}

// daysFromCivil returns days since the Unix epoch for a proleptic Gregorian
// date with month in [1, 12] and any day.
func daysFromCivil(year, month, day int64) int64 {
	if month <= 2 {
		year--
	}
	era := floorDiv(year, 400)
	yearOfEra := year - era*400
	m := month + 9
	if month > 2 {
		m = month - 3
	}
	dayOfYear := (153*m+2)/5 + day - 1
	dayOfEra := yearOfEra*365 + yearOfEra/4 - yearOfEra/100 + dayOfYear
	return era*146097 + dayOfEra - 719468
}
