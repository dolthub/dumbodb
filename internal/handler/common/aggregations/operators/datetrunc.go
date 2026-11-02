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
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// dateTruncOp is ExpressionDateTrunc: { $dateTrunc: { date, unit, binSize,
// timezone, startOfWeek } }.
type dateTruncOp struct {
	date, unit, binSize, startOfWeek any
	hasBinSize, hasStartOfWeek       bool
	timeZone                         *timeZoneArg

	// Constant operands parsed up front, as ExpressionDateTrunc::optimize does.
	parsedUnit        string
	parsedStartOfWeek int64
	parsedBinSize     int64
	alwaysNull        bool
}

func newDateTrunc(args ...any) (Operator, error) {
	op := new(dateTruncOp)
	var hasTimeZone bool
	var timeZone any
	spec := argObject{
		notObject: func(any) error {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateTruncNotObject,
				"$dateTrunc only supports an object as its argument", "$dateTrunc")
		},
		unknown: func(key string) error {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateTruncBadField,
				fmt.Sprintf("Unrecognized argument to $dateTrunc: %s. Expected arguments are date, unit, "+
					"and optionally, binSize, timezone, startOfWeek", key), "$dateTrunc")
		},
		fields: []argField{
			{name: "date", set: assign(&op.date, nil), missing: handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrDateTruncNoDate, "Missing 'date' parameter to $dateTrunc", "$dateTrunc")},
			{name: "unit", set: assign(&op.unit, nil), missing: handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrDateTruncNoUnit, "Missing 'unit' parameter to $dateTrunc", "$dateTrunc")},
			{name: "binSize", set: assign(&op.binSize, &op.hasBinSize)},
			{name: "timezone", set: assign(&timeZone, &hasTimeZone)},
			{name: "startOfWeek", set: assign(&op.startOfWeek, &op.hasStartOfWeek)},
		},
	}
	if err := spec.parse(args); err != nil {
		return nil, err
	}

	var err error
	allConstant := isConstantOperand(op.date) && isConstantOperand(op.unit) &&
		(!op.hasBinSize || isConstantOperand(op.binSize)) &&
		(!hasTimeZone || isConstantOperand(timeZone)) &&
		(!op.hasStartOfWeek || isConstantOperand(op.startOfWeek))
	if allConstant {
		// MongoDB folds the expression by evaluating it, in evaluation order.
		op.timeZone, err = newTimeZoneArgWith(timeZone, hasTimeZone, handlererrors.ErrTimeZoneNotString, false)
		return op, err
	}

	if isConstantOperand(op.unit) {
		if isNullish(op.unit) {
			op.alwaysNull = true
			return op, nil
		}
		if op.parsedUnit, err = parseTimeUnit(op.unit, "$dateTrunc"); err != nil {
			return nil, err
		}
	}

	if op.hasStartOfWeek && isConstantOperand(op.startOfWeek) {
		if isNullish(op.startOfWeek) {
			op.alwaysNull = true
			return op, nil
		}
		if op.parsedStartOfWeek, err = parseDayOfWeek(op.startOfWeek, "$dateTrunc"); err != nil {
			return nil, err
		}
	}

	if op.timeZone, err = newTimeZoneArg(timeZone, hasTimeZone); err != nil {
		return nil, timeZoneParsingFailed(err, "$dateTrunc")
	}
	if hasTimeZone && isConstantOperand(timeZone) && isNullish(timeZone) {
		op.alwaysNull = true
		return op, nil
	}

	if op.hasBinSize && isConstantOperand(op.binSize) {
		if isNullish(op.binSize) {
			op.alwaysNull = true
			return op, nil
		}
		if op.parsedBinSize, err = convertToBinSize(op.binSize); err != nil {
			return nil, err
		}
	}

	return op, nil
}

