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

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// dateToPartsOp represents { $dateToParts: { date: ..., timezone: ..., iso8601: ... } }.
type dateToPartsOp struct {
	date     any
	iso8601  any
	hasISO   bool
	timeZone *timeZoneArg
}

func newDateToParts(args ...any) (Operator, error) {
	op := new(dateToPartsOp)
	var hasTimeZone bool
	var timeZone any
	spec := argObject{
		notObject: func(any) error {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateToPartsNotObject,
				"$dateToParts only supports an object as its argument", "$dateToParts")
		},
		unknown: func(key string) error {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateToPartsUnknownField,
				"Unrecognized argument to $dateToParts: "+key, "$dateToParts")
		},
		fields: []argField{
			{name: "date", set: assign(&op.date, nil), missing: handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrDateToPartsNoDate, "Missing 'date' parameter to $dateToParts", "$dateToParts")},
			{name: "timezone", set: assign(&timeZone, &hasTimeZone)},
			{name: "iso8601", set: assign(&op.iso8601, &op.hasISO)},
		},
	}
	if err := spec.parse(args); err != nil {
		return nil, err
	}

	var err error
	if op.timeZone, err = newTimeZoneArg(timeZone, hasTimeZone); err != nil {
		return nil, err
	}
	return op, nil
}

// Process follows ExpressionDateToParts::evaluate: the time zone and iso8601
// flag are checked before a nullish date makes the result null.
func (op *dateToPartsOp) Process(doc *types.Document) (any, error) {
	date, err := evalArgValue(op.date, doc)
	if err != nil {
		return nil, err
	}

	tz, isNull, err := op.timeZone.evaluate(doc)
	if err != nil || isNull {
		return types.Null, err
	}

	iso := false
	if op.hasISO {
		v, err := evalArgValue(op.iso8601, doc)
		if err != nil {
			return nil, err
		}
		if isNullish(v) {
			return types.Null, nil
		}
		b, ok := v.(bool)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateToPartsISONotBool,
				fmt.Sprintf("iso8601 must evaluate to a bool, found %s", handlerparams.AliasFromType(v)), "$dateToParts")
		}
		iso = b
	}

	if isNullish(date) {
		return types.Null, nil
	}
	ms, err := coerceToDateMillis(date)
	if err != nil {
		return nil, err
	}

	c := tz.localCivil(ms)
	if iso {
		isoYear, isoWeek, isoDay := timelibISODate(c.year, c.month, c.day)
		return must.NotFail(types.NewDocument(
			"isoWeekYear", int32(isoYear), "isoWeek", int32(isoWeek), "isoDayOfWeek", int32(isoDay),
			"hour", int32(c.hour), "minute", int32(c.minute), "second", int32(c.second), "millisecond", int32(c.millis),
		)), nil
	}
	return must.NotFail(types.NewDocument(
		"year", int32(c.year), "month", int32(c.month), "day", int32(c.day),
		"hour", int32(c.hour), "minute", int32(c.minute), "second", int32(c.second), "millisecond", int32(c.millis),
	)), nil
}

var _ Operator = (*dateToPartsOp)(nil)
