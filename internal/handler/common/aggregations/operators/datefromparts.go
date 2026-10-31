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
	"time"

	"github.com/cockroachdb/apd/v3"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// minDatePart and maxDatePart bound the month, day, hour, minute, isoWeek and
// isoDayOfWeek parts of $dateFromParts.
const (
	minDatePart = -32768
	maxDatePart = 32767
)

// dateFromPartsOp represents { $dateFromParts: { year: ..., ... } } or its
// ISO week form; a nil part takes its default.
type dateFromPartsOp struct {
	parts    map[string]any
	iso      bool
	timeZone *timeZoneArg
}

var dateFromPartsFields = []string{
	"year", "month", "day", "hour", "minute", "second", "millisecond",
	"isoWeekYear", "isoWeek", "isoDayOfWeek", "timezone",
}

func newDateFromParts(args ...any) (Operator, error) {
	parts := map[string]any{}
	spec := argObject{
		notObject: func(any) error {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateFromPartsNotObject,
				"$dateFromParts only supports an object as its argument", "$dateFromParts")
		},
		unknown: func(key string) error {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateFromPartsUnknownField,
				"Unrecognized argument to $dateFromParts: "+key, "$dateFromParts")
		},
	}
	for _, field := range dateFromPartsFields {
		spec.fields = append(spec.fields, argField{name: field, set: func(v any) error { parts[field] = v; return nil }})
	}
	if err := spec.parse(args); err != nil {
		return nil, err
	}

	has := func(field string) bool { _, ok := parts[field]; return ok }
	switch {
	case !has("year") && !has("isoWeekYear"):
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateFromPartsNoYear,
			"$dateFromParts requires either 'year' or 'isoWeekYear' to be present", "$dateFromParts")
	case has("year") && (has("isoWeekYear") || has("isoWeek") || has("isoDayOfWeek")):
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateFromPartsMixedNatural,
			"$dateFromParts does not allow mixing natural dates with ISO dates", "$dateFromParts")
	case has("isoWeekYear") && (has("year") || has("month") || has("day")):
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateFromPartsMixedISO,
			"$dateFromParts does not allow mixing ISO dates with natural dates", "$dateFromParts")
	}

	tz, err := newTimeZoneArg(parts["timezone"], has("timezone"))
	if err != nil {
		return nil, err
	}
	delete(parts, "timezone")

	return &dateFromPartsOp{parts: parts, iso: has("isoWeekYear"), timeZone: tz}, nil
}

// Process follows ExpressionDateFromParts::evaluate, including its order: a
// nullish part makes the result null without evaluating the parts after it.
func (op *dateFromPartsOp) Process(doc *types.Document) (any, error) {
	type partSpec struct {
		name    string
		def     int64
		bounded bool
	}
	eval := func(specs ...partSpec) ([]int64, bool, error) {
		values := make([]int64, len(specs))
		for i, s := range specs {
			v, isNull, err := op.part(doc, s.name, s.def, s.bounded)
			if err != nil || isNull {
				return nil, isNull, err
			}
			values[i] = v
		}
		return values, false, nil
	}

	clock, isNull, err := eval(
		partSpec{"hour", 0, true}, partSpec{"minute", 0, true},
		partSpec{"second", 0, false}, partSpec{"millisecond", 0, false},
	)
	if err != nil || isNull {
		return types.Null, err
	}

	tz, isNull, err := op.timeZone.evaluate(doc)
	if err != nil || isNull {
		return types.Null, err
	}

	var days int64
	if !op.iso {
		date, isNull, err := eval(partSpec{"year", 1970, false}, partSpec{"month", 1, true}, partSpec{"day", 1, true})
		if err != nil || isNull {
			return types.Null, err
		}
		year, month, day := date[0], date[1], date[2]
		if year < 1 || year > 9999 {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateFromPartsYearRange,
				fmt.Sprintf("'year' must evaluate to an integer in the range 1 to 9999, found %d", year), "$dateFromParts")
		}
		year += floorDiv(month-1, 12)
		days = daysFromCivil(year, floorMod(month-1, 12)+1, 1) + day - 1
	} else {
		date, isNull, err := eval(partSpec{"isoWeekYear", 1970, false}, partSpec{"isoWeek", 1, true}, partSpec{"isoDayOfWeek", 1, true})
		if err != nil || isNull {
			return types.Null, err
		}
		isoYear, isoWeek, isoDay := date[0], date[1], date[2]
		if isoYear < 1 || isoYear > 9999 {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateFromPartsISOYearRange,
				fmt.Sprintf("'isoWeekYear' must evaluate to an integer in the range 1 to 9999, found %d", isoYear), "$dateFromParts")
		}
		// timelib_daynr_from_weeknr: Monday of ISO week 1, then whole weeks and days.
		jan1 := daysFromCivil(isoYear, 1, 1)
		jan1Weekday := floorMod(jan1+4, 7)
		firstMonday := -jan1Weekday
		if jan1Weekday > 4 {
			firstMonday = 7 - jan1Weekday
		}
		days = jan1 + firstMonday + (isoWeek-1)*7 + isoDay
	}

	localMillis := ((days*86400+clock[0]*3600+clock[1]*60+clock[2])*1000 + clock[3])
	return time.UnixMilli(tz.utcMillis(localMillis)).UTC(), nil
}

// part evaluates one date part as evaluateNumberWithDefault and
// evaluateNumberWithDefaultAndBounds do.
func (op *dateFromPartsOp) part(doc *types.Document, name string, def int64, bounded bool) (int64, bool, error) {
	expr, ok := op.parts[name]
	if !ok {
		return def, false, nil
	}
	v, err := evalArgValue(expr, doc)
	if err != nil {
		return 0, false, err
	}
	if isNullish(v) {
		return 0, true, nil
	}
	n, ok := integralInt64(v)
	if !ok {
		return 0, false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDateFromPartsNotInteger,
			fmt.Sprintf("'%s' must evaluate to an integer, found %s with value %s",
				name, handlerparams.AliasFromType(v), types.FormatAnyValue(v)), "$dateFromParts")
	}
	if bounded && (n < minDatePart || n > maxDatePart) {
		return 0, false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDatePartOutOfRange,
			fmt.Sprintf("'%s' must evaluate to a value in the range [%d, %d]; value %d is not in range",
				name, minDatePart, maxDatePart, n), "$dateFromParts")
	}
	return n, false, nil
}

// integralInt64 returns v as an int64 if it is a number with an integral value
// that fits, as MongoDB's Value::integral64Bit accepts.
func integralInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n != math.Trunc(n) || n < math.MinInt64 || n >= math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case types.Decimal128:
		x := aggregations.DecimalToAPD(n)
		if x.Form != apd.Finite {
			return 0, false
		}
		i, err := x.Int64()
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

var _ Operator = (*dateFromPartsOp)(nil)