// Process follows ExpressionDateTrunc::evaluate.
func (op *dateTruncOp) Process(doc *types.Document) (any, error) {
	if op.alwaysNull {
		return types.Null, nil
	}

	dateValue, err := evalArgValue(op.date, doc)
	if err != nil || isNullish(dateValue) {
		return types.Null, err
	}

	binSize := int64(1)
	switch {
	case op.parsedBinSize != 0:
		binSize = op.parsedBinSize
	case op.hasBinSize:
		v, err := evalArgValue(op.binSize, doc)
		if err != nil || isNullish(v) {
			return types.Null, err
		}
		if binSize, err = convertToBinSize(v); err != nil {
			return nil, err
		}
	}

	unit := op.parsedUnit
	if unit == "" {
		v, err := evalArgValue(op.unit, doc)
		if err != nil || isNullish(v) {
			return types.Null, err
		}
		if unit, err = parseTimeUnit(v, "$dateTrunc"); err != nil {
			return nil, err
		}
	}

	startOfWeek := int64(7)
	if unit == "week" {
		switch {
		case op.parsedStartOfWeek != 0:
			startOfWeek = op.parsedStartOfWeek
		case op.hasStartOfWeek:
			v, err := evalArgValue(op.startOfWeek, doc)
			if err != nil || isNullish(v) {
				return types.Null, err
			}
			if startOfWeek, err = parseDayOfWeek(v, "$dateTrunc"); err != nil {
				return nil, err
			}
		}
	}

	tz, isNull, err := op.timeZone.evaluate(doc)
	if err != nil {
		return nil, timeZoneParsingFailed(err, "$dateTrunc")
	}
	if isNull {
		return types.Null, nil
	}

	ms, err := coerceToDateMillis(dateValue)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateTruncNotDate,
			"$dateTrunc requires 'date' to be a date, but got "+handlerparams.AliasFromType(dateValue), "$dateTrunc")
	}

	res, err := truncateDate(ms, unit, binSize, tz, startOfWeek)
	if err != nil {
		return nil, err
	}
	return time.UnixMilli(res).UTC(), nil
}

func timeZoneParsingFailed(err error, opName string) error {
	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		return err
	}
	return handlererrors.NewCommandErrorMsgWithArgument(cmdErr.Code(),
		opName+" parameter 'timezone' value parsing failed :: caused by :: "+cmdErr.Err().Error(), opName)
}

// convertToBinSize is ExpressionDateTrunc::convertToBinSize.
func convertToBinSize(v any) (int64, error) {
	binSize, ok := integralInt64(v)
	if !ok {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateTruncBinSizeNotInt,
			fmt.Sprintf("$dateTrunc requires 'binSize' to be a 64-bit integer, but got value '%s' of type %s",
				scalarString(v), handlerparams.AliasFromType(v)), "$dateTrunc")
	}
	if binSize <= 0 {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateTruncBinSizeNotPos,
			fmt.Sprintf("$dateTrunc requires 'binSize' to be greater than 0, but got value %d", binSize), "$dateTrunc")
	}
	return binSize, nil
}

// scalarString approximates Value::toString for the scalars error messages show.
func scalarString(v any) string {
	switch v := v.(type) {
	case string:
		return strconv.Quote(v)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// daysOfWeek maps names to MongoDB's DayOfWeek, Monday 1 through Sunday 7.
var daysOfWeek = map[string]int64{
	"monday": 1, "mon": 1, "tuesday": 2, "tue": 2, "wednesday": 3, "wed": 3,
	"thursday": 4, "thu": 4, "friday": 5, "fri": 5, "saturday": 6, "sat": 6,
	"sunday": 7, "sun": 7,
}

// parseDayOfWeek is MongoDB's parseDayOfWeek for an operator's 'startOfWeek'.
func parseDayOfWeek(v any, opName string) (int64, error) {
	name, ok := v.(string)
	if !ok {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrStartOfWeekNotString,
			opName+" requires 'startOfWeek' to be a string, but got "+handlerparams.AliasFromType(v), opName)
	}
	day, ok := daysOfWeek[strings.ToLower(name)]
	if !ok {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrStartOfWeekInvalid,
			opName+" parameter 'startOfWeek' value cannot be recognized as a day of a week: "+name, opName)
	}
	return day, nil
}

const millisPerDay = 86400000

// maxCalendarBinSize is the largest binSize dateAdd handles for day and
// longer units.
const maxCalendarBinSize = 100_000_000_000

