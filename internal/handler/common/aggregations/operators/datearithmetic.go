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
	"time"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/timelib"
)

// timeUnits are MongoDB's TimeUnit names.
var timeUnits = map[string]struct{}{
	"year": {}, "quarter": {}, "month": {}, "week": {}, "day": {},
	"hour": {}, "minute": {}, "second": {}, "millisecond": {},
}

// parseTimeUnit is MongoDB's parseTimeUnit for an expression's 'unit'.
func parseTimeUnit(v any, opName string) (string, error) {
	unit, ok := v.(string)
	if !ok {
		return "", handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTimeUnitNotString,
			fmt.Sprintf("%s requires 'unit' to be a string, but got %s", opName, handlerparams.AliasFromType(v)), opName)
	}
	if _, ok := timeUnits[unit]; !ok {
		return "", handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			fmt.Sprintf("%s parameter 'unit' value parsing failed :: caused by :: unknown time unit value: %s", opName, unit), opName)
	}
	return unit, nil
}

// maxDateAddAmounts bounds dateAdd's amount per unit, as MongoDB's
// isDateAddAmountValid does, so the addition cannot overflow.
var maxDateAddAmounts = func() map[string]int64 {
	const millisPerDay = 86400000
	maxDays := int64(math.MaxUint64/millisPerDay + 1)
	maxYears := maxDays/365 + 1
	return map[string]int64{
		"year": maxYears, "quarter": maxYears * 4, "month": maxYears * 12,
		"week": maxDays / 7, "day": maxDays, "hour": maxDays * 24,
		"minute": maxDays * 24 * 60, "second": maxDays * 24 * 60 * 60,
	}
}()

// leapYearsSince counts leap years from MongoDB's kLeapYearReferencePoint.
func leapYearsSince(year int64) int64 {
	y := year + 1000000000
	return y/4 - y/100 + y/400
}

func daysBetweenYears(start, end int64) int64 {
	return leapYearsSince(end-1) - leapYearsSince(start-1) + (end-start)*365
}

// addMonths is MongoDB's addMonths.
func addMonths(year, month, amount int64) (int64, int64) {
	m, y := month+amount, year
	if m > 12 {
		y += m / 12
		m -= 12 * (m / 12)
	}
	if m <= 0 {
		years := -m/12 + 1
		m += 12 * years
		y -= years
	}
	return y, m
}

// mongoDateAdd is MongoDB's dateAdd: day and larger units are added in local
// time, smaller ones in UTC for a tz database zone, and month based units
// become days that keep the day of month within the target month.
func mongoDateAdd(ms int64, unit string, amount int64, tz mongoTimeZone) (int64, error) {
	if unit == "millisecond" {
		sum := ms + amount
		if (amount > 0 && sum < ms) || (amount < 0 && sum > ms) {
			return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDurationOverflow,
				fmt.Sprintf("Overflow while adding %dms to %dms", amount, ms), "$dateAdd")
		}
		return sum, nil
	}
	if limit := maxDateAddAmounts[unit]; amount <= -limit || amount >= limit {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateAddAmountInvalid,
			fmt.Sprintf("invalid dateAdd 'amount' parameter value: %d %s", amount, unit), "$dateAdd")
	}

	// MongoDB floors the seconds but truncates the milliseconds, so dates
	// before 1970 with a fraction of a second are a second early.
	seconds := floorDiv(ms, 1000)
	micros := ms % 1000 * 1000

	isUTC := tz.loc == nil && tz.offset == 0
	local := &timelib.Time{US: micros, HaveRelative: true}
	offset, isDST := int64(0), false
	switch {
	case tz.loc != nil:
		offset, _, isDST = tz.zoneInfo(seconds)
		local.ZoneType, local.Z = timelib.ZoneID, offset
	case !isUTC:
		offset = tz.offset
		local.ZoneType, local.Z = timelib.ZoneOffset, offset
	}
	c := civilFromMillis((seconds + offset) * 1000)
	local.Y, local.M, local.D, local.H, local.I, local.S = c.year, c.month, c.day, c.hour, c.minute, c.second

	if (unit == "year" || unit == "quarter" || unit == "month") && (local.D > 28 || local.Z != 0) {
		months := amount
		switch unit {
		case "year":
			months *= 12
		case "quarter":
			months *= 3
		}
		y, m := addMonths(local.Y, local.M, months)
		day := min(local.D, daysInMonth(y, m))
		amount = timelibDayOfYear(y, m, day) - timelibDayOfYear(local.Y, local.M, local.D) + daysBetweenYears(local.Y, y)
		unit = "day"
	}

	r := &local.Relative
	switch unit {
	case "year":
		r.Y = amount
	case "quarter":
		r.M = amount * 3
	case "month":
		r.M = amount
	case "week":
		r.D = amount * 7
	case "day":
		r.D = amount
	case "hour":
		r.H = amount
	case "minute":
		r.I = amount
	case "second":
		r.S = amount
	}

	t := local
	if !isUTC && tz.loc != nil && r.D == 0 && r.M == 0 && r.Y == 0 {
		u := civilFromMillis(seconds * 1000)
		t = &timelib.Time{
			Y: u.year, M: u.month, D: u.day, H: u.hour, I: u.minute, S: u.second, US: micros,
			HaveRelative: true, Relative: *r,
		}
	}
	sse, us := t.Resolve(func(l int64) int64 { return tz.utcSecondsWithDST(l, isDST) })

	if sse > math.MaxInt64/1000 || sse < math.MinInt64/1000 {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateAddOverflow, "dateAdd overflowed", "$dateAdd")
	}
	return sse*1000 + us/1000, nil
}

