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
	"strconv"
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// datePartOp represents { $year: <date> } and the other date parts, also
// written { $year: [<date>] } or { $year: { date: <date>, timezone: <tz> } }.
type datePartOp struct {
	name     string
	date     any
	timeZone *timeZoneArg
}

// newDatePartOp follows DateExpressionAcceptingTimeZone::parse; it receives
// its argument whole (see wholeArgumentOperators).
func newDatePartOp(name string) func(args ...any) (Operator, error) {
	return func(args ...any) (Operator, error) {
		op := &datePartOp{name: name, date: args[0]}
		var timeZone any
		var hasTimeZone bool

		switch arg := args[0].(type) {
		case *types.Array:
			if arg.Len() != 1 {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDatePartArrayLen,
					fmt.Sprintf("%s accepts exactly one argument if given an array, but was given %d", name, arg.Len()), name)
			}
			op.date = must.NotFail(arg.Get(0))
		case *types.Document:
			if arg.Len() > 0 && strings.HasPrefix(arg.Keys()[0], "$") {
				break
			}
			op.date = nil
			var hasDate bool
			iter := arg.Iterator()
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
				case "date":
					op.date, hasDate = value, true
				case "timezone":
					timeZone, hasTimeZone = value, true
				default:
					return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDatePartUnknownOption,
						fmt.Sprintf("unrecognized option to %s: \"%s\"", name, key), name)
				}
			}
			if !hasDate {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDatePartNoDate,
					fmt.Sprintf("missing 'date' argument to %s, provided: %s", name, types.FormatAnyValue(arg)), name)
			}
		}

		var err error
		op.timeZone, err = newTimeZoneArgWith(timeZone, hasTimeZone, handlererrors.ErrDatePartTimeZoneNotString,
			!isConstantOperand(op.date))
		if err != nil {
			return nil, err
		}
		return op, nil
	}
}

// Process follows DateExpressionAcceptingTimeZone::evaluate: a nullish date
// is null before the time zone is looked at.
func (op *datePartOp) Process(doc *types.Document) (any, error) {
	date, err := evalArgValue(op.date, doc)
	if err != nil || isNullish(date) {
		return types.Null, err
	}
	ms, err := coerceToDateMillis(date)
	if err != nil {
		return nil, err
	}

	tz, isNull, err := op.timeZone.evaluate(doc)
	if err != nil || isNull {
		return types.Null, err
	}

	c := tz.localCivil(ms)
	switch op.name {
	case "$year":
		return int32(c.year), nil
	case "$month":
		return int32(c.month), nil
	case "$dayOfMonth":
		return int32(c.day), nil
	case "$hour":
		return int32(c.hour), nil
	case "$minute":
		return int32(c.minute), nil
	case "$second":
		return int32(c.second), nil
	case "$millisecond":
		return int32(c.millis), nil
	case "$dayOfWeek":
		return int32(timelibDayOfWeek(c.year, c.month, c.day) + 1), nil
	case "$dayOfYear":
		return int32(timelibDayOfYear(c.year, c.month, c.day) + 1), nil
	case "$week":
		return int32(mongoWeek(c)), nil
	}

	isoYear, isoWeek, isoDay := timelibISODate(c.year, c.month, c.day)
	switch op.name {
	case "$isoWeek":
		return int32(isoWeek), nil
	case "$isoWeekYear":
		return isoYear, nil
	default:
		return int32(isoDay), nil
	}
}

// mongoWeek is TimeZone::week: weeks start on Sunday, and days before the
// year's first Sunday are week 0.
func mongoWeek(c civilDate) int64 {
	weekDay := timelibDayOfWeek(c.year, c.month, c.day) + 1
	yearDay := timelibDayOfYear(c.year, c.month, c.day) + 1
	return (yearDay - weekDay + 7) / 7
}

var _ Operator = (*datePartOp)(nil)

// dateToStringOp represents { $dateToString: { date, format, timezone, onNull } }.
type dateToStringOp struct {
	date, format, onNull any
	hasFormat, hasOnNull bool
	timeZone             *timeZoneArg
}

func newDateToString(args ...any) (Operator, error) {
	spec, ok := args[0].(*types.Document)
	if len(args) != 1 || !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateToStringNotObject,
			"$dateToString only supports an object as its argument", "$dateToString")
	}

	op := new(dateToStringOp)
	var hasDate, hasTimeZone bool
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
		case "date":
			op.date, hasDate = value, true
		case "format":
			op.format, op.hasFormat = value, true
		case "timezone":
			timeZone, hasTimeZone = value, true
		case "onNull":
			op.onNull, op.hasOnNull = value, true
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateToStringUnknownField,
				"Unrecognized argument to $dateToString: "+key, "$dateToString")
		}
	}
	if !hasDate {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateToStringNoDate,
			"Missing 'date' parameter to $dateToString", "$dateToString")
	}

	var err error
	if op.timeZone, err = newTimeZoneArg(timeZone, hasTimeZone); err != nil {
		return nil, err
	}
	if op.hasOnNull {
		if err = checkConstantOperand(op.onNull); err != nil {
			return nil, err
		}
	}
	return op, nil
}

