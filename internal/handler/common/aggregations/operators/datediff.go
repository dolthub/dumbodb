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

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// dateDiffOp is ExpressionDateDiff: { $dateDiff: { startDate, endDate, unit,
// timezone, startOfWeek } }.
type dateDiffOp struct {
	startDate, endDate, unit, startOfWeek any
	hasStartOfWeek                        bool
	timeZone                              *timeZoneArg

	// Constant operands parsed up front, as ExpressionDateDiff::optimize does.
	parsedUnit        string
	parsedStartOfWeek int64
	alwaysNull        bool
}

func newDateDiff(args ...any) (Operator, error) {
	spec, ok := args[0].(*types.Document)
	if len(args) != 1 || !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateDiffNotObject,
			"$dateDiff only supports an object as its argument", "$dateDiff")
	}

	op := new(dateDiffOp)
	var hasStart, hasEnd, hasUnit, hasTimeZone bool
	var timeZone any
	iter := spec.Iterator()
	defer iter.Close()
	for {
		key, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch key {
		case "startDate":
			op.startDate, hasStart = value, true
		case "endDate":
			op.endDate, hasEnd = value, true
		case "unit":
			op.unit, hasUnit = value, true
		case "timezone":
			timeZone, hasTimeZone = value, true
		case "startOfWeek":
			op.startOfWeek, op.hasStartOfWeek = value, true
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateDiffBadField,
				"Unrecognized argument to $dateDiff: "+key, "$dateDiff")
		}
	}
	switch {
	case !hasStart:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateDiffNoStartDate,
			"Missing 'startDate' parameter to $dateDiff", "$dateDiff")
	case !hasEnd:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateDiffNoEndDate,
			"Missing 'endDate' parameter to $dateDiff", "$dateDiff")
	case !hasUnit:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateDiffNoUnit,
			"Missing 'unit' parameter to $dateDiff", "$dateDiff")
	}

	var err error
	allConstant := isConstantOperand(op.startDate) && isConstantOperand(op.endDate) && isConstantOperand(op.unit) &&
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
		if op.parsedUnit, err = parseTimeUnit(op.unit, "$dateDiff"); err != nil {
			return nil, err
		}
	}

	if op.hasStartOfWeek && isConstantOperand(op.startOfWeek) {
		if isNullish(op.startOfWeek) {
			op.alwaysNull = true
			return op, nil
		}
		if op.parsedStartOfWeek, err = parseDayOfWeek(op.startOfWeek, "$dateDiff"); err != nil {
			return nil, err
		}
	}

	if op.timeZone, err = newTimeZoneArg(timeZone, hasTimeZone); err != nil {
		return nil, timeZoneParsingFailed(err, "$dateDiff")
	}
	if hasTimeZone && isConstantOperand(timeZone) && isNullish(timeZone) {
		op.alwaysNull = true
	}
	return op, nil
}

// Process follows ExpressionDateDiff::evaluate.
func (op *dateDiffOp) Process(doc *types.Document) (any, error) {
	if op.alwaysNull {
		return types.Null, nil
	}

	startValue, err := evalArgValue(op.startDate, doc)
	if err != nil || isNullish(startValue) {
		return types.Null, err
	}
	endValue, err := evalArgValue(op.endDate, doc)
	if err != nil || isNullish(endValue) {
		return types.Null, err
	}

	unit := op.parsedUnit
	if unit == "" {
		v, err := evalArgValue(op.unit, doc)
		if err != nil || isNullish(v) {
			return types.Null, err
		}
		if unit, err = parseTimeUnit(v, "$dateDiff"); err != nil {
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
			if startOfWeek, err = parseDayOfWeek(v, "$dateDiff"); err != nil {
				return nil, err
			}
		}
	}

	tz, isNull, err := op.timeZone.evaluate(doc)
	if err != nil {
		return nil, timeZoneParsingFailed(err, "$dateDiff")
	}
	if isNull {
		return types.Null, nil
	}

	start, err := dateDiffOperand(startValue, "startDate")
	if err != nil {
		return nil, err
	}
	end, err := dateDiffOperand(endValue, "endDate")
	if err != nil {
		return nil, err
	}
	return mongoDateDiff(start, end, unit, tz, startOfWeek)
}

func dateDiffOperand(v any, name string) (int64, error) {
	ms, err := coerceToDateMillis(v)
	if err != nil {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateDiffNotDate,
			fmt.Sprintf("$dateDiff requires '%s' to be a date, but got %s", name, handlerparams.AliasFromType(v)), "$dateDiff")
	}
	return ms, nil
}

// mongoDateDiff is MongoDB's dateDiff: calendar units count boundaries
// crossed on the local calendar, and hours, minutes and seconds count them on
// the local clock corrected by the change in UTC offset.
func mongoDateDiff(start, end int64, unit string, tz mongoTimeZone, startOfWeek int64) (int64, error) {
	if unit == "millisecond" {
		d, overflow := subInt64(end, start)
		if overflow {
			return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateDiffOverflow,
				"dateDiff overflowed", "$dateDiff")
		}
		return d, nil
	}

	s, startOffset := tz.localCivilAndOffset(start)
	e, endOffset := tz.localCivilAndOffset(end)
	days := civilDaysBetween(s, e)
	hours := e.hour - s.hour + days*24
	minutes := e.minute - s.minute + hours*60
	switch unit {
	case "year":
		return e.year - s.year, nil
	case "quarter":
		return (e.month-1)/3 - (s.month-1)/3 + (e.year-s.year)*4, nil
	case "month":
		return e.month - s.month + (e.year-s.year)*12, nil
	case "week":
		return (days + weekdayFrom(s, startOfWeek) - weekdayFrom(e, startOfWeek)) / 7, nil
	case "day":
		return days, nil
	case "hour":
		return hours + (startOffset-endOffset)/3600, nil
	case "minute":
		return minutes + (startOffset-endOffset)/60, nil
	default:
		return e.second - s.second + minutes*60 + startOffset - endOffset, nil
	}
}

// localCivilAndOffset is localCivil with the UTC offset in seconds it used.
func (tz mongoTimeZone) localCivilAndOffset(ms int64) (civilDate, int64) {
	offset := tz.offset
	if tz.loc != nil {
		offset, _, _ = tz.zoneInfo(floorDiv(ms, 1000))
	}
	return tz.localCivil(ms), offset
}

var _ Operator = (*dateDiffOp)(nil)