func daysInMonth(y, m int64) int64 {
	if timelibIsLeap(y) {
		return [13]int64{0, 31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[m]
	}
	return [13]int64{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[m]
}

// dateArithmeticOp represents { $dateAdd: { startDate, unit, amount, timezone } }
// and the same for $dateSubtract.
type dateArithmeticOp struct {
	opName                  string
	startDate, unit, amount any

	// parsedUnit is set when unit is a constant, which MongoDB parses up front.
	parsedUnit string
	alwaysNull bool
	timeZone   *timeZoneArg
}

// newDateArithmetic follows ExpressionDateArithmetics::parse.
func newDateArithmetic(opName string) func(args ...any) (Operator, error) {
	return func(args ...any) (Operator, error) {
		return parseDateArithmetic(opName, args...)
	}
}

func parseDateArithmetic(opName string, args ...any) (Operator, error) {
	op := &dateArithmeticOp{opName: opName}
	var hasTimeZone bool
	var timeZone any
	missing := handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateArithmeticNoField,
		opName+" requires startDate, unit, and amount to be present", opName)
	spec := argObject{
		notObject: func(any) error {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateArithmeticNotObject,
				opName+" expects an object as its argument", opName)
		},
		unknown: func(key string) error {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateArithmeticBadField,
				fmt.Sprintf("Unrecognized argument to %s: %s. Expected arguments are startDate, unit, amount, and optionally timezone.", opName, key), opName)
		},
		fields: []argField{
			{name: "startDate", set: assign(&op.startDate, nil), missing: missing},
			{name: "unit", set: assign(&op.unit, nil), missing: missing},
			{name: "amount", set: assign(&op.amount, nil), missing: missing},
			{name: "timezone", set: assign(&timeZone, &hasTimeZone)},
		},
	}
	if err := spec.parse(args); err != nil {
		return nil, err
	}

	// MongoDB folds an expression with only constant operands by evaluating
	// it, which checks startDate before unit and time zone; otherwise it
	// parses a constant unit and time zone up front.
	if isConstantOperand(op.startDate) && isConstantOperand(op.unit) && isConstantOperand(op.amount) &&
		(!hasTimeZone || isConstantOperand(timeZone)) {
		var err error
		op.timeZone, err = newTimeZoneArgWith(timeZone, hasTimeZone, handlererrors.ErrTimeZoneNotString, false)
		return op, err
	}

	if isConstantOperand(op.unit) {
		if isNullish(op.unit) {
			op.alwaysNull = true
			return op, nil
		}
		unit, err := parseTimeUnit(op.unit, opName)
		if err != nil {
			return nil, err
		}
		op.parsedUnit = unit
	}

	var err error
	if op.timeZone, err = newTimeZoneArg(timeZone, hasTimeZone); err != nil {
		return nil, err
	}
	if hasTimeZone && isConstantOperand(timeZone) && isNullish(timeZone) {
		op.alwaysNull = true
	}
	return op, nil
}

// isConstantOperand reports whether expr is a literal rather than a field
// path or an expression.
func isConstantOperand(expr any) bool {
	switch v := expr.(type) {
	case *types.Document, *types.Array:
		return false
	case string:
		return !strings.HasPrefix(v, "$")
	default:
		return true
	}
}

// Process follows ExpressionDateArithmetics::evaluate.
func (op *dateArithmeticOp) Process(doc *types.Document) (any, error) {
	opName := op.opName
	if op.alwaysNull {
		return types.Null, nil
	}

	start, err := evalArgValue(op.startDate, doc)
	if err != nil || isNullish(start) {
		return types.Null, err
	}

	unit := op.parsedUnit
	if unit == "" {
		v, err := evalArgValue(op.unit, doc)
		if err != nil || isNullish(v) {
			return types.Null, err
		}
		if unit, err = parseTimeUnit(v, opName); err != nil {
			return nil, err
		}
	}

	amountValue, err := evalArgValue(op.amount, doc)
	if err != nil || isNullish(amountValue) {
		return types.Null, err
	}

	tz, isNull, err := op.timeZone.evaluate(doc)
	if err != nil || isNull {
		return types.Null, err
	}

	ms, err := coerceToDateMillis(start)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateArithmeticNotDate,
			opName+" requires startDate to be convertible to a date", opName)
	}
	amount, ok := integralInt64(amountValue)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateArithmeticNotInteger,
			opName+" expects integer amount of time units", opName)
	}
	if opName == "$dateSubtract" {
		if amount == math.MinInt64 {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateSubtractMinAmount,
				fmt.Sprintf("invalid %s 'amount' parameter value: %d", opName, amount), opName)
		}
		amount = -amount
	}

	result, err := mongoDateAdd(ms, unit, amount, tz)
	if err != nil {
		return nil, err
	}
	return time.UnixMilli(result).UTC(), nil
}

var _ Operator = (*dateArithmeticOp)(nil)