// Process follows ExpressionDateToString::evaluate: the format and time zone
// are checked first, then a nullish date yields onNull.
func (op *dateToStringOp) Process(doc *types.Document) (any, error) {
	date, err := evalArgValue(op.date, doc)
	if err != nil {
		return nil, err
	}

	var format any
	if op.hasFormat {
		if format, err = evalArgValue(op.format, doc); err != nil {
			return nil, err
		}
		if !isNullish(format) {
			f, ok := format.(string)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateToStringFormatType,
					fmt.Sprintf("$dateToString requires that 'format' be a string, found: %s with value %s",
						handlerparams.AliasFromType(format), types.FormatAnyValue(format)), "$dateToString")
			}
			if err = validateDateFormat(f, "$dateToString", isDateToStringSpecifier); err != nil {
				return nil, err
			}
		}
	}

	tz, tzNull, err := op.timeZone.evaluate(doc)
	if err != nil {
		return nil, err
	}

	if isNullish(date) {
		if op.hasOnNull {
			return evalBranch(op.onNull, doc)
		}
		return types.Null, nil
	}
	if tzNull {
		return types.Null, nil
	}

	layout := "%Y-%m-%dT%H:%M:%S.%L"
	if op.hasFormat {
		if isNullish(format) {
			return types.Null, nil
		}
		layout = format.(string)
	} else if tz.loc == nil && tz.offset == 0 {
		layout += "Z"
	}

	ms, err := coerceToDateMillis(date)
	if err != nil {
		return nil, err
	}
	return tz.formatDate(layout, ms)
}

var _ Operator = (*dateToStringOp)(nil)

// isDateToStringSpecifier reports membership in MongoDB's
// kDateToStringFormatMap.
func isDateToStringSpecifier(c byte) bool {
	return strings.IndexByte("bBdGHjLmMSwuUVYzZ", c) >= 0
}

// validateDateFormat is MongoDB's format string check: every '%' is
// followed by '%' or a specifier the operator supports.
func validateDateFormat(format, opName string, isSpecifier func(byte) bool) error {
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		if i == len(format) {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFormatUnmatchedPercent,
				"Unmatched '%' at end of format string", opName)
		}
		if format[i] != '%' && !isSpecifier(format[i]) {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFormatInvalidSpecifier,
				fmt.Sprintf("Invalid format character '%%%c' in format string", format[i]), opName)
		}
	}
	return nil
}

var (
	fullMonthNames = [13]string{"", "January", "February", "March", "April", "May", "June",
		"July", "August", "September", "October", "November", "December"}
	shortMonthNames = [13]string{"", "Jan", "Feb", "Mar", "Apr", "May", "Jun",
		"Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
)

// formatDate is TimeZone::outputDateWithFormat for a validated format.
func (tz mongoTimeZone) formatDate(format string, ms int64) (any, error) {
	c := tz.localCivil(ms)
	var b strings.Builder
	padded := func(n int64, width int) error {
		if n < 0 || n > 9999 {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateComponentOutOfRange,
				fmt.Sprintf("Could not convert date to string: date component was outside the supported range of 0-9999: %d", n),
				"$dateToString")
		}
		s := strconv.FormatInt(n, 10)
		if len(s) < width {
			b.WriteString(strings.Repeat("0", width-len(s)))
		}
		b.WriteString(s)
		return nil
	}

	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			b.WriteByte(format[i])
			continue
		}
		i++
		var err error
		switch format[i] {
		case '%':
			b.WriteByte('%')
		case 'Y':
			err = padded(c.year, 4)
		case 'm':
			err = padded(c.month, 2)
		case 'd':
			err = padded(c.day, 2)
		case 'H':
			err = padded(c.hour, 2)
		case 'M':
			err = padded(c.minute, 2)
		case 'S':
			err = padded(c.second, 2)
		case 'L':
			err = padded(c.millis, 3)
		case 'B':
			b.WriteString(fullMonthNames[c.month])
		case 'b':
			b.WriteString(shortMonthNames[c.month])
		case 'j':
			err = padded(timelibDayOfYear(c.year, c.month, c.day)+1, 3)
		case 'w':
			err = padded(timelibDayOfWeek(c.year, c.month, c.day)+1, 1)
		case 'U':
			err = padded(mongoWeek(c), 2)
		case 'G', 'V', 'u':
			isoYear, isoWeek, isoDay := timelibISODate(c.year, c.month, c.day)
			switch format[i] {
			case 'G':
				err = padded(isoYear, 4)
			case 'V':
				err = padded(isoWeek, 2)
			default:
				err = padded(isoDay, 1)
			}
		case 'z':
			offset := tz.utcOffset(ms)
			if offset < 0 {
				b.WriteByte('-')
				offset = -offset
			} else {
				b.WriteByte('+')
			}
			if err = padded(offset/3600, 2); err == nil {
				err = padded(offset/60%60, 2)
			}
		case 'Z':
			b.WriteString(strconv.FormatInt(tz.utcOffset(ms)/60, 10))
		}
		if err != nil {
			return nil, err
		}
	}
	return b.String(), nil
}

// utcOffset is TimeZone::utcOffset in seconds; a tz database zone is looked
// up at the date's seconds, truncated toward zero.
func (tz mongoTimeZone) utcOffset(ms int64) int64 {
	if tz.loc == nil {
		return tz.offset
	}
	offset, _, _ := tz.zoneInfo(ms / 1000)
	return offset
}