// truncateDate is MongoDB's truncateDate with its default reference point,
// 2000-01-01T00:00 local time, moved forward to startOfWeek for weeks.
func truncateDate(ms int64, unit string, binSize int64, tz mongoTimeZone, startOfWeek int64) (int64, error) {
	const referenceUTCMillis = 946684800000
	refMillis := referenceUTCMillis - tz.utcOffset(referenceUTCMillis)*1000
	refDay := int64(1)
	if unit == "week" {
		const referenceDayOfWeek = 6 // 2000-01-01 is a Saturday.
		days := (7 - (referenceDayOfWeek-startOfWeek+7)%7) % 7
		refMillis += days * millisPerDay
		refDay += days
	}

	var unitMillis int64
	switch unit {
	case "millisecond":
		unitMillis = 1
	case "second":
		unitMillis = 1000
	case "minute":
		unitMillis = 60 * 1000
	case "hour":
		unitMillis = 60 * 60 * 1000
	}
	if unitMillis != 0 {
		hi, binMillis := bits.Mul64(uint64(binSize), uint64(unitMillis))
		if hi != 0 || binMillis > math.MaxInt64 {
			return 0, dateTruncOverflow(handlererrors.ErrDateTruncBinSizeOverflow)
		}
		return truncateDateMillis(ms, refMillis, int64(binMillis))
	}

	if binSize > maxCalendarBinSize {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateTruncBinSizeTooLarge,
			"dateTrunc unsupported binSize value", "$dateTrunc")
	}

	local := tz.localCivil(ms)
	ref := civilDate{year: 2000, month: 1, day: refDay}
	var distance int64
	switch unit {
	case "day":
		distance = civilDaysBetween(ref, local)
	case "week":
		distance = (civilDaysBetween(ref, local) + weekdayFrom(ref, startOfWeek) - weekdayFrom(local, startOfWeek)) / 7
	case "month":
		distance = local.month - ref.month + (local.year-ref.year)*12
	case "quarter":
		distance = (local.month-1)/3 - (ref.month-1)/3 + (local.year-ref.year)*4
	case "year":
		distance = local.year - ref.year
	}

	binLowerBound, overflow := subInt64(distance, binRemainder(distance, binSize))
	if overflow {
		return 0, dateTruncOverflow(handlererrors.ErrDateTruncBinBoundOverflow)
	}
	return mongoDateAdd(refMillis, unit, binLowerBound, tz)
}

// truncateDateMillis is MongoDB's truncateDateMillis.
func truncateDateMillis(ms, refMillis, binMillis int64) (int64, error) {
	shifted, overflow := subInt64(ms, refMillis)
	if overflow {
		return 0, dateTruncOverflow(handlererrors.ErrDateTruncShiftOverflow)
	}
	res, overflow := subInt64(ms, binRemainder(shifted, binMillis))
	if overflow {
		return 0, dateTruncOverflow(handlererrors.ErrDateTruncResultOverflow)
	}
	return res, nil
}

func dateTruncOverflow(code handlererrors.ErrorCode) error {
	return handlererrors.NewCommandErrorMsgWithArgument(code, "dateTrunc overflowed", "$dateTrunc")
}

// binRemainder is MongoDB's distanceToBinLowerBound: value's distance above
// the lower bound of its bin, for bins of binSize starting at 0.
func binRemainder(value, binSize int64) int64 {
	r := value % binSize
	if r < 0 {
		r += binSize
	}
	return r
}

func subInt64(a, b int64) (int64, bool) {
	d := a - b
	return d, (b > 0 && d > a) || (b < 0 && d < a)
}

// civilDaysBetween is MongoDB's dateDiffDay on calendar dates.
func civilDaysBetween(start, end civilDate) int64 {
	return timelibDayOfYear(end.year, end.month, end.day) - timelibDayOfYear(start.year, start.month, start.day) +
		daysBetweenYears(start.year, end.year)
}

// weekdayFrom is MongoDB's dayOfWeek: 0 for startOfWeek through 6.
func weekdayFrom(d civilDate, startOfWeek int64) int64 {
	isoDay := timelibDayOfWeek(d.year, d.month, d.day)
	if isoDay == 0 {
		isoDay = 7
	}
	return (isoDay - startOfWeek + 7) % 7
}

var _ Operator = (*dateTruncOp)(nil)
