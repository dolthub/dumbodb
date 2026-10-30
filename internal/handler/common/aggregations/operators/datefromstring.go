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
	"time"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/timelib"
)

// dateFromStringFormats is MongoDB's kDateFromStringFormatMap.
var dateFromStringFormats = map[byte]timelib.Format{
	'b': timelib.FormatTextualMonth,
	'B': timelib.FormatTextualMonth,
	'd': timelib.FormatDayTwoDigit,
	'G': timelib.FormatYearISO,
	'H': timelib.FormatHour24,
	'j': timelib.FormatDayOfYear,
	'L': timelib.FormatMillisecond,
	'm': timelib.FormatMonthTwoDigit,
	'M': timelib.FormatMinute,
	'S': timelib.FormatSecond,
	'u': timelib.FormatDayOfWeekISO,
	'V': timelib.FormatWeekOfYearISO,
	'Y': timelib.FormatYearFourDigit,
	'z': timelib.FormatTimezoneOffset,
	'Z': timelib.FormatTimezoneOffsetMinutes,
}

// dateFromStringOp represents { $dateFromString: { dateString: ..., format: ...,
// timezone: ..., onNull: ..., onError: ... } }.
type dateFromStringOp struct {
	dateString any
	format     any
	onNull     any
	onError    any
	hasFormat  bool
	hasOnNull  bool
	hasOnError bool
	timeZone   *timeZoneArg
}

func newDateFromString(args ...any) (Operator, error) {
	spec, ok := args[0].(*types.Document)
	if len(args) != 1 || !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateFromStringNotObject,
			"$dateFromString only supports an object as an argument", "$dateFromString")
	}

	op := new(dateFromStringOp)
	var hasDateString, hasTimeZone bool
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
		case "dateString":
			op.dateString, hasDateString = value, true
		case "format":
			op.format, op.hasFormat = value, true
		case "timezone":
			timeZone, hasTimeZone = value, true
		case "onNull":
			op.onNull, op.hasOnNull = value, true
		case "onError":
			op.onError, op.hasOnError = value, true
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateFromStringBadField,
				"Unrecognized argument to $dateFromString: "+key, "$dateFromString")
		}
	}
	if !hasDateString {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateFromStringNoInput,
			"Missing 'dateString' parameter to $dateFromString", "$dateFromString")
	}

	var err error
	if op.timeZone, err = newTimeZoneArg(timeZone, hasTimeZone); err != nil {
		return nil, err
	}
	for _, operand := range []struct {
		expr    any
		present bool
	}{{op.onNull, op.hasOnNull}, {op.onError, op.hasOnError}} {
		if operand.present {
			if err = checkConstantOperand(operand.expr); err != nil {
				return nil, err
			}
		}
	}
	return op, nil
}

// Process follows ExpressionDateFromString::evaluate: the format and time
// zone are checked first, a nullish dateString yields onNull, and only
// conversion failures are replaced by onError.
func (op *dateFromStringOp) Process(doc *types.Document) (any, error) {
	dateString, err := evalArgValue(op.dateString, doc)
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
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateFormatNotString,
					fmt.Sprintf("$dateFromString requires that 'format' be a string, found: %s with value %s",
						handlerparams.AliasFromType(format), types.FormatAnyValue(format)), "$dateFromString")
			}
			if err = validateDateFormat(f, "$dateFromString", isDateFromStringSpecifier); err != nil {
				return nil, err
			}
		}
	}

	tz, tzNull, err := op.timeZone.evaluate(doc)
	if err != nil {
		return nil, err
	}

	if isNullish(dateString) {
		if op.hasOnNull {
			return evalBranch(op.onNull, doc)
		}
		return types.Null, nil
	}

	result, err := op.convert(dateString, format, tz, tzNull)
	var cmdErr *handlererrors.CommandError
	if err != nil && op.hasOnError && errors.As(err, &cmdErr) && cmdErr.Code() == handlererrors.ErrConversionFailure {
		return evalBranch(op.onError, doc)
	}
	return result, err
}

// maxDateStringLength bounds parsing cost. mongod has no limit, but a
// string past it can only be valid by repeating tokens or padding, and the
// parser costs about 3us per byte (workspace-8tj.11, deliberate deviation).
const maxDateStringLength = 4096

func (op *dateFromStringOp) convert(dateString, format any, tz mongoTimeZone, tzNull bool) (any, error) {
	s, ok := dateString.(string)
	if !ok {
		return nil, conversionFailure(fmt.Sprintf("$dateFromString requires that 'dateString' be a string, found: %s with value %s",
			handlerparams.AliasFromType(dateString), types.FormatAnyValue(dateString)))
	}
	if tzNull {
		return types.Null, nil
	}
	if len(s) > maxDateStringLength {
		return nil, conversionFailure(fmt.Sprintf("Error parsing date string: longer than %d bytes", maxDateStringLength))
	}

	var t *timelib.Time
	if op.hasFormat {
		if isNullish(format) {
			return types.Null, nil
		}
		t, ok = timelib.ParseFromFormat(format.(string), s, '%', dateFromStringFormats)
		if !ok {
			return nil, conversionFailure(fmt.Sprintf("Error parsing date string '%s'", s))
		}
		return tz.fromParsedTime(t, s)
	}
	return parseDateString(s, tz)
}

// parseDateString is TimeZoneDatabase::fromString without a format.
func parseDateString(s string, tz mongoTimeZone) (any, error) {
	if len(s) > maxDateStringLength {
		return nil, conversionFailure(fmt.Sprintf("Error parsing date string: longer than %d bytes", maxDateStringLength))
	}
	t, ok := timelib.StrToTime(s)
	if !ok {
		return nil, conversionFailure(fmt.Sprintf("Error parsing date string '%s'", s))
	}
	return tz.fromParsedTime(t, s)
}

// fromParsedTime is the rest of TimeZoneDatabase::fromString: a missing time
// is midnight, any other missing part is an error, a zone in the string may
// only be combined with UTC, and the result is in the time zone.
func (tz mongoTimeZone) fromParsedTime(t *timelib.Time, s string) (any, error) {
	if t.H == timelib.Unset && t.I == timelib.Unset && t.S == timelib.Unset {
		t.H, t.I, t.S, t.US = 0, 0, 0, 0
	}
	for _, part := range []int64{t.Y, t.M, t.D, t.H, t.I, t.S} {
		if part == timelib.Unset {
			return nil, conversionFailure(fmt.Sprintf(
				"an incomplete date/time string has been found, with elements missing: \"%s\"", s))
		}
	}

	isUTC := tz.loc == nil && tz.offset == 0
	if !isUTC && t.ZoneType != timelib.ZoneNone {
		return nil, conversionFailure("you cannot pass in a date/time string with time zone information together with a timezone argument")
	}
	switch {
	case tz.loc != nil:
		t.ZoneType = timelib.ZoneID
	case tz.offset != 0:
		t.ZoneType, t.Z, t.Dst = timelib.ZoneOffset, tz.offset, 0
	}

	sse, us := t.Resolve(tz.utcSeconds)
	return time.UnixMilli((sse*1_000_000 + us) / 1000).UTC(), nil
}

func conversionFailure(msg string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrConversionFailure, msg, "$dateFromString")
}

func isDateFromStringSpecifier(c byte) bool {
	_, ok := dateFromStringFormats[c]
	return ok
}

var _ Operator = (*dateFromStringOp)(nil)
